package keyscope

import (
	"strings"
	"testing"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// testConfig builds a catalog whose models sit on two connection types, which is what the
// api_types scope exists to select between.
func testConfig(t *testing.T) *config.RouterConfig {
	t.Helper()
	value, err := omap.FromJSON([]byte(`{
		"default_provider": "azure1",
		"providers": {
			"azure1": {"base_url": "https://a", "api_type": "azure"},
			"claude": {"base_url": "https://c", "api_type": "anthropic"}
		},
		"models": {
			"gpt-4o": {"provider": "azure1"},
			"claude-5": {"provider": "claude"},
			"o3-pro": {"provider": "azure1"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return config.New(value.(*omap.Map))
}

func scope(t *testing.T, text string) *omap.Map {
	t.Helper()
	value, err := omap.FromJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*omap.Map)
}

func TestNormalizeOrdersByCatalogNotByInput(t *testing.T) {
	// Two keys built from the same selection must compare equal regardless of the order the
	// checkboxes were ticked in.
	cfg := testConfig(t)
	a, err := Normalize(scope(t, `{"kind":"models","models":["o3-pro","gpt-4o"]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Normalize(scope(t, `{"kind":"models","models":["gpt-4o","o3-pro"]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if Describe(a) != Describe(b) {
		t.Errorf("%s != %s", Describe(a), Describe(b))
	}
	if Describe(a) != "models:gpt-4o+o3-pro" {
		t.Errorf("Describe = %s, want catalog order", Describe(a))
	}
}

func TestNormalizeRejectsUnknownNames(t *testing.T) {
	// A typo that silently narrowed a key to nothing would present as "my key stopped working"
	// with no visible cause.
	cfg := testConfig(t)
	if _, err := Normalize(scope(t, `{"kind":"models","models":["nope"]}`), cfg); err == nil {
		t.Error("an unknown model was accepted")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the message should name the offender, got %q", err)
	}
	if _, err := Normalize(scope(t, `{"kind":"api_types","api_types":["carrier-pigeon"]}`), cfg); err == nil {
		t.Error("an unknown connection type was accepted")
	}
	if _, err := Normalize(scope(t, `{"kind":"models","models":[]}`), cfg); err == nil {
		t.Error("an empty selection was accepted")
	}
}

func TestNormalizeTreatsAbsenceAsUnrestricted(t *testing.T) {
	cfg := testConfig(t)
	for _, raw := range []any{nil, "", omap.New()} {
		got, err := Normalize(raw, cfg)
		if err != nil {
			t.Fatalf("%#v: %v", raw, err)
		}
		if got.Str("kind") != KindAll {
			t.Errorf("%#v normalised to %s, want the unrestricted default", raw, Describe(got))
		}
	}
}

func TestNarrowCanOnlySubtract(t *testing.T) {
	cfg := testConfig(t)
	owner := []string{"gpt-4o", "claude-5"} // what the model policy permits this person

	// A scope naming a model the owner may not use yields the intersection, not the union.
	narrowed := Narrow(cfg, owner, scope(t, `{"kind":"models","models":["gpt-4o","o3-pro"]}`))
	if strings.Join(narrowed, ",") != "gpt-4o" {
		t.Errorf("Narrow = %v, want only the permitted overlap", narrowed)
	}

	// An unscoped key passes the owner's set through untouched, so the common case adds no work.
	if got := Narrow(cfg, owner, Default()); strings.Join(got, ",") != "gpt-4o,claude-5" {
		t.Errorf("an unscoped key changed the set: %v", got)
	}

	// nil means "unrestricted", and an unscoped key keeps it that way.
	if got := Narrow(cfg, nil, Default()); got != nil {
		t.Errorf("Narrow(nil, all) = %v, want nil", got)
	}

	// An empty result is a real answer: the scope names models the owner is no longer permitted,
	// and the caller reports that rather than falling back to the whole catalog.
	empty := Narrow(cfg, []string{"gpt-4o"}, scope(t, `{"kind":"models","models":["claude-5"]}`))
	if empty == nil || len(empty) != 0 {
		t.Errorf("Narrow = %v, want an empty list rather than nil", empty)
	}
}

func TestNarrowByAPITypeIsARuleNotASnapshot(t *testing.T) {
	// A model added to an Anthropic connection next week must be covered by an existing key
	// without anyone editing it, which is why the type is stored rather than the resolved list.
	cfg := testConfig(t)
	got := Narrow(cfg, nil, scope(t, `{"kind":"api_types","api_types":["anthropic"]}`))
	if strings.Join(got, ",") != "claude-5" {
		t.Errorf("Narrow = %v, want every model on an anthropic connection", got)
	}
	got = Narrow(cfg, nil, scope(t, `{"kind":"api_types","api_types":["azure"]}`))
	if strings.Join(got, ",") != "gpt-4o,o3-pro" {
		t.Errorf("Narrow = %v, want catalog order", got)
	}
}
