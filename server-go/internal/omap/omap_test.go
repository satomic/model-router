package omap

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestJSONRoundTripPreservesKeyOrder(t *testing.T) {
	// Key order is load-bearing, not cosmetic: `models` decides the model catalog order, which
	// decides the default model and the candidate list handed to the decision model.
	const doc = `{"zebra":1,"apple":2,"middle":{"b":1,"a":2},"list":[{"y":1,"x":2}]}`
	m := New()
	if err := json.Unmarshal([]byte(doc), m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != doc {
		t.Errorf("round trip = %s, want %s", out, doc)
	}
}

func TestIntegersDoNotBecomeFloats(t *testing.T) {
	// A plain map[string]any decode would turn 1800 into 1800.0, and the console would then
	// write "ttl_seconds: 1800.0" back into config.yaml.
	m := New()
	if err := json.Unmarshal([]byte(`{"ttl":1800,"ratio":0.5}`), m); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(m)
	if string(out) != `{"ttl":1800,"ratio":0.5}` {
		t.Errorf("numbers changed shape: %s", out)
	}
}

func TestYAMLRoundTripPreservesOrderAndTypes(t *testing.T) {
	const doc = `
strategy: rule
session:
  sticky: true
  ttl_seconds: 1800
models:
  gpt-4o:
    default: true
  claude:
    reasoning: true
empty_group: []
nothing: null
quoted: '14501973'
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatal(err)
	}
	value, err := FromYAMLNode(&node)
	if err != nil {
		t.Fatal(err)
	}
	m := value.(*Map)

	if got := m.Keys(); strings.Join(got, ",") != "strategy,session,models,empty_group,nothing,quoted" {
		t.Errorf("top-level order = %v", got)
	}
	if got := m.Map("models").Keys(); strings.Join(got, ",") != "gpt-4o,claude" {
		t.Errorf("models order = %v, want the file's order", got)
	}
	if !m.Map("session").Bool("sticky", false) {
		t.Error("sticky did not read back as true")
	}
	if got := m.Map("session").Int("ttl_seconds", 0); got != 1800 {
		t.Errorf("ttl_seconds = %d, want 1800", got)
	}
	// An enterprise team id is a quoted string in config.yaml and must not become a number.
	if got := m.Value("quoted"); got != "14501973" {
		t.Errorf("quoted = %#v, want the string", got)
	}
	if m.Value("nothing") != nil {
		t.Errorf("null read back as %#v", m.Value("nothing"))
	}
	if got := m.Slice("empty_group"); got == nil || len(got) != 0 {
		// An empty group is legal and meaningful -- it is how an operator says "this grants
		// nothing" -- so it must not read back as a missing key.
		t.Errorf("empty list read back as %#v", m.Value("empty_group"))
	}

	// And back out again, unchanged in shape.
	rendered, err := yaml.Marshal(ToYAMLNode(m))
	if err != nil {
		t.Fatal(err)
	}
	var reparsed yaml.Node
	if err := yaml.Unmarshal(rendered, &reparsed); err != nil {
		t.Fatal(err)
	}
	again, _ := FromYAMLNode(&reparsed)
	first, _ := json.Marshal(m)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Errorf("YAML round trip changed the document:\n  %s\n  %s", first, second)
	}
}

func TestToYAMLNodeQuotesStringsThatWouldChangeType(t *testing.T) {
	m := New()
	m.Set("team", "14501973")
	m.Set("flag", "true")
	m.Set("plain", "hello")
	rendered, err := yaml.Marshal(ToYAMLNode(m))
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if !strings.Contains(text, `team: '14501973'`) {
		t.Errorf("a numeric-looking string was left unquoted:\n%s", text)
	}
	if !strings.Contains(text, `flag: 'true'`) {
		t.Errorf("a bool-looking string was left unquoted:\n%s", text)
	}
	if !strings.Contains(text, "plain: hello") {
		t.Errorf("an ordinary string was quoted unnecessarily:\n%s", text)
	}
}

func TestNumAlwaysCarriesADecimalPoint(t *testing.T) {
	// Matching how Python writes a float keeps a snapshot written by either backend
	// byte-comparable with the other's.
	if got := Num(0).String(); got != "0.0" {
		t.Errorf("Num(0) = %s, want 0.0", got)
	}
	if got := Num(1.5).String(); got != "1.5" {
		t.Errorf("Num(1.5) = %s, want 1.5", got)
	}
	if got := Int(3).String(); got != "3" {
		t.Errorf("Int(3) = %s, want 3", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	// A per-request view must not be able to edit the shared configuration.
	original := New()
	inner := New()
	inner.Set("k", "v")
	original.Set("nested", inner)
	original.Set("list", []any{inner})

	clone := original.Clone()
	clone.Map("nested").Set("k", "changed")
	if inner.Str("k") != "v" {
		t.Error("Clone shared the nested map with the original")
	}
	clone.Slice("list")[0].(*Map).Set("k", "changed too")
	if inner.Str("k") != "v" {
		t.Error("Clone shared a map inside a slice with the original")
	}
}
