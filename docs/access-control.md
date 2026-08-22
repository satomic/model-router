# Access control: who may sign in, who may create keys, how far a key reaches

> Which **models** a caller may then use is a separate control with its own semantics (a union across
> scopes rather than a single verdict, and fail-open rather than fail-closed): see
> [model policy](model-policy.md).

Once GitHub OAuth is configured, **any** GitHub account can sign in (unless
`allow_any_github_user` is set to `false`, which then lets only administrators in). The real
authorization gate is therefore placed on **creating an API key** — without a key you cannot call
`/v1/chat/completions`, and so you cannot use Copilot BYOK. The verdict is based on GitHub's
membership data, queried through the GitHub REST + GraphQL APIs
([app/ghadmin.py](../app/ghadmin.py)) and answered from a local copy where possible
([app/ghcache.py](../app/ghcache.py), see [below](#the-local-github-cache)); the policy evaluation
lives in [app/keypolicy.py](../app/keypolicy.py).

On the "Access control → Key policy" page, enter and save a Personal Access Token carrying the
`admin:enterprise` scope, and the console **automatically fetches the enterprises visible to that
token**, together with each enterprise's **organizations** and **Enterprise Teams** — tick the ones
you want, no hand-written slugs or team ids. The corresponding `config.yaml`:

```yaml
auth:
  key_policy:
    enabled: true              # false = any account that can sign in may create a key
    github_token: 'ghp_...'    # the enterprise administrator PAT, needs admin:enterprise;
                               # stored only in config.yaml
    enterprises:
      my-enterprise:           # the enterprise slug
        enabled: true          # the enterprise master switch; off disables both entries below
        allow_all_orgs: false  # true = a member of any organization in this enterprise may create
        organizations: [org-a, org-b]   # allowed organization logins
        teams: ['14501973']             # allowed Enterprise Teams, by **numeric id** (not slug)
                                        # the UI always shows the team name, to admins and users
                                        # alike
```

The rules:

- **Administrators are exempt**, so a misconfigured policy cannot lock you out of your own service.
- With `enabled: false` no check runs at all, which is equivalent to being open to every account
  that can sign in.
- With `enabled: true` but no token configured, the service cannot verify membership and **rejects
  every non-administrator** key-creation request (fail-closed).
- When an enterprise's master switch is off, all its organization and team settings are inert.
- Matching any allowed organization or Enterprise Team passes; the checks are issued concurrently
  and short-circuit on the first hit.
- Answers come from the local cache where it can be trusted, and from GitHub otherwise; every
  in-process TTL cache is additionally invalidated the moment the policy or the token changes.

> **Why the enterprise switch means "any allowed organization/team inside the enterprise" rather
> than "a member of the enterprise"**: enterprise-level membership checks are unavailable on very
> large enterprises — GraphQL returns `RESOURCE_LIMITS_EXCEEDED` (even with a login filter) and the
> REST `consumed-licenses` endpoint 404s outright; on some enterprises even
> `/enterprises/{slug}/teams` 404s. Enterprise-level checks are therefore **three-state** (yes / no
> / cannot tell), "cannot tell" is treated as not passing, and authorization relies only on the two
> reliable paths: organizations and Enterprise Teams. Organization listing is capped at 200 pages of
> results and the `allow_all_orgs` fallback probe at 30, and **the UI states plainly when a list was
> truncated** — nothing is dropped silently.

## The local GitHub cache

Asking GitHub about one login at a time made every key creation, and every permission panel, wait on
several API round trips. So the enterprise / organization / Enterprise Team structure **and the
member lists of the scopes the policy references** are persisted under `data/github/` and refreshed
on a timer ([app/ghcache.py](../app/ghcache.py)):

| File | Contents |
|---|---|
| `structure.json` | the discovered enterprises with their organizations and Enterprise Teams — what `GET /v1/access/discover` serves |
| `members.json` | one entry per cached scope (`org:acme`, `team:my-enterprise/14501973`): the member logins, plus `truncated` / `error` / `fetched_at` |
| `probe.json` | individual live-probe results, positive and negative, with their own TTLs |
| `refresh.lock` | a best-effort lease, so N workers sharing `data/` do not all refresh at once |

Membership then costs **zero API calls** whenever it can: a set lookup in the cached list. The rules
about when the cache is *not* believed matter more than the speed-up:

- A list answers only when it is present, fetched under the current token, fresh, **not truncated and
  not errored**. Anything else falls through to a live check, because "not in the first 5000 logins I
  could read" is not "not a member", and reading it as one would deny a legitimate user their key.
- A live check's answer is written into `probe.json` — positives kept 10 minutes, **negatives only 2**.
  Short negatives are the point: someone just added to an organization gets in quickly, without every
  request re-probing them until the next full refresh.
- Every file records `token_fp`, a 12-character `sha256` prefix of the admin token — **never the
  token**. A token change therefore invalidates the whole cache, and changing the policy or the token
  through `PUT /v1/config` deletes the files outright rather than leaving stale lists authoritative.
- The refresh interval is `auth.key_policy.cache_refresh_seconds` (default 3600, minimum 60). The
  background loop starts a few seconds after startup — startup never blocks on GitHub — and a GitHub
  outage is logged per scope rather than killing the loop or emptying the cache.
- Timestamps are wall-clock `time.time()`, not `time.monotonic()`: a monotonic value is meaningless
  once written to disk and read back after a restart.

"Access control → Key policy" carries a cache card with the fetch ages, the per-scope member counts,
truncation and error badges, and a Refresh button. It reports counts only — a cache panel is the wrong
place to publish an organization's roster. Each row of evidence in the permission panel also names
its `source` (`cache` / `probe` / `live`), because a decision whose provenance is invisible is a
decision nobody can debug.

At the top of the "API keys" page, a normal user sees a "Key creation permission" panel: the verdict
(allowed / denied), the reason, how it was matched, and expandable per-check evidence (which
enterprises, organizations and teams were checked, and whether they are a member of each).
**Enterprise Teams are shown by team name** (e.g. `team-for-copilot`) rather than the numeric id
stored in the configuration — an id means nothing to a user and does not say which team it is; the
numeric id is attached after the name as secondary information (`#14501973`) so an administrator can
cross-check it on GitHub. Team names are resolved by the backend from its cached team list when the
verdict is returned; a failed resolution falls back to showing the id and never affects the
authorization decision itself. When denied, the create button is disabled and explained — **not
belonging to any allowed Enterprise / Organization means no key, and therefore no BYOK** — and the
evidence in the panel can be sent straight to an administrator as an access request.

## Who may narrow a key's scope

A key can be **restricted** to particular models or particular connection types, so that one key
reaches only `gpt-4o-mini` on `/v1/chat/completions` while another reaches everything. That
restriction is called the key's **scope**, and it can only ever subtract: the effective set is
`model policy of the owner ∩ scope of the key`, so a scope grants nothing that the owner did not
already have (see [model policy](model-policy.md), and `app/keyscope.py` for the intersection).

Which is why the control on it is a **cost** control, not a security one. A user who scopes their
key to the single most expensive model has pinned every request on that key to it, and the whole
point of the router, sending cheap work to a cheap model, stops applying to that key. So narrowing
is a permission an administrator grants, and it is **off by default**. Every key then covers all
models and all connection types, which is exactly what every key did before scopes existed, so the
default takes nothing away from anybody.

The permission is configured on "Access control → Key scope", and it is evaluated in
[app/scopepolicy.py](../app/scopepolicy.py):

```yaml
auth:
  key_scope_policy:
    enabled: false                # false (the default) = nobody may narrow a key
    users: [alice, bob]           # GitHub logins
    teams: ['my-enterprise/14501973']   # '<enterprise slug>/<team id>'
    organizations: [org-a]        # organization logins
```

The rules, which are also printed on the page itself because one of them is not guessable from
three tables:

- **The levels are combined with AND**, not OR. With both `users` and `organizations` filled in,
  a caller must be on the user list *and* in one of the listed organizations. This is the opposite
  of the key-creation policy above, which is an OR,
  and deliberately so: creating a key is the capability being handed out there, while here every
  additional list is an administrator narrowing who may spend.
- **A level left empty abstains** rather than denying. Otherwise listing one organization on its
  own would match nobody at all, because no login is a member of an empty user list, and the
  feature would look broken. So filling in only `organizations` allows everybody in it.
- **Within one level, any single match is enough.**
- `enabled: false`, the default, denies everybody. `enabled: true` with all three lists empty also
  denies everybody, fail-closed, the same posture as an enabled key policy with no token; the
  console says so with a "Grants nobody" badge rather than letting it read as "allow all".
- **Administrators are exempt**, and an administrator editing somebody else's key exercises their
  own permission, not that person's.
- A membership lookup that cannot be answered denies that level. Failing closed here costs a user
  a restriction they wanted; failing open would cost money.

Two things stay allowed regardless of the verdict. **Widening** a key back to "everything its owner
may reach" is always permitted, because it is the documented default, it carries no cost risk, and
refusing it would trap an already narrowed key in place the moment the permission was withdrawn.
And **keys that already carry a restriction keep it**: taking the permission away stops new
restrictions, it does not rewrite keys already issued.

Enforcement is in the backend, on both `POST /v1/keys` and `PATCH /v1/keys/{id}`, which answer
`403` with the reason naming the level that failed. The console reads the same verdict from
`GET /v1/access/me` (as `key_scope`) and hides the scope editor rather than offering an action the
server will refuse, but hiding it is a courtesy: the gate is the endpoint.

### The three lists only offer accounts that may create a key

Narrowing is something you do to a key you own, so the permission is meaningless for anybody who
cannot create a key in the first place. The three tables on "Access control → Key scope" therefore
list only the users, teams and organizations that the **key policy** above allows to create an API
key, exactly as the model policy bindings tables do, and each table says how many entries it left
out and why. With the key policy switched off the gate is open, so every discovered scope is
offered.

The two halves of that filter are answered in different places, because they are different
questions. Teams and organizations are decided by the saved `auth.key_policy` lists, so the console
computes them in the browser from the draft it is editing, through the rule shared with the model
policy page in [frontend/src/keyeligibility.ts](../frontend/src/keyeligibility.ts): edit the key
policy in the tab next door and these tables follow immediately, before a save. A **user** cannot be
decided that way. `key_policy` has no user list at all; it gates on Enterprise, Organization and
Team membership, so whether one login may create a key is a membership question only the server can
answer. `GET /v1/access/users?eligibility=1` answers it by running the same
[app/keypolicy.py](../app/keypolicy.py) evaluation the Keys page shows that user about themselves,
against the **saved** policy, which is why the users table asks you to save a key-policy edit before
reading its verdict column. Cached member lists make this cheap; the request evaluates at most 200
logins, and any beyond that are marked unknown rather than dropped.

Three postures in that filter are deliberate. An **unknown** verdict is offered, not hidden: "we
could not establish it" and "not allowed" are different answers, and a table that hid the first
would quietly shrink during a GitHub outage. An entry that is **already on the allow list** survives
the filter and stays visible with its reason, so a permission granted before the key policy tightened
can still be seen and cleared. And the dropped count is always printed, so a filtered list never
reads as the whole list.

## Both verdicts in the reader's language

The two policy modules are English-only on purpose. Their sentence is the **record**: it is what
goes into the server log, into the `403` body of `POST /v1/keys` and `PATCH /v1/keys/{id}`, and into
the response an API caller parses, all places where the reader's locale is unknown and must not
change what was written down. The console, however, is translated into five languages, and a page
that mixes a Japanese layout with an English verdict is a page that has not been translated.

So each verdict carries both. Beside `reason` (the English sentence) sits `reason_code`, which names
the same verdict without saying it in any language, and `reason_params`, the values that sentence
interpolates:

```json
{
  "allowed": true,
  "reason": "You are a member of organization nekoaru (enterprise satomic), so you can create API keys.",
  "reason_code": "memberOrganization",
  "reason_params": { "name": "nekoaru", "enterprise": "satomic" }
}
```

The console renders the code through its own catalogs
([frontend/src/reasons.ts](../frontend/src/reasons.ts)) and falls back to `reason` for a code it does
not recognise, so an older console against a newer server degrades to English rather than to a blank
line. The codes are `admin`, `policyOff`, `noToken`, `noEnterprise`, `memberOrganization`,
`memberTeam`, `memberEnterprise`, `member` and `noMembership` for key creation, and `admin`, `off`,
`nobodyAllowed`, `levelsFailed` and `allowed` for the scope permission.

Two details in that contract are deliberate. Membership gets **one code per kind** rather than one
code carrying a kind, because "organization *X*" is not a noun phrase every language builds the same
way. And `levelsFailed` passes the failing levels as bare names (`{"levels": ["user", "team"]}`)
rather than as the joined English phrase, because only the console knows what those levels are called
in the reader's language and in which order that language lists them; it joins them with
`Intl.ListFormat`, where the separator is not a comma everywhere.

The failure mode this creates is a new backend branch whose code nobody translated: it falls back to
English **silently**, which no assertion about the English text can see.
[verify/verify_keyscope_policy.py](../verify/verify_keyscope_policy.py) therefore provokes every
branch of both modules, checks that each returns its own code, and cross-checks the full set of codes
against all five catalogs.
