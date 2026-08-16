# Getting started

```powershell
.venv\Scripts\Activate.ps1
pip install -r requirements.txt
copy config.example.yaml config.yaml     # first run: generate the local config from the template
uvicorn app.main:app --host 0.0.0.0 --port 8000
```

Open http://localhost:8000/ . The first visit lands on the **setup wizard** (see
[Sign-in and authentication](authentication.md)); once configured, sign in with GitHub to reach the
console.

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
