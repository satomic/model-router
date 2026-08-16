"""GitHub OAuth sign-in, session cookies and API key authentication.

Modelled on OctoFinance's routers/auth.py: cookie sessions + an admin allow-list +
role separation.
The callback URL and the cookie Secure flag follow X-Forwarded-Proto /
X-Forwarded-Host so this works behind a reverse proxy (uvicorn needs --proxy-headers).
"""
import logging
import secrets
from urllib.parse import urlencode

import httpx
from fastapi import HTTPException, Request, Response

from . import localadmin
from .authstore import AuthStore

logger = logging.getLogger("mr")

SESSION_COOKIE = "mr_session"
STATE_COOKIE = "mr_oauth_state"
CALLBACK_PATH = "/v1/auth/github/callback"

GITHUB_AUTHORIZE = "https://github.com/login/oauth/authorize"
GITHUB_TOKEN = "https://github.com/login/oauth/access_token"
GITHUB_USER = "https://api.github.com/user"

_LOOPBACK = {"127.0.0.1", "::1", "localhost"}


def is_loopback(request: Request) -> bool:
    host = request.client.host if request.client else ""
    return host in _LOOPBACK


def _forwarded_scheme(request: Request) -> str:
    proto = request.headers.get("x-forwarded-proto")
    if proto:
        return proto.split(",")[0].strip()
    return request.url.scheme


def _origin(request: Request) -> str:
    host = request.headers.get("x-forwarded-host") or request.headers.get("host")
    if not host:
        host = request.url.netloc
    return f"{_forwarded_scheme(request)}://{host.split(',')[0].strip()}"


def callback_url(request: Request, configured: str = "") -> str:
    return configured.strip() or f"{_origin(request)}{CALLBACK_PATH}"


def _is_secure(request: Request) -> bool:
    return _forwarded_scheme(request) == "https"


def set_session_cookie(response: Response, request: Request, sid: str, ttl: int) -> None:
    response.set_cookie(
        SESSION_COOKIE, sid,
        max_age=ttl, httponly=True, samesite="lax",
        secure=_is_secure(request), path="/",
    )


def clear_session_cookie(response: Response, request: Request) -> None:
    response.delete_cookie(
        SESSION_COOKIE, path="/", httponly=True, samesite="lax",
        secure=_is_secure(request),
    )


# -- OAuth flow ---------------------------------------------------------------
def build_authorize_url(request: Request, cfg) -> tuple[str, str]:
    """Return (authorize URL, one-time state). The caller must store the state in a
    cookie to guard against CSRF."""
    state = secrets.token_urlsafe(24)
    redirect_uri = callback_url(request, cfg.gh_callback_url)
    if cfg.gh_callback_url and not cfg.gh_callback_url.startswith(_origin(request)):
        logger.warning(
            "configured callback_url (%s) does not match the current origin (%s); "
            "the session cookie may not take effect after sign-in",
            cfg.gh_callback_url, _origin(request),
        )
    query = urlencode({
        "client_id": cfg.gh_client_id,
        "redirect_uri": redirect_uri,
        "scope": "read:user",
        "state": state,
    })
    return f"{GITHUB_AUTHORIZE}?{query}", state


def set_state_cookie(response: Response, request: Request, state: str) -> None:
    response.set_cookie(
        STATE_COOKIE, state,
        max_age=600, httponly=True, samesite="lax",
        secure=_is_secure(request), path="/",
    )


async def exchange_code_for_user(request: Request, cfg, code: str) -> dict:
    """Exchange the code for a token, fetch the user, and return the dict to store in
    the session."""
    async with httpx.AsyncClient(timeout=15.0) as client:
        token_resp = await client.post(
            GITHUB_TOKEN,
            headers={"Accept": "application/json"},
            data={
                "client_id": cfg.gh_client_id,
                "client_secret": cfg.gh_client_secret,
                "code": code,
                "redirect_uri": callback_url(request, cfg.gh_callback_url),
            },
        )
        token_resp.raise_for_status()
        payload = token_resp.json()
        access_token = payload.get("access_token")
        if not access_token:
            raise HTTPException(
                status_code=400,
                detail=f"GitHub returned no access_token: {payload.get('error_description') or payload}",
            )
        user_resp = await client.get(
            GITHUB_USER,
            headers={
                "Authorization": f"Bearer {access_token}",
                "Accept": "application/vnd.github+json",
            },
        )
        user_resp.raise_for_status()
        gh_user = user_resp.json()

    login = gh_user.get("login")
    if not login:
        raise HTTPException(status_code=400, detail="GitHub user info has no login")
    is_admin = cfg.is_admin_login(login)
    if not is_admin and not cfg.allow_any_github_user:
        raise HTTPException(
            status_code=403,
            detail="only administrators may sign in right now (auth.allow_any_github_user = false)",
        )
    return {
        "login": login,
        "name": gh_user.get("name") or login,
        "avatar_url": gh_user.get("avatar_url"),
        "is_admin": is_admin,
    }


# -- Dependencies: session and roles ------------------------------------------
# Reachable while the local administrator's default password is still in force.
# Everything else is refused: a super-admin account on a documented credential must not be
# usable until it has been changed. The refusal carries a machine-readable detail so the
# console can route to the change-password form instead of showing a generic error.
PASSWORD_CHANGE_REQUIRED = "password_change_required"
_PASSWORD_CHANGE_EXEMPT = (
    "/v1/auth/status",
    "/v1/auth/logout",
    "/v1/auth/local/password",
)


def current_user(request: Request, store: AuthStore, cfg) -> dict | None:
    session = store.get_session(request.cookies.get(SESSION_COOKIE))
    if session is None:
        return None
    if session.get("local_admin"):
        # A local administrator's authority comes from auth.local_admin, not from
        # admin_logins -- resolving it through is_admin_login would silently demote the
        # account one request after sign-in. Recomputed for the same reason as the GitHub
        # branch below: renaming or disabling the account in config.yaml must downgrade
        # sessions already issued to it.
        session["is_admin"] = cfg.is_local_admin_login(session.get("login", ""))
        session["must_change_password"] = localadmin.must_change(cfg)
    else:
        # The admin list may change while a session is still valid, so recompute from
        # the configuration on every request
        session["is_admin"] = cfg.is_admin_login(session.get("login", ""))
        session["must_change_password"] = False
    return session


def require_user(request: Request, store: AuthStore, cfg) -> dict:
    user = current_user(request, store, cfg)
    if user is None:
        raise HTTPException(status_code=401, detail="not signed in")
    if user.get("must_change_password") and request.url.path not in _PASSWORD_CHANGE_EXEMPT:
        raise HTTPException(status_code=403, detail=PASSWORD_CHANGE_REQUIRED)
    return user


def require_admin(request: Request, store: AuthStore, cfg) -> dict:
    user = require_user(request, store, cfg)
    if not user.get("is_admin"):
        raise HTTPException(status_code=403, detail="administrator privileges required")
    return user


# -- Dependencies: API key ----------------------------------------------------
def extract_api_key(request: Request) -> str:
    """Accept both Authorization: Bearer <key> and api-key: <key>."""
    auth = request.headers.get("authorization") or ""
    if auth.lower().startswith("bearer "):
        return auth[7:].strip()
    return (request.headers.get("api-key") or request.headers.get("x-api-key") or "").strip()


def require_api_key(request: Request, store: AuthStore) -> dict:
    plaintext = extract_api_key(request)
    if not plaintext:
        raise HTTPException(
            status_code=401,
            detail="missing API key: send Authorization: Bearer <key> "
                   "(create one on the \"API keys\" page of the console)",
        )
    record = store.lookup_api_key(plaintext)
    if record is None:
        raise HTTPException(status_code=401, detail="API key is invalid or has been revoked")
    store.touch_api_key(record["id"])
    return record
