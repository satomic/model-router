"""Shared auth wiring for the verify scripts: seed an admin session and an API key into data/.

The server enforces authentication unconditionally (there is no switch), and GitHub OAuth needs a
real browser interaction, so automated scripts do not go through OAuth -- they write AuthStore's
on-disk files directly. AuthStore re-reads them by mtime on a cache miss, so a running server picks
up the credentials created here immediately.

The admin identity reuses the existing auth.admin_logins[0] from config.yaml: `current_user`
recomputes is_admin from the cfg held by the server process on every request, so a name this script
newly wrote into config.yaml would only take effect after a restart. Hence we can only use the
login the server already knows about.
"""
from __future__ import annotations

import _bootstrap  # noqa: F401  puts the repository root on sys.path and chdir()s into it

import httpx

from app.authstore import AuthStore
from app.config import DATA_DIR, load_raw

BASE = "http://127.0.0.1:8000"
KEY_NAME = "verify-script"


def admin_login() -> str:
    """Return an admin login the server already accepts; abort with guidance if none is configured."""
    auth = load_raw().get("auth") or {}
    admins = [str(x).strip() for x in (auth.get("admin_logins") or []) if str(x).strip()]
    if not admins:
        raise SystemExit(
            "auth.admin_logins in config.yaml is empty, so the verify scripts cannot obtain an "
            "administrator identity.\n"
            "Put your GitHub login there first (e.g. admin_logins: [satomic]), restart the server, "
            "then run this script again."
        )
    return admins[0]


def make_client() -> tuple[httpx.Client, str, str]:
    """Return (client carrying an admin session cookie, API key plaintext, owning login)."""
    login = admin_login()
    store = AuthStore(DATA_DIR)

    # Drop the script key left over from the previous run so they do not pile up
    for record in store.list_api_keys(login):
        if record.get("name") == KEY_NAME:
            store.delete_api_key(record["id"])

    sid = store.create_session(
        {"login": login, "name": login, "avatar_url": None, "is_admin": True}, 3600
    )
    _record, key = store.create_api_key(login, KEY_NAME)

    client = httpx.Client(
        base_url=BASE,
        timeout=300,
        cookies={"mr_session": sid},
        headers={"Authorization": f"Bearer {key}"},
    )
    status = client.get("/v1/auth/status").json()
    user = status.get("user") or {}
    if not user.get("is_admin"):
        raise SystemExit(
            f"session created but {login} is not an administrator -- the admin_logins inside the "
            "server process differ from config.yaml; restart the server and try again."
        )
    return client, key, login
