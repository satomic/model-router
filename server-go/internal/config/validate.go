package config

import (
	"fmt"
	"strings"

	"github.com/satomic/model-router/server-go/internal/cronexpr"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// Validate returns the list of configuration errors; an empty list means valid.
func Validate(raw *omap.Map) []string {
	errors := []string{}
	if raw == nil {
		return errors
	}

	if raw.Has("strategy") {
		strategy := raw.Str("strategy")
		known := false
		for _, s := range Strategies {
			if strategy == s {
				known = true
			}
		}
		if !known {
			errors = append(errors, "strategy must be one of "+quotedList(Strategies))
		}
	}

	var providers *omap.Map
	if raw.Has("providers") {
		providers = raw.Map("providers")
		if providers == nil || providers.Len() == 0 {
			errors = append(errors, "providers must not be empty")
		} else {
			for _, name := range providers.Keys() {
				meta := orEmpty(providers.Map(name))
				if strings.TrimSpace(meta.Str("base_url")) == "" {
					errors = append(errors, fmt.Sprintf("provider %s is missing base_url", quote(name)))
				}
				apiType := meta.Str("api_type")
				if apiType == "" {
					apiType = "azure"
				}
				if !contains(APITypes, apiType) {
					errors = append(errors, fmt.Sprintf("provider %s: api_type must be one of %s",
						quote(name), quotedList(APITypes)))
				}
			}
			defaultProvider := raw.Str("default_provider")
			if defaultProvider != "" && !providers.Has(defaultProvider) {
				errors = append(errors, fmt.Sprintf("default_provider %s is not in providers", quote(defaultProvider)))
			}
		}
	}

	if raw.Has("models") {
		models := raw.Map("models")
		if models == nil || models.Len() == 0 {
			errors = append(errors, "models must not be empty")
		} else {
			for _, name := range models.Keys() {
				ref := orEmpty(models.Map(name)).Str("provider")
				if ref != "" && providers != nil && providers.Len() > 0 && !providers.Has(ref) {
					errors = append(errors, fmt.Sprintf("model %s references unknown provider %s", quote(name), quote(ref)))
				}
			}
			for _, item := range raw.Slice("rules") {
				rule, ok := item.(*omap.Map)
				if !ok {
					continue
				}
				model := rule.Str("model")
				if !models.Has(model) {
					name := rule.Str("name")
					if name == "" {
						name = "?"
					}
					errors = append(errors, fmt.Sprintf("rule %s references unknown model %s", name, quote(model)))
				}
			}
		}
	}

	if session := raw.Map("session"); session != nil && session.Has("sticky") {
		if _, ok := session.Value("sticky").(bool); !ok {
			errors = append(errors, "session.sticky must be a boolean")
		}
	}

	errors = append(errors, validateAIRouter(raw.Value("ai_router"), providers)...)
	if raw.Has("ai_credits") {
		errors = append(errors, ValidateAICredits(raw.Value("ai_credits"))...)
	}
	errors = append(errors, validateModelGroups(raw.Value("model_groups"), raw.Map("models"))...)
	errors = append(errors, validateModelPolicy(raw.Value("model_policy"), raw.Map("model_groups"))...)

	if raw.Has("auth") {
		auth, ok := raw.Value("auth").(*omap.Map)
		if !ok {
			if raw.Value("auth") != nil {
				errors = append(errors, "auth must be an object")
			}
		} else {
			if auth.Has("admin_logins") {
				if _, ok := auth.Value("admin_logins").([]any); !ok && auth.Value("admin_logins") != nil {
					errors = append(errors, "auth.admin_logins must be a list")
				}
			}
			errors = append(errors, validateKeyPolicy(auth.Value("key_policy"))...)
			errors = append(errors, validateKeyScopePolicy(auth.Value("key_scope_policy"))...)
			errors = append(errors, validateLocalAdmin(auth.Value("local_admin"))...)
		}
	}
	return errors
}

// validateLocalAdmin checks auth.local_admin.
//
// The password hash fields are deliberately *not* validated as a required pair: an operator
// who blanks them is asking for the default password back, which is the documented way to
// recover from a lost local-admin password.
func validateLocalAdmin(value any) []string {
	if value == nil {
		return nil
	}
	la, ok := value.(*omap.Map)
	if !ok {
		return []string{"auth.local_admin must be an object"}
	}
	var errors []string
	if la.Has("enabled") {
		if _, ok := la.Value("enabled").(bool); !ok {
			errors = append(errors, "auth.local_admin.enabled must be a boolean")
		}
	}
	if la.Has("username") && la.Value("username") != nil {
		if strings.TrimSpace(la.Str("username")) == "" {
			errors = append(errors, "auth.local_admin.username must not be empty")
		}
	}
	return errors
}

// validateAIRouter checks ai_router. The prompt only gets soft checks such as "not empty" --
// prompt quality cannot be judged mechanically, and a missing {catalog} placeholder is not an
// error (rendering appends the catalog).
func validateAIRouter(value any, providers *omap.Map) []string {
	if value == nil {
		return nil
	}
	ai, ok := value.(*omap.Map)
	if !ok {
		return []string{"ai_router must be an object"}
	}
	var errors []string
	decisionModel := "gpt-4.1"
	if ai.Has("decision_model") {
		decisionModel = ai.Str("decision_model")
	}
	if strings.TrimSpace(decisionModel) == "" {
		errors = append(errors, "ai_router.decision_model must not be empty")
	}
	ref := ai.Str("decision_provider")
	if ref != "" && providers != nil && providers.Len() > 0 && !providers.Has(ref) {
		errors = append(errors, fmt.Sprintf("ai_router.decision_provider %s is not in providers", quote(ref)))
	}
	for _, field := range []struct {
		name string
		low  float64
	}{{"timeout_seconds", 0}, {"max_prompt_chars", 1}} {
		raw := ai.Value(field.name)
		if raw == nil {
			continue
		}
		if _, isBool := raw.(bool); isBool {
			errors = append(errors, fmt.Sprintf("ai_router.%s must be a number greater than %g", field.name, field.low))
			continue
		}
		v, ok := number(raw)
		if !ok || v <= field.low {
			errors = append(errors, fmt.Sprintf("ai_router.%s must be a number greater than %g", field.name, field.low))
		}
	}
	engine := strings.TrimSpace(ai.Str("decision_engine"))
	if ai.Value("decision_engine") != nil && !contains(DecisionEngines, engine) {
		errors = append(errors, fmt.Sprintf("ai_router.decision_engine must be one of %s",
			strings.Join(DecisionEngines, ", ")))
	}
	if ts := ai.Value("typesafe"); ts != nil {
		if tsMap, ok := ts.(*omap.Map); !ok {
			errors = append(errors, "ai_router.typesafe must be an object")
		} else if engine == "typesafe" {
			// A missing key is only an error when the engine is actually selected, and only
			// when the environment does not supply one either: an operator switching back to
			// "llm" should not have to clear a section they may switch to again.
			if strings.TrimSpace(tsMap.Str("api_key")) == "" && EnvTypeSafeAPIKey == "" {
				errors = append(errors, "ai_router.typesafe.api_key is required when decision_engine is typesafe "+
					"(or set TYPESAFE_API_KEY in the environment)")
			}
			if raw := strings.TrimSpace(tsMap.Str("base_url")); raw != "" &&
				!strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
				errors = append(errors, "ai_router.typesafe.base_url must start with http:// or https://")
			}
		}
	} else if engine == "typesafe" && EnvTypeSafeAPIKey == "" {
		errors = append(errors, "ai_router.typesafe.api_key is required when decision_engine is typesafe "+
			"(or set TYPESAFE_API_KEY in the environment)")
	}
	if ai.Has("decision_prompt") && ai.Value("decision_prompt") != nil {
		prompt, ok := ai.Value("decision_prompt").(string)
		if !ok {
			errors = append(errors, "ai_router.decision_prompt must be a string")
		} else if trimmed := strings.TrimSpace(prompt); trimmed != "" && len(trimmed) < 20 {
			errors = append(errors, "ai_router.decision_prompt is too short for the decision model to "+
				"reliably emit JSON (leave it empty to use the built-in default)")
		}
	}
	return errors
}

// validateModelGroups checks model_groups: {name: [model, ...]}.
//
// An **empty list is legal and meaningful** -- it is how an operator says "this group grants
// nothing", which is how a freshly signed-in user can be given an empty group. So emptiness
// is never an error here.
//
// A member that is not in the models catalog *is* an error, because it can only be a typo or
// a stale reference: the group would silently grant less than it appears to.
func validateModelGroups(value any, models *omap.Map) []string {
	if value == nil {
		return nil
	}
	groups, ok := value.(*omap.Map)
	if !ok {
		return []string{"model_groups must be an object keyed by group name"}
	}
	var errors []string
	for _, name := range groups.Keys() {
		if strings.TrimSpace(name) == "" {
			errors = append(errors, "model_groups has an entry with an empty name")
		}
		members := groups.Value(name)
		if members == nil {
			continue // an omitted list reads the same as [], i.e. an empty group
		}
		items, ok := members.([]any)
		if !ok {
			errors = append(errors, fmt.Sprintf("model_groups[%s] must be a list of model names", quote(name)))
			continue
		}
		if models == nil || models.Len() == 0 {
			continue
		}
		for _, member := range items {
			if !models.Has(omap.AsString(member)) {
				errors = append(errors, fmt.Sprintf("model_groups[%s] references unknown model %s",
					quote(name), quote(omap.AsString(member))))
			}
		}
	}
	return errors
}

// validateModelPolicy checks model_policy: which model group each scope gets.
//
// Like validateKeyPolicy this leans towards warning-free tolerance: an enabled policy with no
// bindings at all is legal, so that an admin can turn the toggle on before filling the tables
// in. A binding naming a group that does not exist is an error, though -- it grants nothing
// while looking like it grants something.
func validateModelPolicy(value any, groups *omap.Map) []string {
	if value == nil {
		return nil
	}
	policy, ok := value.(*omap.Map)
	if !ok {
		return []string{"model_policy must be an object"}
	}
	var errors []string
	if policy.Has("enabled") {
		if _, ok := policy.Value("enabled").(bool); !ok {
			errors = append(errors, "model_policy.enabled must be a boolean")
		}
	}
	known := groups != nil && groups.Len() > 0
	defaultGroup := policy.Value("default_group")
	if defaultGroup != nil && omap.AsString(defaultGroup) != "" && known && !groups.Has(omap.AsString(defaultGroup)) {
		errors = append(errors, fmt.Sprintf("model_policy.default_group %s is not a known model group",
			quote(omap.AsString(defaultGroup))))
	}
	for _, field := range []string{"users", "teams", "organizations"} {
		raw := policy.Value(field)
		if raw == nil {
			continue
		}
		table, ok := raw.(*omap.Map)
		if !ok {
			errors = append(errors, fmt.Sprintf("model_policy.%s must be an object keyed by name", field))
			continue
		}
		for _, key := range table.Keys() {
			group := table.Value(key)
			if group == nil || omap.AsString(group) == "" {
				continue // an explicit blank is "no binding", not a broken one
			}
			if known && !groups.Has(omap.AsString(group)) {
				errors = append(errors, fmt.Sprintf("model_policy.%s[%s] references unknown model group %s",
					field, quote(key), quote(omap.AsString(group))))
			}
		}
	}
	return errors
}

// validateKeyPolicy checks auth.key_policy. Enabled-but-tokenless is a legal yet dangerous
// configuration -- key policy evaluation then denies every non-admin -- so this only warns
// instead of erroring, to avoid trapping the admin in a "cannot save the toggle before
// filling in the token" deadlock.
func validateKeyPolicy(value any) []string {
	if value == nil {
		return nil
	}
	policy, ok := value.(*omap.Map)
	if !ok {
		return []string{"auth.key_policy must be an object"}
	}
	var errors []string
	if policy.Has("enabled") {
		if _, ok := policy.Value("enabled").(bool); !ok {
			errors = append(errors, "auth.key_policy.enabled must be a boolean")
		}
	}
	raw := policy.Value("enterprises")
	if raw == nil {
		return errors
	}
	enterprises, ok := raw.(*omap.Map)
	if !ok {
		return append(errors, "auth.key_policy.enterprises must be an object keyed by enterprise slug")
	}
	for _, slug := range enterprises.Keys() {
		entry := enterprises.Value(slug)
		if entry == nil {
			continue
		}
		rule, ok := entry.(*omap.Map)
		if !ok {
			errors = append(errors, fmt.Sprintf("auth.key_policy.enterprises[%s] must be an object", quote(slug)))
			continue
		}
		for _, flag := range []string{"enabled", "allow_all_orgs"} {
			if rule.Has(flag) {
				if _, ok := rule.Value(flag).(bool); !ok {
					errors = append(errors, fmt.Sprintf("auth.key_policy.enterprises[%s].%s must be a boolean", quote(slug), flag))
				}
			}
		}
		for _, field := range []string{"organizations", "teams"} {
			v := rule.Value(field)
			if v == nil {
				continue
			}
			if _, ok := v.([]any); !ok {
				errors = append(errors, fmt.Sprintf("auth.key_policy.enterprises[%s].%s must be a list", quote(slug), field))
			}
		}
	}
	return errors
}

// validateKeyScopePolicy checks auth.key_scope_policy (who may narrow an API key).
//
// Types only. In particular a `teams` entry that is not '<enterprise slug>/<team id>' is not
// rejected here: the same shape sits in model_policy.teams unvalidated, a hand-edited
// config.yaml with one bad entry would otherwise block every unrelated save from the console,
// and the policy evaluator already logs the entry it had to ignore.
func validateKeyScopePolicy(value any) []string {
	if value == nil {
		return nil
	}
	policy, ok := value.(*omap.Map)
	if !ok {
		return []string{"auth.key_scope_policy must be an object"}
	}
	var errors []string
	if policy.Has("enabled") {
		if _, ok := policy.Value("enabled").(bool); !ok {
			errors = append(errors, "auth.key_scope_policy.enabled must be a boolean")
		}
	}
	for _, field := range []string{"users", "teams", "organizations"} {
		raw := policy.Value(field)
		if raw == nil {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			errors = append(errors, fmt.Sprintf("auth.key_scope_policy.%s must be a list", field))
			continue
		}
		for _, item := range items {
			switch item.(type) {
			case *omap.Map, []any:
				errors = append(errors, fmt.Sprintf("auth.key_scope_policy.%s must be a list of names", field))
			}
		}
	}
	return errors
}

// ValidateAICredits checks the `ai_credits` section. Lives here rather than alongside the
// polling code so the validator does not have to import it.
func ValidateAICredits(value any) []string {
	if value == nil {
		return nil
	}
	raw, ok := value.(*omap.Map)
	if !ok {
		return []string{"ai_credits must be an object"}
	}
	var errors []string
	if raw.Has("enabled") {
		if _, ok := raw.Value("enabled").(bool); !ok {
			errors = append(errors, "ai_credits.enabled must be a boolean")
		}
	}
	if raw.Has("schedule") && raw.Value("schedule") != nil {
		if problem := cronexpr.Validate(omap.AsString(raw.Value("schedule"))); problem != "" {
			errors = append(errors, "ai_credits.schedule is not a valid cron expression: "+problem)
		}
	}
	if ents := raw.Value("enterprises"); ents != nil {
		if _, ok := ents.([]any); !ok {
			errors = append(errors, "ai_credits.enterprises must be a list of enterprise slugs")
		}
	}
	if floorRaw := raw.Value("min_remaining_credits"); floorRaw != nil {
		if v, ok := number(floorRaw); !ok {
			errors = append(errors, "ai_credits.min_remaining_credits must be a number")
		} else if v < 0 {
			errors = append(errors, "ai_credits.min_remaining_credits must be >= 0")
		}
	}
	if gateRaw := raw.Value("gate"); gateRaw != nil {
		gate, ok := gateRaw.(*omap.Map)
		if !ok {
			errors = append(errors, "ai_credits.gate must be an object")
		} else {
			for _, field := range []string{"enabled", "per_user"} {
				if gate.Has(field) {
					if _, ok := gate.Value(field).(bool); !ok {
						errors = append(errors, fmt.Sprintf("ai_credits.gate.%s must be a boolean", field))
					}
				}
			}
			for _, field := range []string{"message", "message_budget"} {
				if gate.Has(field) && gate.Value(field) != nil {
					if _, ok := gate.Value(field).(string); !ok {
						errors = append(errors, fmt.Sprintf("ai_credits.gate.%s must be a string", field))
					}
				}
			}
		}
	}
	return errors
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// quote matches Python's repr() for the plain strings these messages carry, so the console
// shows the same wording either backend produced it.
func quote(s string) string { return "'" + s + "'" }

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = quote(item)
	}
	return strings.Join(quoted, ", ")
}
