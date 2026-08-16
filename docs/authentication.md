# Sign-in and authentication

**Why this exists**: when Copilot calls through BYOK it does not pass any user identity —
`x-user-id` is always empty and `user_id` in the logs can only be `null`, which makes per-user
tracking impossible. API keys are therefore **mandatory**: every key belongs to one GitHub account,
and a request's `user_id` is determined by the key's owner and cannot be forged (an `x-user-id` sent
by the client is ignored).

## 1. Create a GitHub OAuth App

GitHub → Settings → Developer settings → OAuth Apps → New OAuth App:

| Field | Value |
|---|---|
| Homepage URL | `http://localhost:8000/` |
| Authorization callback URL | `http://localhost:8000/v1/auth/github/callback` |

Note down the Client ID and Client Secret. When deploying to another domain, replace
`localhost:8000` with the real address (the callback path stays the same).

## 2. Fill in the configuration

Either way works:

- **The setup wizard** (recommended): while `auth.github.client_id` is empty, visiting `/` **from
  the machine running the service** shows a guided page where you enter the Client ID / Secret and
  the administrator GitHub logins. That channel is open only while "unconfigured + coming from
  127.0.0.1"; afterwards it returns 409 forever.
- **Editing `config.yaml`** by hand, in the `auth` section (the only option for a first remote
  deployment), then restarting the service.

Once configured, an administrator can change the OAuth credentials at any time on the console's
"Access control → GitHub OAuth" page; saving applies them immediately, with no restart.

```yaml
auth:
  github:
    client_id: 'Iv1.xxxxxxxxxxxx'
    client_secret: 'xxxxxxxx'
    callback_url: ''            # empty = derived from the request origin
  admin_logins: [satomic]       # these GitHub logins get the administrator view
  allow_any_github_user: true   # false = only admin_logins may sign in
  session_ttl_seconds: 604800
```

## 3. Create an API key and use it in Copilot

Sign in to the console → "API keys" → New. The key is shown at creation and **stays readable to its
owner** afterwards: the API keys table has a Key column with Show and Copy, so a credential never has
to be recreated just because a panel was closed. Copy it into the client:

| Field | Value |
|---|---|
| Base URL | `http://localhost:8000/v1` |
| API Key | `fmr_...` |
| Model | any model name registered under "Routing configuration" |

From the command line:

```powershell
curl http://localhost:8000/v1/chat/completions `
  -H "Authorization: Bearer fmr_xxxxx" `
  -H "Content-Type: application/json" `
  -d '{"messages":[{"role":"user","content":"help me refactor this module architecture"}]}'
```

The key can also be passed via an `api-key:` or `x-api-key:` header (for client compatibility). Keys
can be disabled or deleted at any time, effective immediately.

`data/api_keys.json` stores each key's sha256 hash — still the only thing an incoming request is
compared against — **alongside its plaintext**, which is what makes the key readable later. That file
is gitignored, and the plaintext is only ever returned to the key's own owner: an administrator's
`?all=1` listing carries metadata and the prefix only, so an administrator can disable or delete
anybody's key but never read it. Keys created before this behaviour existed have no stored plaintext
and show as "unavailable" — a hash cannot be reversed — while continuing to work normally.

## The local super administrator (when github.com is unreachable)

GitHub OAuth is not always available — a network without access to github.com would otherwise leave
the console with no door at all, since administrator status is derived from `auth.admin_logins`. So
there is a second, local identity, configured under `auth.local_admin` in `config.yaml`:

```yaml
auth:
  local_admin:
    enabled: true
    username: admin
    password_hash: ''      # scrypt, hex. Empty = the default password is still in force
    password_salt: ''      # per-record random salt, hex
    updated_at: null
```

The default credential is **`admin` / `admin1234`**, and it is deliberately close to useless: it
signs in, but the session carries `must_change_password`, and **every endpoint returns 403 with the
detail `password_change_required`** except status, logout and the change-password call itself, until
the password is changed. The console mirrors that with a change-password screen ahead of the whole
shell. A super-administrator account sitting on a documented password must not be usable.

The username and password are both changeable from "Access control → Local administrator" (or via
`POST /v1/auth/local/password`, which requires the current password). A new password must be at
least 8 characters and must not be the default. Changing it drops every *other* local-admin session,
and blanking `password_hash` / `password_salt` in `config.yaml` by hand is the documented recovery
path for a forgotten password — it puts the default back, gate included.

Storage is `hashlib.scrypt` (n=2¹⁴, r=8, p=1) with a per-record 16-byte random salt, verified with
`secrets.compare_digest`. Deliberately **not** the sha256 used for API keys: that is right for a
256-bit random key and wrong for a human-chosen password. The digest is never returned by any
endpoint — `GET /v1/config` blanks both fields while keeping `username` and `updated_at`, and a save
that submits them empty is read as "unchanged" rather than as a reset.

**This is the one brute-forceable surface in the application.** Everything else authenticates a
256-bit random key or an OAuth session; a password can be guessed. There is no rate limiting — the
console is loopback-first by design — so on a deployment reachable from a network, put it behind a
reverse proxy that rate-limits `POST /v1/auth/local/login`, or set `enabled: false` and rely on
OAuth. A wrong username and a wrong password return the identical 401, so the endpoint does not
reveal whether the account has been renamed.

A local administrator is a full administrator: it sees every user's traces and usage, and it is the
identity that can delete traces.

## Permission matrix

| Endpoint | Identity required |
|---|---|
| `POST /v1/chat/completions`, `GET /v1/models` | **a valid API key** (no key → 401) |
| `GET /v1/auth/status`, `/v1/auth/github/*`, `GET /healthz`, the console (`/` and every non-API path) | public |
| `POST /v1/auth/setup` | only "OAuth unconfigured + request from the local machine" |
| `GET/PATCH/DELETE /v1/keys` | any signed-in user (their own keys only; an administrator sees all with `?all=1`) |
| `POST /v1/keys` | a signed-in user **and** passing the key policy (not in any allowed enterprise/organization → 403, see [access control](access-control.md)) |
| `GET /v1/access/me` | any signed-in user (returns their own key-creation verdict) |
| `GET /v1/usage` | any signed-in user (their own data; an administrator sees everything and can drill down with `?user_id=`) |
| `GET /v1/traces`, `/v1/traces/{id}`, `/v1/router/decisions` | any signed-in user (forcibly filtered to themselves; administrators unrestricted) |
| `DELETE /v1/traces/{id}`, `DELETE /v1/traces?date=&user_id=` | administrators only — a user must not be able to erase the audit trail of their own calls |
| `POST /v1/auth/local/login` | public (the local super administrator's sign-in) |
| `POST /v1/auth/local/password` | the local super administrator itself (reachable even while the forced password change is pending) |
| `POST /v1/auth/local/enabled` | administrators only |
| `GET/PUT /v1/config`, `GET /v1/access/token`, `POST /v1/access/verify-token`, `GET /v1/access/discover`, `GET /v1/access/cache`, `POST /v1/access/cache/refresh` | administrators only (otherwise 403) |

A normal user sees only "Usage / API keys / Traces / Playground", with the data scope locked to
themselves; an administrator additionally gets the "Routing configuration" and "Access control"
pages, plus cross-user statistics and traces.

Sessions and keys are persisted under `data/` (`auth_sessions.json` / `api_keys.json`, both
gitignored), so sign-in state and keys survive a restart.
