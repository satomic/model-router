package routing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/upstream"
	"gopkg.in/yaml.v3"
)

func fromYAML(t *testing.T, doc string) *omap.Map {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatal(err)
	}
	value, err := omap.FromYAMLNode(&node)
	if err != nil {
		t.Fatal(err)
	}
	return value.(*omap.Map)
}

// typesafeConfig builds a config whose decision engine is TypeSafe, pointed at baseURL.
func typesafeConfig(t *testing.T, baseURL, extra string) *config.RouterConfig {
	t.Helper()
	doc := `
strategy: ai
models:
  small:
    description: Cheap model for chit-chat.
    default: true
  big:
    description: Strong reasoning model.
  bare: {}
ai_router:
  decision_model: gpt-4.1
  decision_engine: typesafe
  timeout_seconds: 5
  typesafe:
    api_key: ts-test-key
    base_url: ` + baseURL + `
` + extra
	return config.New(fromYAML(t, doc))
}

// stubTypeSafe answers every request with `answer`, recording the last request body and headers.
func stubTypeSafe(t *testing.T, status int, answer string) (*httptest.Server, *map[string]any, *http.Header) {
	t.Helper()
	var body map[string]any
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &body, &header
}

func TestRouteByAITypeSafePicksTheChoice(t *testing.T) {
	srv, body, header := stubTypeSafe(t, 200, `{"model":"jev-1.13.0","answers":{"model":{"type":"choice",
		"choice":"big","confidence":0.93,"probabilities":{"small":0.03,"big":0.96,"bare":0.01}}},
		"usage":{"input_tokens":300,"output_tokens":40}}`)
	cfg := typesafeConfig(t, srv.URL, "")

	model, reason, analysis := RouteByAI(context.Background(), "prove the Riemann hypothesis", cfg, upstream.NewPool())
	if model != "big" || reason != "ai-decision" {
		t.Fatalf("got (%s, %s), want (big, ai-decision); analysis=%v", model, reason, analysis)
	}
	if got := (*header).Get("Authorization"); got != "Bearer ts-test-key" {
		t.Errorf("Authorization = %q", got)
	}
	// The request: default model alias, the prompt as structured state, one Choice over the
	// whole catalog, and a model without a description sent as a null rubric.
	req := *body
	if req["model"] != "jev-latest" {
		t.Errorf("model = %v, want jev-latest", req["model"])
	}
	if state, _ := req["state"].(map[string]any); state["user_request"] != "prove the Riemann hypothesis" {
		t.Errorf("state = %v", req["state"])
	}
	q := req["questions"].(map[string]any)["model"].(map[string]any)
	criteria := q["criteria"].(map[string]any)
	if q["type"] != "choice" || len(criteria) != 3 || criteria["big"] != "Strong reasoning model." {
		t.Errorf("question = %v", q)
	}
	if v, ok := criteria["bare"]; !ok || v != nil {
		t.Errorf("a model without a description should be a null rubric, got %v (present=%v)", v, ok)
	}
	// The analysis: tagged with the engine, carrying the calibrated answer.
	for key, want := range map[string]string{
		"decision_engine": "typesafe", "decision_model": "jev-latest",
		"decision_provider": "typesafe", "decision_model_version": "jev-1.13.0",
	} {
		if got := analysis.Str(key); got != want {
			t.Errorf("analysis[%s] = %q, want %q", key, got, want)
		}
	}
	if analysis.Value("confidence") == nil || analysis.Map("probabilities") == nil {
		t.Errorf("analysis lacks confidence/probabilities: %v", analysis)
	}
	if !strings.Contains(analysis.Str("rationale"), "p=0.96") {
		t.Errorf("rationale = %q", analysis.Str("rationale"))
	}
	if analysis.Has("decision_system") {
		t.Error("the LLM system prompt must not be recorded for a TypeSafe decision")
	}
}

func TestRouteByAITypeSafeFallsBack(t *testing.T) {
	cases := []struct {
		name, answer string
		status       int
		wantErr      string
	}{
		{"upstream error", `{"detail":{"error_type":"authentication_error","message":"Cannot authenticate"}}`,
			401, "401: Cannot authenticate"},
		{"validation error", `{"detail":[{"loc":["body","state"],"msg":"field required"}]}`, 422, "422"},
		{"unknown choice", `{"answers":{"model":{"type":"choice","choice":"ghost","confidence":0.9,
			"probabilities":{"ghost":1}}}}`, 200, "unknown model 'ghost'"},
		{"no answer", `{"answers":{}}`, 200, "no answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := stubTypeSafe(t, tc.status, tc.answer)
			cfg := typesafeConfig(t, srv.URL, "")
			model, reason, analysis := RouteByAI(context.Background(), "hi", cfg, upstream.NewPool())
			if model != "small" || reason != "ai-fallback-default" {
				t.Fatalf("got (%s, %s), want the default model", model, reason)
			}
			if !strings.Contains(analysis.Str("error"), tc.wantErr) || !analysis.Bool("fallback", false) {
				t.Errorf("error = %q, want it to mention %q", analysis.Str("error"), tc.wantErr)
			}
		})
	}
}

// The engine is opt-in: a config without decision_engine keeps the LLM path, and so a
// TypeSafe section sitting unused in config.yaml changes nothing.
func TestDecisionEngineDefaultsToLLM(t *testing.T) {
	cfg := config.New(fromYAML(t, "ai_router:\n  typesafe:\n    api_key: x\n"))
	if cfg.UsesTypeSafe() || cfg.DecisionEngine != "llm" {
		t.Fatalf("engine = %q, want llm", cfg.DecisionEngine)
	}
	if cfg.TypeSafe.Model != config.TypeSafeDefaultModel || cfg.TypeSafe.BaseURL != config.TypeSafeDefaultBaseURL {
		t.Errorf("TypeSafe defaults not applied: %+v", cfg.TypeSafe)
	}
}
