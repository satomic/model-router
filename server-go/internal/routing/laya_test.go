package routing

import (
	"context"
	"strings"
	"testing"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/upstream"
)

// layaConfig builds a config whose decision engine is a self-hosted Laya at baseURL. `laya`
// holds the extra keys of the ai_router.laya section (api_key, model), indented.
func layaConfig(t *testing.T, baseURL, laya string) *config.RouterConfig {
	t.Helper()
	doc := `
strategy: ai
models:
  small:
    description: Cheap model for chit-chat.
    default: true
  big:
    description: Strong reasoning model.
ai_router:
  decision_engine: laya
  timeout_seconds: 5
  typesafe:
    api_key: must-not-be-sent-to-laya
  laya:
    base_url: ` + baseURL + `
` + laya
	return config.New(fromYAML(t, doc))
}

// laya-serve's actual response shape: Jev's answers and usage, plus the checkpoint it routed to.
const layaAnswer = `{"model":"laya-rl-agent","answers":{"model":{"type":"choice","choice":"big",
	"confidence":0.41,"probabilities":{"small":0.27,"big":0.73}}},"usage":{"input_tokens":205,"output_tokens":0},
	"routing":{"model":"multilingual","repo":"convaiinnovations/laya/multilingual","reason":"non-Latin script"}}`

func TestRouteByAILayaWithoutKeyOrCheckpoint(t *testing.T) {
	srv, body, header := stubTypeSafe(t, 200, layaAnswer)
	cfg := layaConfig(t, srv.URL, "")

	model, reason, analysis := RouteByAI(context.Background(), "证明这个算法的复杂度", cfg, upstream.NewPool())
	if model != "big" || reason != "ai-decision" {
		t.Fatalf("got (%s, %s), want (big, ai-decision); analysis=%v", model, reason, analysis)
	}
	// No key configured: no Authorization header at all -- and in particular not the TypeSafe
	// key, which belongs to a different service.
	if got := (*header).Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
	// No checkpoint pinned: the model field is omitted so laya chooses by language.
	if _, ok := (*body)["model"]; ok {
		t.Errorf("model field sent although no checkpoint is pinned: %v", (*body)["model"])
	}
	for key, want := range map[string]string{
		"decision_engine": "laya", "decision_provider": "laya", "decision_model": "auto",
		"decision_model_version": "laya-rl-agent",
	} {
		if got := analysis.Str(key); got != want {
			t.Errorf("analysis[%s] = %q, want %q", key, got, want)
		}
	}
	if got := analysis.Map("decision_checkpoint").Str("model"); got != "multilingual" {
		t.Errorf("decision_checkpoint.model = %q, want multilingual", got)
	}
	if !strings.HasPrefix(analysis.Str("rationale"), "Laya picked big") {
		t.Errorf("rationale = %q", analysis.Str("rationale"))
	}
}

func TestRouteByAILayaWithKeyAndCheckpoint(t *testing.T) {
	srv, body, header := stubTypeSafe(t, 200, layaAnswer)
	cfg := layaConfig(t, srv.URL, "    api_key: laya_secret\n    model: english\n")

	if model, _, _ := RouteByAI(context.Background(), "prove it", cfg, upstream.NewPool()); model != "big" {
		t.Fatalf("model = %s, want big", model)
	}
	if got := (*header).Get("Authorization"); got != "Bearer laya_secret" {
		t.Errorf("Authorization = %q, want the Laya key", got)
	}
	if (*body)["model"] != "english" {
		t.Errorf("model = %v, want the pinned checkpoint", (*body)["model"])
	}
}

// laya-serve reports errors as FastAPI does, {"detail": "<message>"}; the trace must carry the
// message, and the request must still be served by the default model.
func TestRouteByAILayaErrorFallsBack(t *testing.T) {
	srv, _, _ := stubTypeSafe(t, 401, `{"detail":"invalid or missing bearer token"}`)
	cfg := layaConfig(t, srv.URL, "")

	model, reason, analysis := RouteByAI(context.Background(), "hi", cfg, upstream.NewPool())
	if model != "small" || reason != "ai-fallback-default" {
		t.Fatalf("got (%s, %s), want the default model", model, reason)
	}
	if got := analysis.Str("error"); got != "401: invalid or missing bearer token" {
		t.Errorf("error = %q", got)
	}
}

func TestRouteByAILayaWithoutAddressFallsBack(t *testing.T) {
	cfg := layaConfig(t, "''", "")
	model, reason, analysis := RouteByAI(context.Background(), "hi", cfg, upstream.NewPool())
	if model != "small" || reason != "ai-fallback-default" || !strings.Contains(analysis.Str("error"), "base_url") {
		t.Fatalf("got (%s, %s, %q), want a fallback naming base_url", model, reason, analysis.Str("error"))
	}
}
