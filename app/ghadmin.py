"""API client for GitHub Enterprise / Organization access control.

Purpose: by default, once GitHub OAuth is configured, **any** GitHub account can sign
in to this service. This module uses an enterprise admin PAT to ask GitHub which
Enterprise / Enterprise Team / Organization a signed-in user actually belongs to, and
decides from that whether they may create an API key (and therefore use BYOK).
Decisions always rest on GitHub's live data; the client is never trusted.

Measured capability differences per endpoint (they dictate the shape of the code below
-- do not "simplify" it on the assumption that they behave alike):

| Goal                       | Route                                                    | Note |
|----------------------------|----------------------------------------------------------|------|
| List enterprises for a PAT | GraphQL `viewer.enterprises`                             | no REST equivalent |
| Orgs of an enterprise      | GraphQL `enterprise(slug:).organizations`                 | paginated |
| Enterprise teams           | REST `/enterprises/{slug}/teams`                         | **404s on some enterprises** |
| Enterprise membership      | filtered GraphQL -> REST `consumed-licenses`             | **both unusable on large enterprises** |
| Org membership             | REST `/orgs/{org}/members/{user}`                        | 204 / 404 |
| Enterprise team membership | REST `/enterprises/{slug}/teams/{id}/memberships/{user}`  | 200 / 404 |

Key constraint: enterprise-level membership is **not** determinable on every
enterprise. On very large ones GraphQL reports RESOURCE_LIMITS_EXCEEDED (even with a
login filter) and REST consumed-licenses returns 404. So `check_enterprise_member` is
**tri-state** (yes / no / cannot tell), "cannot tell" is handled fail-closed, and
authorization relies only on the two reliable routes: orgs and teams. That is why the
enterprise master switch means "any allowed org/team within the enterprise" rather
than "any enterprise member" -- the latter cannot be verified reliably everywhere.
"""
import asyncio
import logging
import time

import httpx

logger = logging.getLogger("fmr")

API = "https://api.github.com"
GRAPHQL = f"{API}/graphql"
_ACCEPT = "application/vnd.github+json"
_TIMEOUT = 20.0

# Same wording for every 401, so it is stated once here.
_ERR_TOKEN_401 = "token is invalid or expired (GitHub 401)"

# Cache lifetime for discovery results (enterprise/org/team listings): the admin
# configuration page reads them repeatedly and these structures rarely change, so
# there is no need to hit GitHub every time.
_DISCOVERY_TTL = 300.0
# Membership results are cached for less time: a user removed from an org should lose
# the ability to create keys reasonably quickly.
_MEMBERSHIP_TTL = 60.0

# How many orgs to enumerate per enterprise at most: enterprises like avocado-corp have
# a huge number of orgs, and paging through all of them saturates GraphQL's secondary
# rate limit. Truncation is reported explicitly to the caller.
_MAX_ORGS = 200
_ORG_PAGE = 100

# Ceiling on a single member list. An org with more members than this is not listed
# exhaustively, and the caller is told so via `truncated` -- see _rest_paged.
_MAX_MEMBERS = 5000
_MEMBER_PAGE = 100


class GitHubAdminError(Exception):
    """A GitHub call failed and the reason needs to be shown to the administrator."""


class _TTLCache:
    def __init__(self) -> None:
        self._data: dict[tuple, tuple[float, object]] = {}
        self._lock = asyncio.Lock()

    async def get(self, key: tuple, ttl: float):
        async with self._lock:
            hit = self._data.get(key)
            if hit is None or (time.monotonic() - hit[0]) > ttl:
                return None
            return hit[1]

    async def put(self, key: tuple, value: object) -> None:
        async with self._lock:
            self._data[key] = (time.monotonic(), value)

    async def clear(self) -> None:
        async with self._lock:
            self._data.clear()


_cache = _TTLCache()


async def invalidate_cache() -> None:
    """Call after the token or the policy changes, so stale identities/structures are
    not reused."""
    await _cache.clear()


def _headers(token: str) -> dict:
    return {"Authorization": f"Bearer {token}", "Accept": _ACCEPT}


async def _rest(
    token: str, path: str, *, allow_404: bool = False
) -> tuple[int, object]:
    """Return (status, body). body is None when empty or not JSON.

    Membership endpoints express their answer in the **status code** with an empty body:
    204 = yes, 404 = no, 302 = the caller is not a member of that org (GitHub uses a
    redirect to mean "not allowed to look"). So this must neither follow redirects
    (which would turn the 302 into some other status) nor parse JSON unconditionally.
    """
    async with httpx.AsyncClient(timeout=_TIMEOUT, follow_redirects=False) as client:
        resp = await client.get(f"{API}{path}", headers=_headers(token))
    if resp.status_code in (204, 302, 304, 404):
        if resp.status_code == 404 and not allow_404:
            raise GitHubAdminError(f"GitHub returned 404: {path}")
        return resp.status_code, None
    if resp.status_code == 401:
        raise GitHubAdminError(_ERR_TOKEN_401)
    if resp.status_code == 403:
        raise GitHubAdminError(
            f"token lacks permission or hit a rate limit (GitHub 403): {path}"
        )
    if resp.status_code >= 400:
        raise GitHubAdminError(f"GitHub {resp.status_code}: {path}")
    if not resp.content:
        return resp.status_code, None
    try:
        return resp.status_code, resp.json()
    except ValueError:
        # A non-JSON body (very rare) must not take down the whole decision chain
        logger.warning("GitHub returned a non-JSON response path=%s", path)
        return resp.status_code, None


async def _rest_paged(
    token: str, path: str, *, cap: int = _MAX_MEMBERS
) -> tuple[list, bool, str | None]:
    """Walk a paginated REST collection. Returns (items, truncated, error).

    A sibling of _rest() rather than a wrapper: paging needs the `Link` response header,
    which _rest deliberately discards. Every other REST call in this module passes a bare
    per_page=100 and follows nothing -- harmless for a team listing that fits on one page,
    silently wrong for a member list, where "page 1 of 12" read as the whole thing would
    turn members into non-members.

    Errors are returned rather than raised: a partially-read list must be reported as
    incomplete (so the caller does not treat it as authoritative), not thrown away.
    """
    items: list = []
    truncated = False
    sep = "&" if "?" in path else "?"
    url = f"{API}{path}{sep}per_page={_MEMBER_PAGE}"
    try:
        async with httpx.AsyncClient(timeout=_TIMEOUT, follow_redirects=False) as client:
            while url:
                resp = await client.get(url, headers=_headers(token))
                if resp.status_code == 401:
                    return items, True, _ERR_TOKEN_401
                if resp.status_code == 403:
                    return items, True, (
                        f"token lacks permission or hit a rate limit (GitHub 403): {path}"
                    )
                if resp.status_code >= 400 or resp.status_code in (302, 304):
                    # 302 here means the same thing as on the membership endpoints: the
                    # token is not allowed to look. That is not an empty list.
                    return items, True, f"GitHub {resp.status_code}: {path}"
                try:
                    body = resp.json()
                except ValueError:
                    return items, True, f"GitHub returned a non-JSON response: {path}"
                if not isinstance(body, list):
                    return items, True, f"GitHub returned an unexpected payload: {path}"
                items.extend(body)
                if len(items) >= cap:
                    del items[cap:]
                    truncated = True
                    break
                nxt = (resp.links or {}).get("next") or {}
                url = nxt.get("url") or ""
    except httpx.HTTPError as e:
        return items, True, f"GitHub request failed: {e}"
    return items, truncated, None


async def list_org_members(token: str, org: str) -> dict:
    """Every member login of an org. Returns {logins, truncated, error}.

    Logins are lower-cased here: GitHub logins are case-insensitive, and the whole point
    of caching the list is to answer membership as a set lookup, which needs one casing.
    """
    items, truncated, error = await _rest_paged(token, f"/orgs/{org}/members")
    logins = sorted({
        str(u["login"]).lower() for u in items if isinstance(u, dict) and u.get("login")
    })
    return {"logins": logins, "truncated": truncated, "error": error}


async def list_enterprise_team_members(
    token: str, slug: str, team_id: int | str
) -> dict:
    """Every member login of an enterprise team. Returns {logins, truncated, error}.

    The numeric team id is required, exactly as in check_enterprise_team_member -- the
    `ent:`-prefixed slug 404s here too.
    """
    items, truncated, error = await _rest_paged(
        token, f"/enterprises/{slug}/teams/{team_id}/memberships"
    )
    logins: set[str] = set()
    for entry in items:
        if not isinstance(entry, dict):
            continue
        # This endpoint's shape has varied: some responses nest the account under `user`,
        # others carry the login at the top level. Accept both rather than silently
        # producing an empty list against the variant we did not expect.
        login = entry.get("login") or ((entry.get("user") or {}) if isinstance(
            entry.get("user"), dict) else {}).get("login")
        if login:
            logins.add(str(login).lower())
    return {"logins": sorted(logins), "truncated": truncated, "error": error}


async def _graphql(token: str, query: str, variables: dict | None = None) -> dict:
    """Run a GraphQL query. Returns the payload; callers handle null fields themselves.

    GitHub's GraphQL returns data *and* errors together on partial success (e.g. one
    field exceeded resource limits), so this must not raise on sight of errors -- the
    errors are handed to the caller alongside the data.
    """
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        resp = await client.post(
            GRAPHQL,
            headers=_headers(token),
            json={"query": query, "variables": variables or {}},
        )
    if resp.status_code == 401:
        raise GitHubAdminError(_ERR_TOKEN_401)
    if resp.status_code >= 400:
        raise GitHubAdminError(f"GitHub GraphQL {resp.status_code}: {resp.text[:200]}")
    payload = resp.json()
    if payload.get("data") is None and payload.get("errors"):
        first = payload["errors"][0]
        raise GitHubAdminError(f"GraphQL error: {first.get('message')}")
    return payload


# -- The token itself ---------------------------------------------------------
async def verify_token(token: str) -> dict:
    """Validate the token and return its owner and scopes, so the configuration page
    can confirm whose token this is."""
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        resp = await client.get(f"{API}/user", headers=_headers(token))
    if resp.status_code == 401:
        raise GitHubAdminError(_ERR_TOKEN_401)
    if resp.status_code >= 400:
        raise GitHubAdminError(f"GitHub {resp.status_code}: /user")
    user = resp.json()
    # A classic PAT reports its scopes in a response header; fine-grained tokens do not
    scopes = [
        s.strip() for s in (resp.headers.get("x-oauth-scopes") or "").split(",")
        if s.strip()
    ]
    return {
        "login": user.get("login"),
        "name": user.get("name") or user.get("login"),
        "avatar_url": user.get("avatar_url"),
        "scopes": scopes,
        # Listing enterprises needs admin:enterprise; when it is missing the frontend
        # says so explicitly instead of showing an empty list
        "has_enterprise_scope": (not scopes) or any(
            s in scopes for s in ("admin:enterprise", "manage_billing:enterprise",
                                  "read:enterprise")
        ),
    }


# -- Discovery: enterprises / orgs / teams ------------------------------------
async def list_enterprises(token: str) -> list[dict]:
    """List the enterprises visible to the token. REST cannot do this; GraphQL only."""
    cached = await _cache.get(("ents", token), _DISCOVERY_TTL)
    if cached is not None:
        return cached  # type: ignore[return-value]

    payload = await _graphql(
        token,
        "query { viewer { enterprises(first: 50) { nodes { slug name id } } } }",
    )
    nodes = (
        ((payload.get("data") or {}).get("viewer") or {}).get("enterprises") or {}
    ).get("nodes") or []
    result = [
        {"slug": n.get("slug"), "name": n.get("name") or n.get("slug"), "id": n.get("id")}
        for n in nodes
        if n and n.get("slug")
    ]
    await _cache.put(("ents", token), result)
    return result


async def list_enterprise_orgs(token: str, slug: str) -> dict:
    """Orgs of an enterprise. Returns {organizations, total, truncated, error}.

    The org count can be enormous, so paging stops at _MAX_ORGS and sets truncated --
    better to report truncation explicitly than to silently list fewer (which would let
    an admin believe some org does not exist).
    """
    cached = await _cache.get(("orgs", token, slug), _DISCOVERY_TTL)
    if cached is not None:
        return cached  # type: ignore[return-value]

    query = """
    query($slug: String!, $first: Int!, $after: String) {
      enterprise(slug: $slug) {
        organizations(first: $first, after: $after, orderBy: {field: LOGIN, direction: ASC}) {
          totalCount
          pageInfo { hasNextPage endCursor }
          nodes { login name }
        }
      }
    }
    """
    orgs: list[dict] = []
    total = 0
    truncated = False
    cursor: str | None = None
    error: str | None = None
    try:
        while True:
            payload = await _graphql(
                token, query, {"slug": slug, "first": _ORG_PAGE, "after": cursor}
            )
            ent = (payload.get("data") or {}).get("enterprise")
            if ent is None:
                # Common on very large enterprises: data.enterprise is null plus
                # RESOURCE_LIMITS_EXCEEDED
                errs = payload.get("errors") or []
                error = (
                    (errs[0].get("message") if errs else None)
                    or "the enterprise organization list is unavailable"
                )
                break
            conn = ent.get("organizations") or {}
            total = conn.get("totalCount") or 0
            for n in conn.get("nodes") or []:
                if n and n.get("login"):
                    orgs.append({"login": n["login"], "name": n.get("name") or n["login"]})
            page = conn.get("pageInfo") or {}
            if not page.get("hasNextPage") or len(orgs) >= _MAX_ORGS:
                truncated = bool(page.get("hasNextPage"))
                break
            cursor = page.get("endCursor")
    except GitHubAdminError as e:
        error = str(e)

    result = {
        "organizations": orgs,
        "total": total or len(orgs),
        "truncated": truncated,
        "error": error,
    }
    await _cache.put(("orgs", token, slug), result)
    return result


async def list_enterprise_teams(token: str, slug: str) -> dict:
    """Enterprise teams. This endpoint 404s on some enterprises (which is not the same
    as "has no teams"), so an error is distinguished from an empty list.

    Membership checks require the numeric id, not the `ent:`-prefixed slug (measured:
    the slug 404s), so the id is kept here and the policy layer stores ids too.
    """
    cached = await _cache.get(("teams", token, slug), _DISCOVERY_TTL)
    if cached is not None:
        return cached  # type: ignore[return-value]

    teams: list[dict] = []
    error: str | None = None
    try:
        status, body = await _rest(
            token, f"/enterprises/{slug}/teams?per_page=100", allow_404=True
        )
        if status == 404:
            error = (
                "this enterprise does not support the Enterprise Teams endpoint "
                "(GitHub returned 404)"
            )
        elif isinstance(body, list):
            teams = [
                {
                    "id": t.get("id"),
                    "slug": t.get("slug"),
                    "name": t.get("name") or t.get("slug"),
                }
                for t in body
                if t and t.get("id") is not None
            ]
    except GitHubAdminError as e:
        error = str(e)

    result = {"teams": teams, "error": error}
    await _cache.put(("teams", token, slug), result)
    return result


async def discover(token: str) -> dict:
    """Fetch everything the configuration page needs in one go: enterprises plus each
    one's orgs and teams."""
    enterprises = await list_enterprises(token)
    # Fetched concurrently; the enterprise count is small (3 in practice), so this does
    # not trigger rate limiting
    detail = await asyncio.gather(
        *[
            asyncio.gather(
                list_enterprise_orgs(token, e["slug"]),
                list_enterprise_teams(token, e["slug"]),
            )
            for e in enterprises
        ]
    )
    out = []
    for ent, (orgs, teams) in zip(enterprises, detail):
        out.append({
            **ent,
            "organizations": orgs["organizations"],
            "organizations_total": orgs["total"],
            "organizations_truncated": orgs["truncated"],
            "organizations_error": orgs["error"],
            "teams": teams["teams"],
            "teams_error": teams["error"],
        })
    return {"enterprises": out}


# -- Membership checks --------------------------------------------------------
async def check_org_member(token: str, org: str, login: str) -> bool:
    """Org membership -- the most reliable and cheapest route.

    `/orgs/{org}/members/{login}`: 204 = yes, 404 = no, **302 = this token cannot see
    that org's members** (GitHub uses a redirect for "not allowed to look", which does
    not mean the user is not a member).
    On 302, fall back to `/orgs/{org}/memberships/{login}` (needs admin:org) for a
    definite answer; when neither can answer, treat it as fail-closed -- better to
    withhold a key than to hand one out wrongly.
    """
    key = ("org-member", token, org.lower(), login.lower())
    cached = await _cache.get(key, _MEMBERSHIP_TTL)
    if cached is not None:
        return bool(cached)
    try:
        status, _ = await _rest(
            token, f"/orgs/{org}/members/{login}", allow_404=True
        )
        if status == 302:
            result = await _org_member_via_membership(token, org, login)
        else:
            result = status == 204
    except GitHubAdminError as e:
        logger.warning("org membership check failed org=%s login=%s: %s", org, login, e)
        return False
    await _cache.put(key, result)
    return result


async def _org_member_via_membership(token: str, org: str, login: str) -> bool:
    """Fallback when members/ returns 302: only 200 with state=active counts."""
    try:
        status, body = await _rest(
            token, f"/orgs/{org}/memberships/{login}", allow_404=True
        )
    except GitHubAdminError as e:
        logger.warning(
            "org membership check unavailable (token cannot see this org) "
            "org=%s login=%s: %s", org, login, e
        )
        return False
    if status != 200 or not isinstance(body, dict):
        return False
    return body.get("state") == "active"


async def check_enterprise_team_member(
    token: str, slug: str, team_id: int | str, login: str
) -> bool:
    """200 = member; everything else (404 / 302 not-allowed-to-look) counts as not a
    member. The numeric team id is required.

    Only 200 is accepted on purpose -- an empty-bodied 2xx/3xx is not evidence of
    membership, and fail-closed beats issuing a key by mistake.
    """
    key = ("team-member", token, slug, str(team_id), login.lower())
    cached = await _cache.get(key, _MEMBERSHIP_TTL)
    if cached is not None:
        return bool(cached)
    try:
        status, _ = await _rest(
            token,
            f"/enterprises/{slug}/teams/{team_id}/memberships/{login}",
            allow_404=True,
        )
        result = status == 200
    except GitHubAdminError as e:
        logger.warning(
            "enterprise team membership check failed ent=%s team=%s login=%s: %s",
            slug, team_id, login, e
        )
        return False
    await _cache.put(key, result)
    return result


async def check_enterprise_member(token: str, slug: str, login: str) -> bool | None:
    """Enterprise membership. **Tri-state**: True / False / None (GitHub cannot answer).

    None occurs on very large enterprises: GraphQL reports RESOURCE_LIMITS_EXCEEDED
    (even with a login filter) and consumed-licenses returns 404. Callers must treat
    None as "no match" and fall back to org/team checks -- never as True.
    """
    key = ("ent-member", token, slug, login.lower())
    cached = await _cache.get(key, _MEMBERSHIP_TTL)
    if cached is not None:
        return None if cached == "unknown" else bool(cached)

    result: bool | None = None
    # Route 1: login-filtered GraphQL member query (works on small/medium enterprises)
    try:
        payload = await _graphql(
            token,
            """
            query($slug: String!, $q: String!) {
              enterprise(slug: $slug) {
                members(first: 10, query: $q) {
                  nodes {
                    __typename
                    ... on EnterpriseUserAccount { login }
                    ... on User { login }
                  }
                }
              }
            }
            """,
            {"slug": slug, "q": login},
        )
        ent = (payload.get("data") or {}).get("enterprise")
        if ent is not None:
            nodes = (ent.get("members") or {}).get("nodes") or []
            result = any(
                (n or {}).get("login", "").lower() == login.lower() for n in nodes
            )
    except GitHubAdminError as e:
        logger.info("enterprise member check via GraphQL unavailable ent=%s: %s", slug, e)

    # Route 2: consumed-licenses (fallback when GraphQL is unavailable; can also 404)
    if result is None:
        try:
            status, body = await _rest(
                token,
                f"/enterprises/{slug}/consumed-licenses?per_page=100",
                allow_404=True,
            )
            if status != 404 and isinstance(body, dict):
                users = body.get("users") or []
                result = any(
                    (u or {}).get("github_com_login", "").lower() == login.lower()
                    for u in users
                )
        except GitHubAdminError as e:
            logger.info(
                "enterprise member check via licenses unavailable ent=%s: %s", slug, e
            )

    await _cache.put(key, "unknown" if result is None else result)
    return result
