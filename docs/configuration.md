# Configuration

All configuration (including credentials) lives in `config.yaml`, which is **gitignored** (it never
enters the repository); see [config.example.yaml](../config.example.yaml) for the template. Edit the
text directly or change it from the console (comments are preserved on write-back).

The console splits the configuration into two top-level pages: **Routing configuration**
(`providers` / `models` / `strategy` / `session` / `ai_router` / `rules`) and **Access control**
(`auth`). Each has a sticky save bar below its sub-page navigation — **when there are unsaved
changes the whole bar turns blue and a dot appears on the affected sub-page tabs**, prompting you to
press "Save and apply"; "Discard changes" reloads from `config.yaml`. Each page writes back only the
top-level sections it owns (the backend merges by top-level key), so an unsaved draft on one page
cannot overwrite what the other page just saved.

The 4 sub-pages of "Routing configuration":

| Sub-page | Section | Notes |
|---|---|---|
| Backend connections | `providers` / `default_provider` | address, key, `api_type`, `api_version` |
| Model catalog | `models` | the `provider` binding, the upstream `model_name`, `description` for the AI decision to reason about, and the `default` / `reasoning` / `api: responses` flags |
| Routing strategy | `strategy` / `session` / `ai_router` | `rule` or `ai`; the stickiness toggle and capacity (one decision per interaction / per session); the decision model, its provider, timeout and prompt truncation length; the **AI decision prompt** (`ai_router.decision_prompt`) editor plus a preview rendered against the real model catalog |
| Rule routing | `rules` | keywords / prompt length, matched in order |

The 3 sub-pages of "Access control" (see [access control](access-control.md)):

| Sub-page | Section | Notes |
|---|---|---|
| Administrators and sign-in | `auth.admin_logins` / `auth.allow_any_github_user` | the administrator list and who is allowed to sign in |
| GitHub OAuth | `auth.github` | Client ID / Secret / callback URL, **editable in the UI** (getting it wrong locks everybody out, and only editing `config.yaml` gets you back) |
| Key policy | `auth.key_policy` | the Enterprise administrator token, and control over who may create API keys by Enterprise / Team / Organization |

`.env` still works as a compatibility fallback: when `providers` is missing, a `foundry` connection
is synthesized from the `AZURE_OPENAI_*` variables.
