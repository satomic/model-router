# Backend connections (providers)

A **provider** is one "address + key" pair. Models inherit `default_provider` by default, but each
can be bound to a different provider — an Azure AI Foundry resource in another environment or
region, or any OpenAI-compatible service (OpenRouter, vLLM, a local inference server, …). Providers
can be added, edited and removed on the console's "Routing configuration → Backend connections"
page (keys are rendered in a password field with a "Show" toggle); saving writes back to
`config.yaml` and rebuilds the client connection pool immediately, with no restart.

```yaml
providers:
  foundry:
    base_url: https://xxx.openai.azure.com/     # Azure: the resource root address
    api_key: '...'
    api_type: azure                             # azure | openai
    api_version: 2024-12-01-preview             # azure only
  openrouter:
    base_url: https://openrouter.ai/api/v1      # OpenAI-compatible: include /v1
    api_key: 'sk-or-...'
    api_type: openai

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

ai_router:
  decision_model: gpt-4.1
  decision_provider: foundry     # optional: the decision model can use its own connection
  # decision_prompt: |           # optional: omit for the built-in default; {catalog} is replaced
  #   ...                        # with the model catalog
```

Getting one provider's key wrong only affects the models bound to it. Every trace's `backend`
section records the `provider` / `base_url` / `api_type` actually used (never the key), which makes
it easy to confirm the request really reached the intended environment.

Before deleting a provider you must unbind the models referencing it, and the default provider
cannot be deleted — the console blocks both cases with an explanation.
