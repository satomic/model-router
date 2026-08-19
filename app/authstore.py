"""JSON-file persistence for sign-in sessions, API keys, and who has ever signed in.

data/auth_sessions.json  sessions (survive a restart; expired entries are pruned on read)
data/api_keys.json       API keys -- both the sha256 hash (what lookup compares) and the
                         plaintext (so an owner can read their own key back later). The
                         plaintext is handed out only to the key's owner; see public_key().
data/known_users.json    one durable entry per login that has ever signed in, with first and
                         last sign-in and a count. Sessions cannot answer "who has used this
                         deployment": they are purged the moment they expire, so a user who
                         signed in last week leaves no trace in them at all. The model policy
                         needs that list -- an admin has to be able to bind a group to a user
                         without first asking them to spell their login.

Modelled on OctoFinance's services/auth_store.py: no database, JSON on disk plus an
in-memory cache, and multiple workers only need to share the same data/ directory.
"""
import hashlib
import json
import secrets
import threading
import time
from pathlib import Path

# Only the *display* prefix and what new keys are minted with: lookup compares the sha256
# digest (hash_key below), so keys issued under an older prefix keep working unchanged.
KEY_PREFIX = "mr_"
_PREFIX_VISIBLE = 8  # How much of the plaintext prefix the listing shows


def hash_key(plaintext: str) -> str:
    return hashlib.sha256(plaintext.encode("utf-8")).hexdigest()


def _now() -> float:
    return time.time()


# These three are public because app/ghcache.py persists its own JSON under data/ and needs
# exactly the same read / atomic-write / mtime semantics. Writing tmp.replace() twice is a
# divergence waiting to happen.
def read_json(path: Path, fallback: dict) -> dict:
    if not path.exists():
        return dict(fallback)
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else dict(fallback)
    except Exception:  # noqa: BLE001 a corrupt file must not block startup
        return dict(fallback)


def write_json(path: Path, data: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(path)  # Atomic replace, so a concurrent reader never sees half a file


def mtime(path: Path) -> float:
    try:
        return path.stat().st_mtime_ns / 1e9
    except OSError:
        return 0.0


# Kept as aliases: the private names are used throughout this module and by verify scripts.
_read_json = read_json
_write_json = write_json
_mtime = mtime


class AuthStore:
    def __init__(self, data_dir: Path):
        self.sessions_path = data_dir / "auth_sessions.json"
        self.keys_path = data_dir / "api_keys.json"
        self.users_path = data_dir / "known_users.json"
        self._lock = threading.Lock()
        self._users_lock = threading.Lock()
        self._sessions: dict[str, dict] = _read_json(self.sessions_path, {})
        self._keys: dict[str, dict] = _read_json(self.keys_path, {})
        self._sessions_mtime = _mtime(self.sessions_path)
        self._keys_mtime = _mtime(self.keys_path)
        self._purge_expired()

    # -- Cache synchronisation ------------------------------------------------
    # Disk is re-read only on a cache miss: that way sessions/keys written by another
    # worker (or an external script) are still recognised, while the common case of a
    # cache hit costs no extra IO. Callers must hold self._lock.
    def _reload_sessions_if_changed(self) -> bool:
        mtime = _mtime(self.sessions_path)
        if mtime == self._sessions_mtime:
            return False
        self._sessions = _read_json(self.sessions_path, {})
        self._sessions_mtime = mtime
        return True

    def _reload_keys_if_changed(self) -> bool:
        mtime = _mtime(self.keys_path)
        if mtime == self._keys_mtime:
            return False
        self._keys = _read_json(self.keys_path, {})
        self._keys_mtime = mtime
        return True

    def _save_sessions(self) -> None:
        _write_json(self.sessions_path, self._sessions)
        self._sessions_mtime = _mtime(self.sessions_path)

    def _save_keys(self) -> None:
        _write_json(self.keys_path, self._keys)
        self._keys_mtime = _mtime(self.keys_path)

    # -- Sessions -------------------------------------------------------------
    def _purge_expired(self) -> None:
        with self._lock:
            now = _now()
            alive = {
                sid: s for sid, s in self._sessions.items()
                if s.get("expires_at", 0) > now
            }
            if len(alive) != len(self._sessions):
                self._sessions = alive
                self._save_sessions()

    def create_session(self, user: dict, ttl_seconds: int) -> str:
        sid = secrets.token_urlsafe(32)
        with self._lock:
            # Merge in other writers' entries first, so the whole table is not overwritten
            self._reload_sessions_if_changed()
            self._sessions[sid] = {
                **user,
                "created_at": _now(),
                "expires_at": _now() + ttl_seconds,
            }
            self._save_sessions()
        # Recorded here rather than at each call site: there are two ways to open a session
        # (GitHub OAuth and the local administrator) and a third would be easy to add without
        # remembering the registry. A failure to record must not fail the sign-in, so
        # record_sign_in swallows its own IO errors.
        self.record_sign_in(user)
        return sid

    def get_session(self, sid: str | None) -> dict | None:
        if not sid:
            return None
        with self._lock:
            s = self._sessions.get(sid)
            if s is None and self._reload_sessions_if_changed():
                s = self._sessions.get(sid)
            if s is None:
                return None
            if s.get("expires_at", 0) <= _now():
                del self._sessions[sid]
                self._save_sessions()
                return None
            return dict(s)

    def delete_session(self, sid: str | None) -> None:
        if not sid:
            return
        with self._lock:
            self._reload_sessions_if_changed()
            if self._sessions.pop(sid, None) is not None:
                self._save_sessions()

    def refresh_admin_flags(self, is_admin_login) -> None:
        """Recompute is_admin on existing sessions after the admin list changes, so
        nobody has to sign in again.

        Local-administrator sessions are skipped: their authority comes from
        auth.local_admin rather than admin_logins, so running them through this predicate
        would strip the flag that auth.current_user just recomputed for them.
        """
        with self._lock:
            self._reload_sessions_if_changed()
            changed = False
            for s in self._sessions.values():
                if s.get("local_admin"):
                    continue
                flag = is_admin_login(s.get("login", ""))
                if s.get("is_admin") != flag:
                    s["is_admin"] = flag
                    changed = True
            if changed:
                self._save_sessions()

    def rekey_local_admin(self, sid: str | None, new_login: str) -> None:
        """After the local administrator's credential changes: point this session at the
        new username and drop every *other* local-admin session.

        Changing the password has to invalidate sessions opened with the old one -- that is
        most of what changing it is for -- while the operator doing the change stays signed
        in rather than being bounced back to the login form.
        """
        with self._lock:
            self._reload_sessions_if_changed()
            changed = False
            for other in [k for k, s in self._sessions.items() if s.get("local_admin") and k != sid]:
                del self._sessions[other]
                changed = True
            current = self._sessions.get(sid) if sid else None
            if current is not None and current.get("login") != new_login:
                current["login"] = new_login
                current["name"] = new_login
                changed = True
            if changed:
                self._save_sessions()

    # -- Known users ----------------------------------------------------------
    # A separate file and a separate lock from the session table. The two have opposite
    # lifetimes -- a session is transient and pruned, this is append-only history -- and keeping
    # history in the sessions file would have made _purge_expired delete it.
    def record_sign_in(self, user: dict) -> None:
        """Note that `user` signed in: first_seen, last_seen, and a sign-in count.

        Never raises. Being unable to write the registry is not a reason to refuse a sign-in,
        and there is no caller in a position to do anything useful with the error.
        """
        login = str(user.get("login") or "").strip()
        if not login:
            return
        try:
            with self._users_lock:
                data = _read_json(self.users_path, {})
                entry = data.get(login.lower()) or {}
                now = _now()
                data[login.lower()] = {
                    "login": login,
                    "name": user.get("name") or entry.get("name") or login,
                    "avatar_url": user.get("avatar_url") or entry.get("avatar_url"),
                    # Which door they came through, so the admin list can distinguish the local
                    # administrator from a GitHub user of the same name.
                    "kind": "local" if user.get("local_admin") else "github",
                    "first_seen": entry.get("first_seen") or now,
                    "last_seen": now,
                    "sign_ins": int(entry.get("sign_ins") or 0) + 1,
                }
                _write_json(self.users_path, data)
        except Exception as e:  # noqa: BLE001 see the docstring
            import logging

            logging.getLogger("mr").warning("could not record the sign-in of %s: %s", login, e)

    def list_known_users(self) -> list[dict]:
        """Every login that has ever signed in, most recent first.

        Read straight from disk each time: this feeds an admin page rather than a request path,
        and a cache would only add a way for one worker's list to lag another's.
        """
        users = [u for u in _read_json(self.users_path, {}).values() if isinstance(u, dict)]
        return sorted(users, key=lambda u: u.get("last_seen") or 0, reverse=True)

    # -- API keys -------------------------------------------------------------
    def create_api_key(self, user_login: str, name: str) -> tuple[dict, str]:
        """Return (record, plaintext key).

        Both the plaintext and its digest are persisted: the digest is what lookup
        compares, and the plaintext is what lets the owner read their own key back later
        instead of having to delete it and reconfigure every client. It is only ever
        handed to that owner -- see public_key().
        """
        plaintext = KEY_PREFIX + secrets.token_urlsafe(32)
        key_id = secrets.token_hex(8)
        record = {
            "id": key_id,
            "name": name or "default",
            "user_login": user_login,
            "key_hash": hash_key(plaintext),
            "key": plaintext,
            "prefix": plaintext[: len(KEY_PREFIX) + _PREFIX_VISIBLE],
            "created_at": _now(),
            "last_used_at": None,
            "request_count": 0,
            "disabled": False,
        }
        with self._lock:
            # Merge in other writers' entries first, so the whole table is not overwritten
            self._reload_keys_if_changed()
            self._keys[key_id] = record
            self._save_keys()
        return self.public_key(record, include_secret=True), plaintext

    def lookup_api_key(self, plaintext: str) -> dict | None:
        """Look up an enabled key record by its plaintext."""
        if not plaintext:
            return None
        digest = hash_key(plaintext)
        with self._lock:
            for attempt in range(2):
                for record in self._keys.values():
                    if record.get("key_hash") == digest and not record.get("disabled"):
                        return dict(record)
                # Missed on the first pass: another worker may have just created this
                # key, so re-read from disk once and retry
                if attempt == 0 and not self._reload_keys_if_changed():
                    break
        return None

    def touch_api_key(self, key_id: str) -> None:
        with self._lock:
            record = self._keys.get(key_id)
            if record is None:
                return
            record["last_used_at"] = _now()
            record["request_count"] = int(record.get("request_count") or 0) + 1
            self._save_keys()

    def list_api_keys(
        self, user_login: str | None = None, include_secret: bool = False
    ) -> list[dict]:
        """A user_login of None returns every key (the administrator view).

        include_secret is only correct when user_login names the caller themselves: the
        cross-user administrator view must never carry plaintext.
        """
        with self._lock:
            self._reload_keys_if_changed()
            records = list(self._keys.values())
        if user_login is not None:
            records = [r for r in records if r.get("user_login") == user_login]
        records.sort(key=lambda r: r.get("created_at") or 0, reverse=True)
        return [self.public_key(r, include_secret=include_secret) for r in records]

    def get_api_key(self, key_id: str) -> dict | None:
        with self._lock:
            record = self._keys.get(key_id)
            if record is None and self._reload_keys_if_changed():
                record = self._keys.get(key_id)
            return dict(record) if record else None

    def delete_api_key(self, key_id: str) -> bool:
        with self._lock:
            self._reload_keys_if_changed()
            if self._keys.pop(key_id, None) is None:
                return False
            self._save_keys()
            return True

    def set_api_key_disabled(self, key_id: str, disabled: bool) -> dict | None:
        with self._lock:
            self._reload_keys_if_changed()
            record = self._keys.get(key_id)
            if record is None:
                return None
            record["disabled"] = disabled
            self._save_keys()
            return self.public_key(record)

    @staticmethod
    def public_key(record: dict, include_secret: bool = False) -> dict:
        """The outward-facing representation.

        The digest never leaves the process. The plaintext leaves only once the caller has
        been confirmed to be the key's owner, hence include_secret defaulting to False: a
        new call site has to opt in deliberately rather than leak by omission. Keys created
        before the plaintext was stored simply have no "key" field, and the console renders
        them as unavailable.
        """
        out = {k: v for k, v in record.items() if k not in ("key_hash", "key")}
        if include_secret and record.get("key"):
            out["key"] = record["key"]
        return out
