# Copilot AI credits: pool monitoring and the BYOK gate

Every GitHub Copilot Business / Enterprise seat comes with a monthly allowance of **AI credits**
that is pooled across the enterprise and **forfeited at the end of the month**. A user who routes
all of their work through this service on BYOK spends the customer's Azure / OpenAI bill while the
credits they already paid for expire unused.

The **AI credits** page (administrators only, under *Management*) closes that gap:

1. It **polls GitHub on a cron schedule** with the enterprise administrator token already configured
   for the key policy, and works out the state of each enterprise's shared pool: size, used, remaining.
2. With the **BYOK gate** on, a request from a user who **belongs to** an enterprise whose pool still
   has credits is **neither routed nor sent to the decision model**. It is answered with a short note
   (a normal assistant message) telling them to use Copilot's built-in models first. The gate lifts by
   itself once the pool is exhausted.
3. Optionally the gate is **per user**: the caller's own GitHub user-level budget takes precedence --
   headroom left means "use Copilot" even if the pool is already exhausted, budget used up means GitHub
   blocks them and BYOK stays open.

## What GitHub's API can and cannot tell us

This was verified live against three enterprises (September 2026). The API documents none of the
pool arithmetic; the derivation below is what the module relies on.

| Question | Endpoint | Verdict |
|---|---|---|
| How much of the pool is used this month? | `GET /enterprises/{slug}/settings/billing/usage/summary?product=copilot` | **Yes.** The SKUs with `unitType: ai-units` (`copilot_ai_unit`, `coding_agent_ai_unit`) are the pool. `discountQuantity` is what the pool covered, `netQuantity` is metered overage. |
| Is the pool exhausted? | same | **Yes.** `netQuantity > 0` on any ai-unit SKU means the pool is used up. |
| How big is the pool? | same + `GET /enterprises/{slug}/copilot/billing/seats` | **Partly.** GitHub caps `discountQuantity` at the pool size, so once anything is metered `sum(discountQuantity)` *is* the exact size. Before that it is **estimated from the seat list** (3,900 credits per Enterprise seat, 1,900 per Business seat). The seats endpoint **404s on very large enterprises**; the size is then unknown until exhaustion, but exhaustion itself is still detected. |
| Per-user consumption? | `GET .../settings/billing/ai_credit/usage?user=login` | Yes, but one call per user and it says nothing about the user's *limit*. Not used. |
| Per-user limit and headroom? | `GET .../settings/billing/budgets` + `.../budgets/{id}/user-states` | **Yes.** User-level budgets on the `ai_credits` SKU are the only per-user limit GitHub enforces. Amounts are USD (1 credit = $0.01). The universal budget's `user-states` lists every user who consumed this cycle with their effective target and consumption; individual user budgets fill in the rest. |
| A "remaining credits" figure? | — | **No.** Nothing publishes it; the module derives it as `size − used`. |

Two consequences shape the behaviour:

- **The gate is fail-open.** An unknown pool size with nothing drawn yet, a snapshot from a different
  token, a stale snapshot (older than two schedule intervals, never less than 15 minutes), or a user
  GitHub itself blocks — every uncertainty resolves to *let the request through*. The gate protects a
  budget; it must never be the reason a developer cannot work.
- **Billing figures lag.** GitHub's usage report trails real consumption by up to a few hours, so
  polling more often than every 15 minutes buys nothing, and **Lift the gate when fewer than N credits
  remain** exists so the gate can open a little before GitHub starts metering.

## Configuration

Everything is on the console page; this is what it writes to `config.yaml`:

```yaml
ai_credits:
  enabled: false                # poll GitHub on the schedule below
  schedule: '*/30 * * * *'      # five-field cron, evaluated in UTC
  enterprises: []               # slugs to poll; empty = every enterprise the token administers
  min_remaining_credits: 0      # treat the pool as used up while this many (or fewer) remain
  gate:
    enabled: false              # answer with the note instead of routing while the pool has credits
    per_user: false             # also honour each user's seat and user-level budget
    message: ''                 # empty = the built-in English note (pool has credits)
    message_budget: ''          # empty = the built-in English note (own budget has headroom)
```

The cron parser accepts the usual subset: `*`, `N`, `A-B`, `*/S`, `A-B/S`, comma lists, three-letter
month and weekday names, `0` and `7` both meaning Sunday. The page previews the next five firings
(rendered by the backend, so what is previewed is what will run) and offers presets from every
15 minutes to daily.

The pool note supports placeholders: `{enterprise}`, `{slug}`, `{remaining}`, `{total}` and
`{remaining_note}` (e.g. ` (about 1,200 of 42,800 credits)`, empty when the size is unknown). The budget
note additionally takes `{budget_remaining_usd}`, `{budget_total_usd}` and `{budget_remaining_credits}`.
Both built-in defaults are in English; set `gate.message` / `gate.message_budget` to localise them.

### Which enterprise is the caller's?

The gate never sends a user to a pool that is not theirs. For each polled enterprise it asks whether the
caller **belongs** there, in this order:

1. a user-level budget record for the login exists (they consumed credits there) -- yes;
2. GitHub returned a seat list -- the login is a member iff it holds a seat;
3. no seat list (large enterprises) -- the key policy's cached organization / team member lists for
   that enterprise decide; a login none of them mention is **unknown**, not absent.

Unknown membership lets the request through. Each enterprise card shows where its membership test
draws from (`members: N from seat list` / `from policy cache` / `unknown`).

### Enterprise mode vs per-user mode

| `gate.per_user` | Verdict for a caller who belongs to the enterprise |
|---|---|
| off | Sent back to Copilot iff the enterprise pool is `available` (and above `min_remaining_credits`). |
| on, caller has a user-level budget | Their **own budget decides**: headroom left -> sent back to Copilot with the budget note, even if the pool is exhausted (the budget is pre-approved Copilot spend); budget used up -> GitHub blocks them, BYOK stays open. |
| on, caller has no user-level budget | Falls back to the pool test above. |

With several enterprises polled, the first one that yields a verdict wins.

## Request path

The gate is the **first** thing `_prepare_call` does after the API key resolves its owner — before the
model policy, the key scope and the routing decision — so a gated request costs nothing on any
upstream. The answer is a normal `200` in the caller's protocol and streaming mode: OpenAI or
Anthropic, streamed or not. A `4xx` would surface in Copilot as a bare failure; a message is read.

A trace is still recorded, with `ai-credits-gate` in both the model and reason columns and an
analysis note naming the enterprise, the reason (`pool` or `budget`) and the remaining credits or
budget headroom, so *"why did nobody route through the router this morning"* has an answer in the
trace list. Administrators are gated like everyone else:
this is about the customer's budget, not a privilege boundary.

## Operations

- The snapshot lives in `data/ai_credits.json`; every worker reads the same file, and a restart does
  not start blind. `data/ai_credits.lock` is the best-effort lease that keeps N workers from polling
  at once. The token is never stored — a fingerprint detects a token change and a snapshot fetched
  under another token's visibility is ignored until the next poll.
- **Refresh now** polls immediately regardless of the schedule and returns the fresh figures in the
  same response.
- Changing the enterprise token under *Access control → Key policy* drops the snapshot.
- Endpoints (administrators): `GET /v1/credits`, `POST /v1/credits/refresh`,
  `POST /v1/credits/schedule/preview`. See [API](api.md).
- Checks: `python verify/verify_ai_credits.py` covers the cron parser, the pool derivation, and the
  gate's decisions including per-user mode.
