# Model policy: which models each caller may use

The key policy answers "may this person have a key at all". The **model policy** answers the next
question: given that they have one, which models does it reach? It is a distribution control — a way
to hand every scope a curated, deliberately small list rather than the whole catalog — and not a
privilege boundary, which is why **administrators are exempt from it**, exactly as with the key
policy.

Resolution lives in [app/modelpolicy.py](../app/modelpolicy.py); the console page is **Model policy**, a top-level
entry under Management.

## Model groups

A **model group** is a named, reusable list of models drawn from the catalog. Groups are independent
of scope: the same group can be granted to a user, a team and an organization. In the console you
build one by ticking checkboxes against the model catalog, so there are no names to retype and no way
to name a model that does not exist.

```yaml
model_groups:
  starter: [gpt-4o]              # only the cheapest model
  full: [gpt-4o, gpt-5.4]        # everything
  locked: []                     # an empty group: grants nothing
```

**An empty group is legal, and load-bearing.** It is how you say "this scope grants nothing", which
is what makes the arrangement "a newly signed-in user gets nothing until an administrator assigns
them something" expressible at all. Without it, the only way to express that would be to leave the
scope unconfigured — and an unconfigured scope means the opposite (see below).

A group naming a model that is not in the catalog is rejected by validation with a `422`, so a group
can never appear to grant something it cannot. Deleting a model from the catalog in the console prunes
it out of every group in the same save for that reason.

## Scopes, and how they resolve

```yaml
model_policy:
  enabled: false
  default_group: ''            # every signed-in user starts here, e.g. starter (or locked)
  users: {}                    # {github-login: group}
  teams: {}                    # {'<enterprise slug>/<team id>': group}
  organizations: {}            # {organization login: group}
```

A caller's effective list is the **union** of the default group and every group bound to a user, team
or organization they belong to. Union rather than precedence is the whole design:

- **A binding can only ever widen a list, never narrow it.** So there is no ordering to configure, no
  precedence table to reason about, and adding a grant can never take a model away from somebody.
- **An empty group contributes nothing**, so the union is empty only when *every* contributor is
  empty. That is the case that produces a caller with no models.
- The `teams` key is `'<enterprise slug>/<team id>'` — the form the local GitHub cache needs to
  resolve a team. The numeric ids are listed under "Access control → Key policy" once an enterprise
  administrator token is configured.

Three states are deliberately different, and the distinction is what keeps the switch safe to turn
on:

| State | Effect |
|---|---|
| `enabled: false` | No restriction at all. This is the default, so upgrading changes nothing. |
| `enabled: true`, nothing bound to the caller | **Still unrestricted.** Turning the switch on before filling in the tables cannot lock anybody out, including the operator doing it. |
| `enabled: true`, bound only to empty groups | No models. Requests are refused with a `403` that names the reason. |

`default_group` exists as its own field precisely so "not configured" and "configured to grant
nothing" can be told apart: leave it empty and an otherwise-unbound account is unrestricted; point it
at an empty group and every new account starts with nothing.

**Per-scope membership lookups fail open.** Under a union, a failure narrows a list rather than
widening it, so a transient GitHub error must not silently take models away from somebody who was
granted them. This is the opposite of the key policy's fail-closed posture, and for the opposite
reason: there, a failure that passed would grant access.

## How it is enforced

Enforcement is by **narrowing the catalog**, not by adding a check to each routing path.
`RouterConfig.restricted_to(names)` returns a shallow view of the configuration whose `models` dict
holds only the permitted entries, and the routing code runs unchanged against that view:

- `match_rules` already skips a rule whose model is not in the catalog, so a rule pointing at a
  model the caller may not use is passed over rather than obeyed.
- `route_by_ai` only ever offers `list(cfg.models)` as candidates, so the decision model is never
  told about a model it must not pick, and an unparseable answer falls back within the same view.
- `default_model` reads the same dict, so the fallback is permitted by construction.
- A sticky session binding naming a model the caller may no longer use is dropped and re-decided,
  rather than kept because it was decided while the old policy was in force.

One narrowing therefore closes every path at once, including any path added later. The view is built
per request and never written into the module-level configuration, which is shared by every
concurrent request.

Two endpoints are affected directly: `GET /v1/models` returns only the permitted models, and
`POST /v1/chat/completions` refuses with a `403` naming the policy reason when the effective set is
empty. Attribution is unchanged — Copilot BYOK sends no identity, so the caller is the API key's
owner; API-key records carry only the owner's login, so the administrator exemption is derived from
the configuration rather than stored on the key.

## What a user sees

The **"Available models"** page in the sidebar is not admin-gated: the point of a curated list is
that people can see their own without asking. It calls `GET /v1/models/available`, which resolves the
policy through the same code the API path uses, so the page cannot claim a model a real request would
then refuse. It shows the effective list with each model's description and which one an unrouted
request would land on, the reason the list looks the way it does, and a table of the grants that
applied.

**Only the grants that actually applied are returned.** Listing the teams and organizations a user is
*not* in would publish the policy tables to every signed-in user, so the response contains no trace
of them.

## Who has signed in

Binding a group to a user means knowing their login, and asking somebody to spell it out is a poor
substitute for a list. Sessions cannot answer "who has signed in", because expired sessions are
purged — they only know who is signed in *right now*. So `data/known_users.json` is written from
inside `create_session` itself: one record per login, with the first and last sign-in and a count.
`GET /v1/access/users` serves it to administrators, and the console renders it as a table on the
Model policy page with a group picker on each row.
