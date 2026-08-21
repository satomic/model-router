# Architecture and data flow

Diagrams describing what the Model Router is made of, what happens to a request as it passes
through, and how the routing strategies decide. They are written in [Mermaid](https://mermaid.js.org/),
so GitHub renders them inline and any Markdown viewer with Mermaid support does too.

Each diagram names the module it describes, so a diagram that has drifted from the code can be
checked against it. The narrative version of the request path is [Router logic](router-logic.md);
this document is the map, that one is the walkthrough.

- [1. The system at a glance](#1-the-system-at-a-glance)
- [2. The request path](#2-the-request-path)
- [3. The routing strategies](#3-the-routing-strategies)
- [4. Which models a caller may use](#4-which-models-a-caller-may-use)
- [5. Identity and authorization](#5-identity-and-authorization)
- [6. Persistent state and background tasks](#6-persistent-state-and-background-tasks)

## 1. The system at a glance

One FastAPI process. It serves the built React console from `/`, an API under `/v1` that speaks
both the OpenAI chat-completions protocol and the Anthropic Messages protocol, and keeps every byte
of its own state in a single `data/` directory. Nothing else is required to run it: no database, no
cache server, no queue.

```mermaid
flowchart LR
    subgraph clients["Clients"]
        copilot["GitHub Copilot BYOK<br/>and any OpenAI-compatible client"]
        anth["Claude Code<br/>and any Anthropic-compatible client"]
        console["Administrators and users<br/>in a browser"]
    end

    subgraph process["Model Router process (FastAPI, app/)"]
        spa["React console served from /<br/>frontend/dist"]
        api["Dual-protocol API<br/>/v1/chat/completions, /v1/messages, /v1/models"]
        conv["wire.py<br/>OpenAI to Messages and back,<br/>streaming included"]
        mgmt["Management API<br/>/v1/config, /v1/keys, /v1/traces, /v1/usage"]

        subgraph decide["Decision layer"]
            keys["auth.py + authstore.py<br/>API keys, sign-in sessions"]
            mpol["modelpolicy.py<br/>which models this caller may use"]
            router["routing.py<br/>rule / ai / rule-then-ai"]
            sticky["sessions.py<br/>one decision per interaction"]
        end

        subgraph gate["Access control"]
            kpol["keypolicy.py<br/>who may create an API key"]
            cache["ghcache.py<br/>cached GitHub structure and members"]
            ghapi["ghadmin.py<br/>GitHub REST and GraphQL"]
        end

        pool["providers.py<br/>ClientPool, one client per provider"]
        cfgmod["config.py<br/>RouterConfig, validation, hot reload"]
        tracemod["traces.py<br/>full-chain records"]
        rel["release.py + version.py<br/>update check"]
    end

    subgraph state["Persistent state (section 6)"]
        files[("data/<br/>config.yaml, api_keys.json,<br/>auth_sessions.json, known_users.json,<br/>release.json, github/, logs/traces/")]
    end

    subgraph upstream["External services"]
        foundry["Azure AI Foundry<br/>and any OpenAI-compatible endpoint"]
        claude["Anthropic, Databricks Claude,<br/>and any Messages-API endpoint"]
        github["github.com<br/>OAuth, REST, GraphQL, releases"]
    end

    copilot -->|"Bearer mr_..."| api
    anth -->|"x-api-key: mr_..."| api
    console -->|"mr_session cookie"| spa
    console --> mgmt

    api --> conv
    api --> keys
    api --> mpol
    api --> router
    api --> tracemod
    router --> sticky
    router --> pool
    mgmt --> kpol
    mgmt --> cfgmod
    mgmt --> tracemod

    kpol --> cache
    mpol --> cache
    cache --> ghapi
    ghapi --> github
    keys -.->|"OAuth sign-in"| github
    rel -.->|"latest release"| github

    keys --> files
    cache --> files
    cfgmod --> files
    tracemod --> files
    rel --> files
    conv --> pool
    pool --> foundry
    pool --> claude
```

Two properties of this shape are deliberate:

- **The provider is per model, not per deployment.** `ClientPool` caches a client per
  `(base_url, api_key, api_type, api_version, kind)`, so one router can serve models that live on
  different endpoints with different keys, and the AI decision model can have a connection of its
  own. See [Backend connections](providers.md).
- **The client protocol and the backend protocol are independent.** Everything between the two edges
  works in one canonical form, OpenAI chat completions, and `wire.py` converts at the edges only. So
  an Anthropic-style client can be answered by an Azure deployment and an OpenAI-style client by a
  Claude endpoint, and a new routing feature is written once rather than twice.
- **GitHub is never on the critical path of a model call when it can be avoided.**
  `ghcache.py` answers membership questions from a locally cached member list and only falls
  through to a live probe when that list is missing, stale or truncated. See
  [Access control](access-control.md).

## 2. The request path

What one call does, in order. Both entry points funnel into the same path, so this sequence
describes `POST /v1/messages` too; only the two conversion steps differ.
`interaction-sticky` and `session-sticky` are the two paths that skip the routing decision entirely.

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant API as chat_completions /<br/>anthropic_messages (main.py)
    participant W as wire.py
    participant Auth as authstore.py
    participant MP as modelpolicy.py
    participant S as sessions.py
    participant R as routing.py
    participant D as Decision model
    participant P as ClientPool
    participant U as Upstream model
    participant T as traces.py

    C->>API: POST /v1/chat/completions (Bearer mr_...)<br/>or POST /v1/messages (x-api-key: mr_...)
    opt the request arrived on /v1/messages
        API->>W: anthropic_request_to_openai(body)
        Note over W: converted to the canonical form,<br/>so everything below is protocol-agnostic
    end
    API->>Auth: look up the API key
    Auth-->>API: the key's owner, or 401
    Note over API: user_id is the key owner.<br/>Copilot BYOK sends no identity,<br/>so it cannot be forged by the client.

    API->>R: extract_user_prompt(messages)
    Note over R: the last user message only,<br/>unwrapped from Copilot's<br/>userRequest tags

    API->>MP: allowed_models(user_id)
    MP-->>API: a model list, or null when unrestricted
    alt the list is empty
        API-->>C: 403, no models under the current policy
    end

    API->>S: bound model for this interaction or session?
    alt already bound and still permitted
        S-->>API: the bound model
        Note over API: reason = interaction-sticky / session-sticky<br/>no decision is made, nothing is paid for
    else unbound, or stickiness is off
        API->>R: route by the configured strategy
        opt the strategy consults the decision model
            R->>P: client for the decision provider
            R->>D: one JSON classification call<br/>temperature 0, max_tokens 120
            D-->>R: {"model": ..., "rationale": ...}
        end
        R-->>API: model, reason, full analysis
        API->>S: bind this interaction and session to the model
    end

    API->>API: resolve the provider, adapt the parameters<br/>reasoning models: max_completion_tokens, no sampling
    API->>P: client for the model's provider
    alt the provider is anthropic
        API->>W: openai_request_to_anthropic
        P->>U: Messages API call, streamed or not
        U-->>P: a Messages response
        P->>W: anthropic_response_to_openai
    else api = responses
        P->>U: Responses API call
        U-->>P: a response, converted back to the chat.completion shape
    else api = chat
        P->>U: Chat Completions call, streamed or not
        U-->>P: the completion
    end
    P-->>API: the upstream result, in the canonical form
    opt the request arrived on /v1/messages
        API->>W: convert the reply back to a Messages response
    end
    API->>T: record this turn, folded into its interaction's record
    API-->>C: the completion, plus x-trace-id,<br/>x-routed-model, x-router-reason
```

The response headers are the cheap way to see what happened without opening the console:
`x-trace-id`, `x-routed-model`, `x-router-reason`, `x-router-decision-ms`, and
`x-router-interaction-id` when the request carried one.

**One user interaction is one record.** An agentic client answers a single question with a loop of
requests, each replaying the whole conversation under the same interaction id. Those requests
become successive `turns` inside one trace file rather than N separate records, and the model stays
constant across the loop:

```mermaid
flowchart LR
    q["One user question"] --> t1

    subgraph loop["One interaction, one x-interaction-id"]
        direction TB
        t1["Turn 1<br/>the routing decision is made here"] --> t2["Turn 2<br/>tool result appended"]
        t2 --> t3["Turn 3<br/>tool result appended"]
        t3 --> t4["Turn 4<br/>the model stops asking for tools"]
    end

    t1 -.->|"binds the id to a model"| bind[("sessions.py<br/>TTL + LRU")]
    t2 -.->|"reads the binding"| bind
    t3 -.->|"reads the binding"| bind
    t4 -.->|"reads the binding"| bind

    loop --> rec[("One trace file<br/>logs/traces/date/user/id.json<br/>with 4 turns")]
```

## 3. The routing strategies

Three strategies, set by `strategy` in `config.yaml` and editable on the console's "Routing
configuration" page. Every branch below writes its evidence into the trace's `routing.analysis`, so
the console can show which rule was evaluated, what the decision model was sent and what it
answered.

```mermaid
flowchart TD
    start(["A prompt to route"]) --> sticky{"stickiness on and<br/>this interaction or session<br/>already bound?"}
    sticky -->|"yes"| bound["use the bound model<br/>reason: interaction-sticky / session-sticky"]
    sticky -->|"no"| strat{"strategy"}

    strat -->|"rule"| r1["evaluate rules in order"]
    strat -->|"ai"| a1["ask the decision model"]
    strat -->|"rule-then-ai"| c1["evaluate rules in order"]

    r1 --> r2{"a rule matched?"}
    r2 -->|"yes"| rhit["that rule's model<br/>reason: the rule's own name"]
    r2 -->|"no"| rdef["the default model<br/>reason: default"]

    c1 --> c2{"a rule matched?"}
    c2 -->|"yes"| chit["that rule's model<br/>decided_by: rule<br/>no decision call is paid for"]
    c2 -->|"no"| a1

    a1 --> a2{"a usable answer<br/>naming a candidate?"}
    a2 -->|"yes"| ahit["the model it named<br/>reason: ai-decision"]
    a2 -->|"no, or timeout, or error"| afb["the default model<br/>reason: ai-fallback-default"]

    rhit --> out(["the chosen model"])
    rdef --> out
    chit --> out
    ahit --> out
    afb --> out
    bound --> out
```

How a single rule is evaluated, and why a rule can be skipped:

```mermaid
flowchart TD
    rule(["The next rule in order"]) --> known{"is its model in<br/>the catalog the caller<br/>is allowed to see?"}
    known -->|"no"| skip["skipped, recorded as such"]
    known -->|"yes"| kind{"does the rule set<br/>min_prompt_chars?"}
    kind -->|"yes"| len{"prompt length<br/>at or above it?"}
    kind -->|"no"| kw{"any keyword matches?<br/>regex, case-insensitive"}
    len -->|"yes"| hit["matched, this rule decides"]
    kw -->|"yes"| hit
    len -->|"no"| next["not matched"]
    kw -->|"no"| next
    skip --> cont(["continue to the next rule"])
    next --> cont
    cont -.->|"until a rule matches<br/>or the list runs out"| rule
```

A rule that matched on `min_prompt_chars` wins exactly as a keyword rule does. Under
`rule-then-ai` this matters: a rule is the operator stating an explicit intent, and an explicit
intent is not handed to a classifier that cannot legitimately overrule it.

What the decision model is and is not sent, which is the reason an AI decision stays cheap:

```mermaid
flowchart LR
    subgraph sent["Sent to the decision model"]
        sys["System: ai_router.decision_prompt<br/>with {catalog} replaced by<br/>each model's name and description"]
        usr["User: the last user message only,<br/>unwrapped from Copilot's userRequest tags,<br/>truncated head and tail to max_prompt_chars"]
    end

    subgraph notsent["Deliberately not sent"]
        n1["the original system prompt"]
        n2["tool / MCP / skill JSON schemas"]
        n3["the conversation history"]
    end

    sys --> ask["one call: temperature 0,<br/>max_tokens 120,<br/>response_format json_object,<br/>timeout ai_router.timeout_seconds"]
    usr --> ask
    ask --> ans{"parsed model<br/>among the candidates?"}
    ans -->|"yes"| ok["ai-decision"]
    ans -->|"no"| fb["ai-fallback-default<br/>the request is never broken<br/>by a failed decision"]
```

Truncation keeps the **head and the tail** of an over-long prompt with the middle omitted, because
the real question in an agent's prompt is usually at the very end and a head-only cut would remove
exactly the part being classified.

## 4. Which models a caller may use

Named model groups are bound to user, team and organization scopes and resolve as a **union**. The
result narrows the catalog *before* anything routes, so a model a caller may not use is unreachable
through a rule, through the decision model and through the default-model substitution alike.

```mermaid
flowchart TD
    ask(["Which models may this login call?"]) --> en{"model_policy.enabled?"}
    en -->|"no"| all["the whole catalog<br/>reason: policy-disabled"]
    en -->|"yes"| adm{"an administrator?"}
    adm -->|"yes"| alladm["the whole catalog<br/>reason: administrator"]
    adm -->|"no"| collect["collect every binding that applies"]

    collect --> b1["default_group<br/>every signed-in user"]
    collect --> b2["users: this login"]
    collect --> b3["teams: enterprise/team id<br/>membership from ghcache"]
    collect --> b4["organizations: org login<br/>membership from ghcache"]

    b1 --> u{"did any binding apply?"}
    b2 --> u
    b3 --> u
    b4 --> u

    u -->|"no"| none["the whole catalog<br/>reason: no-binding<br/>enabling the toggle must not<br/>lock the deployment out of itself"]
    u -->|"yes"| union["the union of their model groups"]
    union --> empty{"is the union empty?"}
    empty -->|"yes"| deny["nothing: 403 on a call,<br/>an empty list on /v1/models<br/>reason: empty-group"]
    empty -->|"no"| grant["that set<br/>reason: union"]

    all --> narrow
    alladm --> narrow
    none --> narrow
    grant --> narrow["RouterConfig.restricted_to<br/>narrows the catalog every<br/>strategy then sees"]
```

Because the result is a union, a binding can only ever *add* models. There is no scope that takes
models away, which is what lets an operator widen a group without auditing every narrower binding
first. A membership lookup that cannot be answered fails **open** for that one scope: it simply
does not contribute, because silently showing a user fewer models than they were granted is worse
than briefly including a scope they may have left.

## 5. Identity and authorization

Two independent identity sources, and one gate that decides who may obtain the credential the API
actually accepts.

```mermaid
flowchart LR
    gh["GitHub OAuth sign-in"] --> sess["a console session<br/>mr_session cookie"]
    la["Local super administrator<br/>admin / admin1234,<br/>a change is forced"] --> sess
    sess --> isadm{"an administrator?<br/>auth.admin_logins, or<br/>the local admin account"}
    isadm -->|"yes"| admin["every management endpoint:<br/>/v1/config, all users' traces<br/>and usage, the access-control policy"]
    isadm -->|"no"| user["own keys, own traces,<br/>own usage, own model list"]

    apikey["API key<br/>Authorization: Bearer mr_..."] --> apiuse["/v1/chat/completions, /v1/models<br/>enforced unconditionally,<br/>no config switch"]
    apiuse --> attrib["user_id = the key's owner<br/>attribution the client cannot forge"]
```

A session and an API key are separate credentials for separate surfaces: the cookie reaches the
console and the management API, the key reaches `/v1` and nothing else. Copilot BYOK sends no user
identity of its own, which is exactly why attribution is taken from the key's owner rather than
from anything in the request body.

Obtaining a key is the gated step:

```mermaid
flowchart LR
    want(["a signed-in user asks<br/>for an API key"]) --> kp["keypolicy.evaluate"]
    kp --> kpen{"key_policy.enabled?"}
    kpen -->|"no"| allow["allowed"]
    kpen -->|"yes"| tok{"an enterprise admin<br/>token configured?"}
    tok -->|"no"| deny["denied, with the reason<br/>enabled but unable to verify"]
    tok -->|"yes"| member{"a member of an allowed<br/>organization or<br/>enterprise team?"}
    member -->|"yes"| allow
    member -->|"no, or cannot tell"| deny
    allow --> issued[("a key is issued<br/>api_keys.json")]
```

The gate is **fail-closed**, because withholding one key is better than issuing one wrongly, while the
model policy of section 4 is fail-open per scope. The difference is deliberate: one is a privilege
boundary, the other is a distribution control. That is also why "enabled but unable to verify"
denies: a policy switched on must not leave the deployment less protected than switching it off.

Membership answers come from the local cache first:

```mermaid
flowchart LR
    q(["Is LOGIN a member of SCOPE?"]) --> list{"a cached member list<br/>that is fresh, complete<br/>and error-free?"}
    list -->|"yes"| set["a set lookup<br/>source: cache, zero API calls"]
    list -->|"no"| probe{"a cached individual probe<br/>within its TTL?<br/>600s positive, 120s negative"}
    probe -->|"yes"| ph["source: probe"]
    probe -->|"no"| live["live GitHub probe<br/>source: live"]
    live --> write[("written back to<br/>github/probe.json")]
    write --> ans(["the answer, with its provenance"])
    set --> ans
    ph --> ans
```

A truncated or errored member list is never authoritative: "not in the pages I could read" is not
"not a member", and treating it as one would deny legitimate users. Short negative TTLs are the
other half of that: a user added to an organization gets in quickly instead of being re-probed on
every request until a full refresh happens.

## 6. Persistent state and background tasks

Everything the router persists lives under one directory. That is the whole state of a deployment:
one directory to back up, copy to another machine, or mount into a container.

```mermaid
flowchart TB
    subgraph data["data/  (all of it gitignored)"]
        cfg[("config.yaml<br/>seeded from config.example.yaml<br/>on first start, never overwritten")]
        sessions[("auth_sessions.json")]
        keys[("api_keys.json")]
        users[("known_users.json")]
        rel[("release.json")]
        subgraph gh["github/"]
            struct[("structure.json<br/>enterprises, orgs, teams")]
            members[("members.json<br/>one entry per scope")]
            probe[("probe.json<br/>individual probe results")]
            lease[("refresh.lock<br/>best-effort lease")]
        end
        subgraph logs["logs/traces/"]
            tr[("date / user / id.json<br/>one file per interaction")]
        end
    end

    subgraph tasks["Background tasks, started by the FastAPI lifespan"]
        t1["GitHub cache refresh<br/>first run after 10s,<br/>then every cache_refresh_seconds,<br/>300s after a failure"]
        t2["Release check<br/>first run after 30s,<br/>then once a day,<br/>3600s after a failure"]
    end

    t1 --> lease
    t1 --> struct
    t1 --> members
    t2 --> rel

    inmem["In-memory only, lost on restart:<br/>sessions.py sticky bindings,<br/>the ClientPool,<br/>the trace summary index"]

    save["PUT /v1/config from the console"] --> cfg
    save --> reload["hot reload: RouterConfig rebuilt,<br/>ClientPool invalidated,<br/>policy caches dropped"]
    reload --> inmem
```

Both loops are wrapped per iteration, so a GitHub outage cannot kill either task, and a background
task that dies silently is worse than no background task, because the cache would simply stop
ageing forward and nobody would be told. Neither is required for routing: a router that cannot
reach github.com keeps serving requests, keeps the last cached answers, and shows no update chip.

The lease matters only when several workers share one `data/` directory. It makes a duplicate
refresh unlikely, and the atomic writes make a duplicate refresh merely wasteful rather than
corrupting, which is why a best-effort lease is enough and a real lock is not warranted.
