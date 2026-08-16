# Getting started (from source)

For a deployment, [Docker](docker.md) is the shorter path — one command, no toolchain, and upgrades
are a new image over the same volume. Run from source to develop the router itself.

```powershell
.venv\Scripts\Activate.ps1
pip install -r requirements.txt
cd frontend; npm ci; npm run build; cd ..   # FastAPI serves the built console from /
uvicorn app.main:app --host 0.0.0.0 --port 8000
```

`config.yaml` does not have to be created by hand: on startup it is copied from
[config.example.yaml](../config.example.yaml) if it is missing, comments and all, and the log says
so. An existing file is never overwritten.

Open http://localhost:8000/ . The first visit lands on the **setup wizard** (see
[Sign-in and authentication](authentication.md)); once configured, sign in with GitHub to reach the
console. The wizard only accepts requests from `127.0.0.1`, so a remote or containerised deployment
signs in as the local super administrator (`admin` / `admin1234`) and configures OAuth from the
console instead.

Everything the router persists lives under a single directory, `data/`:

```
data/config.yaml          the configuration, rewritten whenever the console saves
data/auth_sessions.json   sign-in sessions
data/api_keys.json        issued API keys
data/github/              the cached GitHub structure and member lists
data/logs/traces/         full-chain trace records
```

That is the whole state of a deployment — one directory to back up, copy to another machine, or
mount into a container. All of it is gitignored.

Each location is overridable, though a normal deployment only ever needs the first:

| Variable | Default | Contents |
|---|---|---|
| `MR_DATA_DIR` | `<repo>/data` | **the root of all persistent state**; the other two default to positions inside it |
| `MR_CONFIG_FILE` | `<data>/config.yaml` | the configuration file, read at startup and rewritten by the console |
| `MR_LOG_DIR` | `<data>/logs` | trace records, under `traces/` |

Upgrading an existing checkout needs no action: a `config.yaml` or `logs/traces/` still at the
repository root is moved under `data/` on the next start, and the log says what moved. Nothing is
overwritten — if a file already exists at the new location, the old one is left where it is.

The console owns the site root and every page has its own address — `/usage`, `/keys`,
`/traces/<id>`, `/config/models`, `/access/policy` — so a page can be bookmarked or pasted to a
colleague. Any unknown path is answered with the console shell, which then renders its own
not-found view.

Behind a reverse proxy, add `--proxy-headers` so the callback URL and the cookie's `Secure` flag
follow `X-Forwarded-Proto` / `X-Forwarded-Host` correctly.

Frontend development mode (optional, with hot reload):

```powershell
cd frontend; npm run dev   # http://localhost:5173/, API calls proxied to :8000
npm run build              # the build output is served by FastAPI from /
```

## Language

The console renders in **English, Simplified Chinese, Traditional Chinese, Japanese or Korean**.
Pick a language from the selector in the top bar (also available on the sign-in and setup pages, so
the choice can be made before signing in). The choice is stored under the `locale` key in
`localStorage` and also written to `<html lang>`.

English is the source language:
[frontend/src/i18n/locales/en.json](../frontend/src/i18n/locales/en.json) is authored first and acts
as the fallback, and the other four catalogs are translations of it.
`node frontend/scripts/check-locales.mjs` gates key parity — all five catalogs must have identical
key sets.

**The backend deliberately speaks English only**, in every locale. Text composed server-side is not
translated: the API-key eligibility `reason`, configuration validation errors, GitHub and upstream
error messages, and the trace `analysis` notes. Those strings reach the UI verbatim, so an error
banner shows English even when the console is set to Japanese. This keeps the API's responses
identical for every client and avoids locale plumbing through the request path.
