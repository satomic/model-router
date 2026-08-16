# Full-chain logging

Every **user interaction** produces one trace file:
`logs/traces/<date YYYY-MM-DD>/<user_id>/<trace_id>.json` ([app/traces.py](../app/traces.py)).
`user_id` is the GitHub login of the API key's owner (sanitized for path traversal), so the
directories are naturally split by real user.

## One interaction is one record, not one per request

One file is not one HTTP request. As described under [Router logic](router-logic.md), an agentic
client answers a single user question with a loop of requests. All of them carry the same
`x-interaction-id`, so each is recorded as a **turn** and folded into the one record:

- `request.messages` always holds the **complete final chain** — every assistant `tool_calls`
  message and every `tool` result the client replayed — so the record is the whole conversation
  rather than a fragment of it.
- `routing` appears **once**: the model was chosen once for the whole interaction, and a second
  decision block would be a decision that never happened.
- `turns[]` has one entry per upstream call, each with its own timestamp, `initiator`
  (`user` for the question, `agent` for the loop's follow-ups), `message_count`, latency, `usage`,
  and the `tool_calls` the model asked for on that turn. Those were previously invisible: the
  assistant message carrying them only ever showed up in the *next* request's replayed messages, and
  the final turn asks for no tools, so a trace read as though none had been requested at all. They
  are now captured on both the streaming path (assembled by delta index, since the name arrives on
  the first fragment and the arguments accumulate over later ones) and the non-streaming one.
- A turn stores no copy of the chain it sent when that chain is a prefix of the final one — it is
  reconstructible from `message_count`. A client that *rewrote* history instead of appending breaks
  that, so such a turn keeps its own `messages` and is flagged `rewritten`.
- `usage` and `total_ms` at the top level are the **interaction's** totals, summed over the turns:
  each request really did send the whole replayed chain upstream and really was billed for it, so
  the cost of the interaction is the sum rather than the last turn's figure. `response.content` and
  `finish_reason`, by contrast, come from the closing turn — that is the answer the user read.
- `turn_count` counts every turn that happened; a runaway tool loop is capped at 200 stored turns
  and says so with `turns_truncated` rather than quietly dropping them.

## Reading traces in the console

The call-trace page lists every trace it can reach on disk, filterable by date, by user
(administrators only — a normal user's value is overwritten server-side, so the box is not offered)
and by trace-id fragment, paging with a "load more" rather than a fixed window. One row is one
**user interaction**, and its "Calls" column shows `×N` when that interaction took several upstream
calls. The detail pane appears only once a row is selected, the split is draggable and remembered
(70/30 by default), and request/response payloads render as a colored, collapsible JSON tree in both
themes. A multi-call interaction also gets a **Call chain** panel: one collapsible row per upstream
call, showing its initiator, model, latency, tokens and the tool calls the model asked for.
Administrators get a per-row delete and a "delete filtered" action whose confirmation quotes the
exact number of traces it will remove.

## How the listing stays cheap

Because date and user are **path segments**, `GET /v1/traces` filters on them by *selecting
directories* rather than scanning: a page of results costs one `stat()` per candidate file plus one
read per row actually returned, so the listing sees the whole tree on disk instead of a recency
window. Ordering is by `(date directory, file mtime)` rather than the `ts` inside each file — mtime
is stamped when the trace is written, i.e. at the end of the very request its `ts` opens, so the two
orders agree and sorting costs no reads. `/v1/usage` scans the date directories directly to
aggregate. The in-memory index of the 500 most recent summaries remains only for the legacy
`/v1/router/decisions` endpoint.

## What a trace contains

- **user_id / api_key_id / api_key_name / session_id / interaction_id / client_ip**: the origin
  identifiers. `user_id` comes from the API key and no longer relies on what the client claims, so
  even Copilot BYOK requests are attributed accurately
- **request**: the original request headers (`authorization` / `api-key` / `x-api-key` / `cookie`
  and other sensitive headers are redacted), the complete messages, and the request parameters
- **routing**: the chosen model, the reason, the decision latency, and the decision analysis —
  in rule mode, the evaluation result of every rule and the keyword that matched; in AI mode, the
  system prompt actually sent this time (`decision_system` — the prompt is editable, so each call is
  archived), the input given to the decision model, its raw output, the rationale, the latency and
  the token consumption; for a sticky hit, a note about the binding
- **backend**: the provider name and address, `api_type`, the real deployment name, the API flavour
  (chat/responses), the payload sent after parameter adaptation, and the backend latency (never any
  credentials)
- **response**: the complete response content (a streaming request is aggregated and recorded once
  the stream ends), `finish_reason`, the `tool_calls` the model requested, and `usage` (summed over
  the interaction's turns)
- **turns / turn_count**: the per-upstream-call breakdown described above; a single-request
  interaction has exactly one turn
