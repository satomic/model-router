# Access control: who may sign in, who may create keys

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
