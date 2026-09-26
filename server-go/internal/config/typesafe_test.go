package config

import (
	"strings"
	"testing"

	"github.com/satomic/model-router/server-go/internal/omap"
	"gopkg.in/yaml.v3"
)

func aiRouterDoc(t *testing.T, doc string) any {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatal(err)
	}
	value, err := omap.FromYAMLNode(&node)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestValidateDecisionEngine(t *testing.T) {
	saved := EnvTypeSafeAPIKey
	t.Cleanup(func() { EnvTypeSafeAPIKey = saved })
	EnvTypeSafeAPIKey = ""

	cases := []struct {
		name, doc, wantErr string
	}{
		{"absent engine is the LLM", "decision_model: gpt-4.1\n", ""},
		{"llm", "decision_engine: llm\n", ""},
		{"unknown engine", "decision_engine: magic\n", "decision_engine must be one of"},
		{"typesafe with key", "decision_engine: typesafe\ntypesafe:\n  api_key: k\n", ""},
		{"typesafe without key", "decision_engine: typesafe\ntypesafe:\n  model: jev-latest\n", "api_key is required"},
		{"typesafe without section", "decision_engine: typesafe\n", "api_key is required"},
		// An unused section is kept as-is: switching back to the LLM must not demand clearing it.
		{"unused section without key", "decision_engine: llm\ntypesafe:\n  model: jev-latest\n", ""},
		{"bad base_url", "decision_engine: typesafe\ntypesafe:\n  api_key: k\n  base_url: api.typesafe.ai\n", "base_url must start"},
		{"laya with address", "decision_engine: laya\nlaya:\n  base_url: http://127.0.0.1:8100\n", ""},
		{"laya with key and checkpoint", "decision_engine: laya\nlaya:\n  base_url: http://h:8100\n  api_key: k\n  model: multilingual\n", ""},
		{"laya without address", "decision_engine: laya\nlaya:\n  api_key: k\n", "laya.base_url is required"},
		{"laya without section", "decision_engine: laya\n", "laya.base_url is required"},
		{"laya bad address", "decision_engine: laya\nlaya:\n  base_url: 127.0.0.1:8100\n", "laya.base_url must start"},
		{"laya unknown checkpoint", "decision_engine: laya\nlaya:\n  base_url: http://h\n  model: jev-latest\n", "laya.model must be one of"},
		// Kept but unused while another engine is selected.
		{"unused laya section", "decision_engine: llm\nlaya:\n  model: english\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := strings.Join(validateAIRouter(aiRouterDoc(t, tc.doc), nil), "; ")
			if tc.wantErr == "" && errs != "" {
				t.Errorf("unexpected errors: %s", errs)
			}
			if tc.wantErr != "" && !strings.Contains(errs, tc.wantErr) {
				t.Errorf("errors %q do not mention %q", errs, tc.wantErr)
			}
		})
	}

	// The environment variable stands in for a missing key, as it does at request time.
	EnvTypeSafeAPIKey = "from-env"
	if errs := validateAIRouter(aiRouterDoc(t, "decision_engine: typesafe\n"), nil); len(errs) != 0 {
		t.Errorf("TYPESAFE_API_KEY in the environment should satisfy the key check, got %v", errs)
	}
}
