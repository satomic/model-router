# API

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible, streaming supported. **Requires an API key**; optional `x-interaction-id` (one decision and one trace per user interaction) and `x-session-id` (session stickiness); response headers include `x-trace-id` (the *interaction's* trace) / `x-routed-model` / `x-router-reason` / `x-router-decision-ms`, plus `x-router-interaction-id` when the request carried one |
| `POST /v1/messages` | Anthropic Messages-compatible, streaming supported, so a client configured with `ANTHROPIC_BASE_URL` pointing here works unchanged. **Requires an API key**, accepted from either `x-api-key` or `Authorization: Bearer` because the two ecosystems send different headers. Same routing, same stickiness headers and same response headers as `/v1/chat/completions`; the body's `model` is ignored, since the router picks the model |
| `GET /v1/models` | the available backend models (requires an API key), **narrowed to what the key's owner may use** under the [model policy](model-policy.md) and then by that key's own [scope](#api-key-scopes) |
| `GET /v1/models/available` | the signed-in user's effective model list and why it looks that way: the resolved names, the catalog descriptions, the default model, a `reason`, and the grants that applied. Resolved through the same code the API path uses, so it cannot disagree with a real request. Available to **every** signed-in user, and returns no trace of the teams or organizations they are not in |
| `GET /v1/auth/status` | what the frontend uses to choose between the setup page, the sign-in page and the console |
| `POST /v1/auth/setup` | the first-run wizard (only while unconfigured + from the local machine) |
| `GET /v1/auth/github/login` · `GET /v1/auth/github/callback` · `POST /v1/auth/logout` | the GitHub OAuth flow |
| `GET/POST /v1/keys`, `PATCH/DELETE /v1/keys/{id}` | list / create (returns the one-time plaintext, subject to the key policy) / disable / delete API keys. `POST` takes an optional `scope`; `PATCH` accepts `disabled` and `scope` independently, so saving one does not clear the other |
| `GET /v1/access/me` | whether the signed-in user may create a key: the verdict, the reason, and the per-check evidence; plus `key_scope`, the separate verdict on whether they may [narrow a key](#api-key-scopes). Both verdicts also carry `reason_code` and `reason_params`, the same answer machine-readably, so a console can say it in the reader's language; `reason` stays the English record used by the logs and the `403` bodies (see [access control](access-control.md#both-verdicts-in-the-readers-language)) |
| `GET /v1/access/token` | the status of the Enterprise administrator token (administrators; returns only a mask and the owner, **never echoes the plaintext**) |
| `POST /v1/access/verify-token` | validate a token and report its scopes (administrators) |
| `GET /v1/access/discover?refresh=` | automatically fetch the enterprise, Enterprise Team and organization lists (administrators; served from `data/github/structure.json`, `?refresh=1` goes to GitHub) |
| `GET /v1/access/cache` | the state of the on-disk GitHub cache: fetch ages, per-scope member counts, truncation and errors (administrators; never returns member logins) |
| `POST /v1/access/cache/refresh` | refresh that cache now instead of waiting for the background loop (administrators) |
| `GET /v1/access/users` | every login that has signed in at least once, with first / last sign-in, a count and the model group currently bound to it (administrators). Read from `data/known_users.json` rather than the session table, because expired sessions are purged, so sessions can only answer "who is signed in right now" |
| `POST /v1/auth/local/login` | sign in as the local super administrator (`{username, password}`; the response reports `must_change_password`) |
| `POST /v1/auth/local/password` | change that password, and optionally the username (`{current_password, new_password, new_username?}`) |
| `POST /v1/auth/local/enabled` | enable / disable the local account (administrators) |
| `GET /v1/usage?days=&user_id=` | usage statistics: request count, tokens, error rate, latency, and the distribution by model / date / user |
| `GET /v1/config` / `PUT /v1/config` | read / update the configuration (administrators; hot reload + write-back to config.yaml) |
| `GET /v1/traces?offset=&limit=&date=&user_id=&session_id=&trace_id=` | a page of trace summaries, `{total, items, offset, limit, truncated}`, read straight off disk, filterable by date / user / session / trace-id fragment. Each summary carries `interaction_id` and `turn_count`, so a row that took several upstream calls says so. `limit` is clamped to 500; `truncated` says the date-directory cap was reached, so a shortened `total` is not mistaken for "that is all there is" |
| `GET /v1/traces/{id}` | one full interaction: the complete message chain, the single routing decision, the backend call, the model response, and `turns[]`, one entry per upstream call with its tool calls, latency and tokens |
| `DELETE /v1/traces/{id}` | delete one trace, i.e. the whole interaction (administrators); 404 if it is already gone |
| `DELETE /v1/traces?date=&user_id=` | delete every trace matching the criteria, returning `{deleted: N}` (administrators). **At least one criterion is required**: an unfiltered call is refused with 422 rather than treated as a wipe-all |
| `GET /v1/router/decisions` | a legacy endpoint, kept for compatibility |
| `GET /healthz` | health check: the loaded provider list, plus the running `version` and the project links the console's header shows |
| `GET /v1/release` | the last answer from the background release check: `{current_version, latest_version, update_available, release_url, published_at, checked_at, error}` (public). Reads a cached result, so it never calls GitHub |
| `POST /v1/release/check` | ask GitHub for the latest release now instead of waiting for the daily check (administrators, since it makes an outbound request) |

Which identity each endpoint requires is tabulated in
[the permission matrix](authentication.md#permission-matrix).

## Both protocols reach every model

`/v1/chat/completions` and `/v1/messages` are two front doors onto the same router. Which one a
client uses has no bearing on which models it can reach: the router converts between the OpenAI
chat-completions shape and the Anthropic Messages shape at both edges, so an OpenAI-style client can
be answered by a Claude endpoint and an Anthropic-style client by an Azure deployment, streaming
included. Each trace turn records `client_protocol` (the door the request came in) and `protocol`
(what the backend spoke), so a conversion is visible after the fact. Details and the exact field
mapping: [Backend connections](providers.md).

## API key scopes

A key carries a `scope` describing what it may reach, and it is composed with the owner's model
policy by **intersection**: `effective = policy(owner) ∩ scope(key)`. A scope can only narrow, so it
is never a way around the policy, and a key cannot outlive its owner's permissions: revoke a model
group from the user and every one of their keys loses it on the next request.

```json
{"kind": "all"}
{"kind": "api_types", "api_types": ["anthropic", "azure"]}
{"kind": "models", "models": ["gpt-4o", "claude-sonnet-5"]}
```

`api_types` is stored as a rule rather than as the models it matched at creation time, so a model
added to an `anthropic` connection later is picked up without editing the key. `models` is validated
against the owner's own available models when it is written, so a scope cannot name something its
owner could not reach in the first place.

Setting any scope other than `{"kind": "all"}` is itself a **permission**, granted per user, team and
organization under `auth.key_scope_policy` and off by default, because a key pinned to one expensive
model defeats the routing that keeps costs down. `POST /v1/keys` and `PATCH /v1/keys/{id}` answer
`403` with the failing level named when the caller does not have it. Widening a key back to
`{"kind": "all"}` is always accepted, and administrators are exempt. The verdict a console can read
ahead of time is `key_scope` on `GET /v1/access/me`; the semantics are in
[Access control](access-control.md#who-may-narrow-a-keys-scope).
