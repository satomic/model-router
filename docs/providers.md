# Backend connections (providers)

A **provider** is one "address + key" pair, plus the interface type it speaks. Models inherit
`default_provider` by default, but each can be bound to a different provider: an Azure AI Foundry
resource in another environment or region, any OpenAI-compatible service (OpenRouter, vLLM, a local
inference server), or any Anthropic-compatible service (Anthropic itself, a Databricks Claude
serving endpoint, a gateway exposing the Messages API). Providers can be added, edited and removed
on the console's "Routing configuration → Backend connections" page (keys are rendered in a password
field with a "Show" toggle); saving writes back to `config.yaml` and rebuilds the client connection
pool immediately, with no restart.

```yaml
providers:
  foundry:
    base_url: https://xxx.openai.azure.com/     # Azure: the resource root address
    api_key: '...'
    api_type: azure                             # azure | openai | anthropic
    api_version: 2024-12-01-preview             # azure: the ?api-version= query parameter
  openrouter:
    base_url: https://openrouter.ai/api/v1      # OpenAI-compatible: include /v1
    api_key: 'sk-or-...'
    api_type: openai                            # no version field at all
  databricks:
    base_url: https://xxx.azuredatabricks.net/serving-endpoints/anthropic
    api_key: 'dapi...'                          # sent as the x-api-key header
    api_type: anthropic
    api_version: '2023-06-01'                   # anthropic: the anthropic-version header

default_provider: foundry

models:
  gpt-4o:
    provider: foundry            # omit to use default_provider
    description: ...
    default: true
  claude-opus-5:
    provider: openrouter
    model_name: anthropic/claude-opus-5   # the real upstream model name; omit to use the key above
    reasoning: true
  claude-sonnet-5:
    provider: databricks
    model_name: databricks-claude-sonnet-5
    reasoning: true               # this endpoint rejects temperature, so the flag is required

ai_router:
  decision_model: gpt-4.1
  decision_provider: foundry     # optional: the decision model can use its own connection
  # decision_prompt: |           # optional: omit for the built-in default; {catalog} is replaced
  #   ...                        # with the model catalog
```

## The interface type is a backend detail

`azure` and `openai` differ only in how the client is constructed; both speak OpenAI chat
completions. `anthropic` speaks the Messages API, which is a different request and response shape
entirely. **Callers do not have to match it.** The router accepts requests on both protocols and
converts to whatever the chosen model's provider speaks:

| Client sends | Provider `api_type` | What the router does |
|---|---|---|
| `POST /v1/chat/completions` | `azure` / `openai` | passes through |
| `POST /v1/chat/completions` | `anthropic` | converts the request to Messages, converts the reply back |
| `POST /v1/messages` | `anthropic` | passes through |
| `POST /v1/messages` | `azure` / `openai` | converts the request to chat completions, converts the reply back |

Streaming is converted event by event, so a streaming client always sees its own protocol's events.
Fields both protocols have survive intact: a system message becomes the Anthropic `system` parameter
and back, tool calls map to `tool_use` / `tool_result` blocks and back, image parts map to `image`
blocks, and `finish_reason` maps to `stop_reason` and back. Two conversions are worth knowing about
because they are not one-to-one:

* **OpenAI-only parameters are dropped, not guessed at**: `presence_penalty`, `frequency_penalty`,
  `seed`, `n`, `logprobs` and `response_format`. The trace's `backend.dropped_params` lists exactly
  which ones, so a parameter that never reached the model is visible rather than silently ignored.
* **`max_tokens` is required by the Messages API** and optional in chat completions, so an
  OpenAI-style request that omits it is sent with a ceiling of 8192. That is a cap rather than a
  target, chosen so a caller who asked for no limit does not get a truncated answer; a caller who
  wants the upstream's own default should set the field explicitly. `temperature` is clamped rather than
  rescaled, because Anthropic accepts 0..1 while OpenAI allows up to 2 and rejects nothing.

Getting one provider's key wrong only affects the models bound to it. Every trace's `backend`
section records the `provider` / `base_url` / `api_type` / `protocol` actually used (never the key),
and every turn records the `client_protocol` it arrived on, which is how you confirm both that the
request reached the intended environment and whether a conversion happened.

Before deleting a provider you must unbind the models referencing it, and the default provider
cannot be deleted: the console blocks both cases with an explanation.
