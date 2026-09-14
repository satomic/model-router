// Package keyscope answers what one API key may reach, inside what its owner may reach.
//
// The model policy answers "which models may this person use". This package answers a
// narrower question: "which of those may this particular key use". The two compose in one
// direction only -- a key scope can subtract, never add:
//
//	effective = policy(owner)  intersected with  scope(key)
//
// That direction is the whole point. A user hands a key to a CI job, an IDE, or a colleague's
// tool, and wants that key limited to a Claude model or to two cheap deployments without
// involving an administrator. Letting a scope *widen* would turn a self-service field into a
// privilege escalation, so intersection is enforced here rather than trusted to the caller.
//
// Three kinds, because those are the three ways a user actually thinks about the limit:
//
//	{"kind": "all"}                                     everything the owner may use (default)
//	{"kind": "api_types", "api_types": ["anthropic"]}   every model on connections of that type
//	{"kind": "models", "models": ["gpt-4o", ...]}       an explicit pick from their own list
//
// "api_types" is stored as the *type*, not as the list of models it currently resolves to, so
// a model added to an Anthropic connection next week is covered by an existing key without
// anyone editing it. That is the difference between a rule and a snapshot.
package keyscope

import (
	"errors"
	"strings"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/omap"
)

var Kinds = []string{"all", "api_types", "models"}

// KindAll is "everything the owner may reach", i.e. no narrowing at all. Named because
// callers outside this package compare against it to decide whether a scope needs permission.
const KindAll = "all"

// Default is the unrestricted scope.
func Default() *omap.Map {
	scope := omap.New()
	scope.Set("kind", KindAll)
	return scope
}

// Normalize validates an incoming scope and returns it in canonical form.
//
// The error message is meant for the caller. Unknown model names are rejected rather than
// dropped: a typo that silently narrowed a key to nothing would present as "my key stopped
// working" with no visible cause.
func Normalize(raw any, cfg *config.RouterConfig) (*omap.Map, error) {
	if raw == nil {
		return Default(), nil
	}
	if s, ok := raw.(string); ok && s == "" {
		return Default(), nil
	}
	scope, ok := raw.(*omap.Map)
	if !ok {
		return nil, errors.New("scope must be an object")
	}
	if scope.Len() == 0 {
		return Default(), nil
	}
	kind := scope.Str("kind")
	if kind == "" {
		kind = "all"
	}
	if !contains(Kinds, kind) {
		return nil, errors.New("scope.kind must be one of " + strings.Join(Kinds, ", "))
	}
	if kind == "all" {
		return Default(), nil
	}
	if kind == "api_types" {
		types := omap.StringSlice(scope.Value("api_types"))
		var unknown []string
		for _, t := range types {
			if !contains(config.APITypes, t) {
				unknown = append(unknown, t)
			}
		}
		if len(unknown) > 0 {
			return nil, errors.New("unknown connection type: " + strings.Join(unknown, ", "))
		}
		if len(types) == 0 {
			return nil, errors.New("select at least one connection type")
		}
		// Deduplicated in the catalog's own order, so two keys built from the same selection
		// compare equal regardless of the order the checkboxes were ticked in.
		ordered := []any{}
		for _, t := range config.APITypes {
			if contains(types, t) {
				ordered = append(ordered, t)
			}
		}
		out := omap.New()
		out.Set("kind", "api_types")
		out.Set("api_types", ordered)
		return out, nil
	}

	models := omap.StringSlice(scope.Value("models"))
	if len(models) == 0 {
		return nil, errors.New("select at least one model")
	}
	picked := make([]any, 0, len(models))
	if cfg != nil {
		var unknown []string
		for _, m := range models {
			if !cfg.Models.Has(m) {
				unknown = append(unknown, m)
			}
		}
		if len(unknown) > 0 {
			return nil, errors.New("unknown model: " + strings.Join(unknown, ", "))
		}
		wanted := map[string]bool{}
		for _, m := range models {
			wanted[m] = true
		}
		for _, name := range cfg.Models.Keys() {
			if wanted[name] {
				picked = append(picked, name)
			}
		}
	} else {
		for _, m := range models {
			picked = append(picked, m)
		}
	}
	out := omap.New()
	out.Set("kind", "models")
	out.Set("models", picked)
	return out, nil
}

// ModelsForAPITypes lists every catalog model whose connection speaks one of `apiTypes`,
// in catalog order.
func ModelsForAPITypes(cfg *config.RouterConfig, apiTypes []string) []string {
	wanted := map[string]bool{}
	for _, t := range apiTypes {
		wanted[t] = true
	}
	out := []string{}
	for _, name := range cfg.Models.Keys() {
		provider := cfg.GetProvider(cfg.ModelMeta(name).Str("provider"))
		if provider != nil && wanted[provider.APIType] {
			out = append(out, name)
		}
	}
	return out
}

// Narrow intersects the owner's allowed set with this key's scope.
//
// `allowed` of nil means "unrestricted" (the model policy's own convention) and is returned
// unchanged for an unscoped key, so the common case adds no work and no behaviour change. A
// scoped key always returns a list, and an empty list is a real answer: the scope may name
// models the owner is no longer permitted, and the caller reports that as a 403 rather than
// silently falling back to the whole catalog.
func Narrow(cfg *config.RouterConfig, allowed []string, scope *omap.Map) []string {
	if scope == nil {
		scope = Default()
	}
	kind := scope.Str("kind")
	if kind == "" {
		kind = "all"
	}
	if kind == "all" {
		return allowed
	}
	var picked []string
	if kind == "api_types" {
		picked = ModelsForAPITypes(cfg, omap.StringSlice(scope.Value("api_types")))
	} else {
		wanted := map[string]bool{}
		for _, m := range omap.StringSlice(scope.Value("models")) {
			wanted[m] = true
		}
		picked = []string{}
		for _, name := range cfg.Models.Keys() {
			if wanted[name] {
				picked = append(picked, name)
			}
		}
	}
	if allowed == nil {
		return picked
	}
	permitted := map[string]bool{}
	for _, m := range allowed {
		permitted[m] = true
	}
	out := []string{}
	for _, m := range picked {
		if permitted[m] {
			out = append(out, m)
		}
	}
	return out
}

// Describe is a one-line form for the router log and the trace record.
func Describe(scope *omap.Map) string {
	if scope == nil {
		scope = Default()
	}
	switch scope.Str("kind") {
	case "api_types":
		return "api_types:" + strings.Join(omap.StringSlice(scope.Value("api_types")), "+")
	case "models":
		return "models:" + strings.Join(omap.StringSlice(scope.Value("models")), "+")
	}
	return "all"
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
