# API

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible, streaming supported. **Requires an API key**; optional `x-interaction-id` (one decision and one trace per user interaction) and `x-session-id` (session stickiness); response headers include `x-trace-id` (the *interaction's* trace) / `x-routed-model` / `x-router-reason` / `x-router-decision-ms`, plus `x-router-interaction-id` when the request carried one |
| `GET /v1/models` | the available backend models (requires an API key) |
| `GET /v1/auth/status` | what the frontend uses to choose between the setup page, the sign-in page and the console |
| `POST /v1/auth/setup` | the first-run wizard (only while unconfigured + from the local machine) |
| `GET /v1/auth/github/login` · `GET /v1/auth/github/callback` · `POST /v1/auth/logout` | the GitHub OAuth flow |
| `GET/POST /v1/keys`, `PATCH/DELETE /v1/keys/{id}` | list / create (returns the one-time plaintext, subject to the key policy) / disable / delete API keys |
| `GET /v1/access/me` | whether the signed-in user may create a key: the verdict, the reason, and the per-check evidence |
| `GET /v1/access/token` | the status of the Enterprise administrator token (administrators; returns only a mask and the owner, **never echoes the plaintext**) |
| `POST /v1/access/verify-token` | validate a token and report its scopes (administrators) |
| `GET /v1/access/discover?refresh=` | automatically fetch the enterprise, Enterprise Team and organization lists (administrators; served from `data/github/structure.json`, `?refresh=1` goes to GitHub) |
| `GET /v1/access/cache` | the state of the on-disk GitHub cache: fetch ages, per-scope member counts, truncation and errors (administrators; never returns member logins) |
| `POST /v1/access/cache/refresh` | refresh that cache now instead of waiting for the background loop (administrators) |
| `POST /v1/auth/local/login` | sign in as the local super administrator (`{username, password}`; the response reports `must_change_password`) |
| `POST /v1/auth/local/password` | change that password, and optionally the username (`{current_password, new_password, new_username?}`) |
| `POST /v1/auth/local/enabled` | enable / disable the local account (administrators) |
| `GET /v1/usage?days=&user_id=` | usage statistics: request count, tokens, error rate, latency, and the distribution by model / date / user |
| `GET /v1/config` / `PUT /v1/config` | read / update the configuration (administrators; hot reload + write-back to config.yaml) |
| `GET /v1/traces?offset=&limit=&date=&user_id=&session_id=&trace_id=` | a page of trace summaries — `{total, items, offset, limit, truncated}` — read straight off disk, filterable by date / user / session / trace-id fragment. Each summary carries `interaction_id` and `turn_count`, so a row that took several upstream calls says so. `limit` is clamped to 500; `truncated` says the date-directory cap was reached, so a shortened `total` is not mistaken for "that is all there is" |
| `GET /v1/traces/{id}` | one full interaction: the complete message chain, the single routing decision, the backend call, the model response, and `turns[]` — one entry per upstream call, with its tool calls, latency and tokens |
| `DELETE /v1/traces/{id}` | delete one trace, i.e. the whole interaction (administrators); 404 if it is already gone |
| `DELETE /v1/traces?date=&user_id=` | delete every trace matching the criteria, returning `{deleted: N}` (administrators). **At least one criterion is required** — an unfiltered call is refused with 422 rather than treated as a wipe-all |
| `GET /v1/router/decisions` | a legacy endpoint, kept for compatibility |
| `GET /healthz` | health check (including the loaded provider list) |

Which identity each endpoint requires is tabulated in
[the permission matrix](authentication.md#permission-matrix).
