"""The local super administrator's credential: hashing, verification, and the
forced-change rule.

This account exists so the console stays reachable where GitHub is not -- an air-gapped
network, a blocked github.com, a misconfigured OAuth app. It is a genuine super
administrator (it sees every user's traces and may delete them), so the credential gets
treated accordingly:

  * The password is stored salted with scrypt, never with authstore.hash_key. That helper
    is a bare unsalted sha256, which is right for a 256-bit random API key and wrong for
    a human-chosen password. scrypt ships in hashlib, so this costs no new dependency.
  * Until the password has actually been changed, the default one is in force and the
    session is refused everywhere except the change-password form -- a super-admin account
    on a documented credential must not be usable as-is. See app/auth.py.
  * The hash lives in config.yaml alongside every other secret in this project, and that
    file is gitignored.
"""
import hashlib
import secrets

DEFAULT_USERNAME = "admin"
# Documented in the README and in config.example.yaml. It is only ever enough to reach the
# change-password form, never the console itself.
DEFAULT_PASSWORD = "admin1234"

MIN_PASSWORD_LENGTH = 8

# Cost parameters. n=2**14 keeps a sign-in comfortably under ~100ms on a laptop while
# making an offline guessing run against the stored hash expensive.
_SCRYPT_N = 2 ** 14
_SCRYPT_R = 8
_SCRYPT_P = 1
_DK_LEN = 32


def hash_password(password: str, salt_hex: str | None = None) -> tuple[str, str]:
    """Return (salt_hex, hash_hex). A fresh random salt unless one is supplied."""
    salt = bytes.fromhex(salt_hex) if salt_hex else secrets.token_bytes(16)
    digest = hashlib.scrypt(
        password.encode("utf-8"), salt=salt,
        n=_SCRYPT_N, r=_SCRYPT_R, p=_SCRYPT_P, dklen=_DK_LEN,
    )
    return salt.hex(), digest.hex()


def has_stored_password(cfg) -> bool:
    la = cfg.local_admin
    return bool(str(la.get("password_hash") or "").strip() and str(la.get("password_salt") or "").strip())


def must_change(cfg) -> bool:
    """True while the built-in default password is still in force.

    Blanking the hash fields in config.yaml therefore restores the default password --
    the deliberate lockout-recovery path for an operator who has lost the new one.
    """
    return cfg.local_admin_enabled and not has_stored_password(cfg)


def verify_password(cfg, password: str) -> bool:
    """Constant-time check against the stored hash, or against the default password while
    no hash has been stored yet."""
    if not cfg.local_admin_enabled:
        return False
    if not has_stored_password(cfg):
        return secrets.compare_digest(password, DEFAULT_PASSWORD)
    la = cfg.local_admin
    try:
        _, candidate = hash_password(password, str(la["password_salt"]))
    except ValueError:  # a corrupt / hand-edited salt
        return False
    return secrets.compare_digest(candidate, str(la["password_hash"]))


def validate_new_password(password: str) -> str:
    """Return an error message for an unacceptable new password, or '' when it is fine."""
    if len(password) < MIN_PASSWORD_LENGTH:
        return f"password must be at least {MIN_PASSWORD_LENGTH} characters"
    if password == DEFAULT_PASSWORD:
        # Otherwise "change the password" could be satisfied by re-entering the documented
        # one, which would leave the account exactly as exposed as before.
        return "choose a password other than the default one"
    return ""


def validate_username(username: str) -> str:
    """Return an error message for an unacceptable username, or '' when it is fine."""
    name = (username or "").strip()
    if not name:
        return "username must not be empty"
    if len(name) > 64:
        return "username must be at most 64 characters"
    return ""
