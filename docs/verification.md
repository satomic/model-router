# Verification scripts

They all live in [verify/](../verify/). The service has to be running first; the scripts read and
write `data/` directly to assemble an administrator session and a temporary API key (reusing the
first login in `auth.admin_logins` from `config.yaml`), so **no GitHub sign-in is needed**. Run them
from the repository root:

```powershell
python verify/verify_stub_upstream.py    # optional: a local OpenAI-compatible stub upstream (:8899) for
                                         # end-to-end verification without real credentials
python verify/verify_auth.py             # authentication, user_id attribution, multi-provider binding,
                                         # admin/normal-user separation
python verify/verify_storage.py          # layered trace storage and filtered queries
python verify/verify_enhanced.py         # full-chain traces, hot config reload, UI hosting, usage stats
python verify/verify_interaction.py      # one user interaction = one routing decision + one trace: an
                                         # agentic tool loop sharing an x-interaction-id, model
                                         # consistency, the complete chain, summed usage
python verify/verify_userrequest.py      # <userRequest> extraction
python verify/verify_prompt.py           # the AI decision prompt: rendering, validation, the preview
                                         # endpoint, and end-to-end effect (temporarily rewrites
                                         # ai_router and restores it)
python verify/verify_rules.py            # rule routing (switches strategy to rule itself and restores it)
python verify/verify_combined.py         # the rule-then-ai strategy: a matched rule decides and costs
                                         # no decision call, an unmatched request reaches the decision
                                         # model (switches strategy and disables stickiness for the
                                         # run, then restores both)
python verify/verify_localadmin.py       # the local super administrator: sign-in, the forced
                                         # password change, salted-scrypt storage, renaming,
                                         # disabling (temporarily rewrites auth.local_admin in
                                         # config.yaml and restores it)
```

Access control needs a real Enterprise administrator token, so it gets its own command. The token is
read from an environment variable and is **never written into any git-tracked file**; the script
temporarily rewrites `auth.key_policy` and restores it when it finishes:

```powershell
$env:GH_ENTERPRISE_TOKEN = 'ghp_...'     # needs admin:enterprise + admin:org
python verify/verify_access.py           # token validation, enterprise/organization/team discovery,
                                         # organization and team authorization, the administrator
                                         # exemption, fail-closed behaviour
python verify/verify_ghcache.py          # the on-disk GitHub cache: what a refresh writes, the
                                         # zero-call cache hit, the negative probe, and the cases
                                         # where a member list must NOT be trusted (backs up and
                                         # restores data/github/)
```

`verify/_bootstrap.py` puts the repository root on `sys.path` and switches the working directory;
`verify/verify_auth_helper.py` provides the shared authentication setup.

When verifying against the stub upstream, point a provider at `http://127.0.0.1:8899/v1` with
`api_type: openai` — which simultaneously verifies the "an OpenAI-compatible address that is not
Foundry" path.

A few test prompts in `verify/` are deliberately written in Chinese: they have to contain the
literal keywords configured in the live `config.yaml`'s `rules`, and keyword matching is a plain
substring test, so translating them would silently stop the rules from matching. Each such site
carries an inline comment saying so.

## Frontend gates

```powershell
cd frontend
npx tsc --noEmit                  # types
node scripts/check-locales.mjs    # all five catalogs must have identical key sets
npm run build                     # mandatory: FastAPI serves frontend/dist, so a stale bundle
                                  # is the likeliest way a change lands looking broken
```
