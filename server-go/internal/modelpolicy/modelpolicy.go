// Package modelpolicy answers which models a given caller may use: named model groups bound to
// user / team / organization scopes, resolved as a **union**.
//
// The shape in config.yaml:
//
//	model_groups:                 # independent, named, reusable; an empty group is legal
//	  starter:   [gpt-4o]
//	  full:      [gpt-4o, gpt-5.4, o3-pro]
//	  locked:    []
//
//	model_policy:
//	  enabled: true
//	  default_group: starter      # every signed-in user, before any binding below
//	  users:
//	    alice: full               # a GitHub login
//	  teams:
//	    my-enterprise/14501973: full   # "<enterprise slug>/<team id>", as ghcache keys it
//	  organizations:
//	    acme: full                # an organization login
//
// Resolution, and the two questions the shape forces:
//
//  1. **Union, not override.** A caller's effective set is the union of the default group and
//     every group bound to a scope they belong to. Precedence was specified as a union, so a
//     team binding can only ever *add* to what the user already had -- there is no way to
//     configure a scope that takes models away, and that is a deliberate property.
//
//  2. **An empty group contributes nothing, so the union is empty only when every contributor
//     is.** That is what makes "a newly signed-in user gets an empty group" work: set
//     default_group to an empty group and a user with no other binding resolves to *nothing*,
//     which is refused outright (403 on a call, an empty list on /v1/models).
//
//  3. **No binding at all means unrestricted.** If the policy is enabled, default_group is
//     unset, and no scope binding matches, the caller gets the whole catalog. Enabling the
//     toggle before filling the tables in must not lock the deployment out of itself.
//
//  4. **Administrators are exempt.** Same posture as the key policy: an admin's authority comes
//     from auth.admin_logins (or the local admin account), and a model policy is a distribution
//     control, not a privilege boundary.
//
// How the effective set is *enforced* is not in this package. The router applies it by
// narrowing the catalog (RouterConfig.RestrictedTo) before any routing decision runs.
//
// Membership answers come from internal/ghcache, cache-first with a live fallback, exactly as
// the key policy does. Unlike the key policy, a membership lookup that cannot be answered here
// fails *open* for that one scope: it simply does not contribute. Failing closed would be wrong
// under a union, because the union's failure mode is "the user sees fewer models than the
// operator granted", and silently narrowing someone's model list on a transient GitHub error is
// worse than briefly including a scope they may have left.
package modelpolicy

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// Resolution is on the hot path -- every /v1/chat/completions and every /v1/models call needs
// an effective set -- and each scope lookup reads a JSON file. So results are memoised per
// login for a short window. 60s bounds how long a membership or policy change takes to show up
// in routing; a configuration save calls Invalidate and does not wait for it.
const ttl = 60 * time.Second

// maxEntries bounds the map on a deployment with many distinct callers.
const maxEntries = 5000

type cacheEntry struct {
	expires time.Time
	verdict *omap.Map
}

var (
	mu    sync.Mutex
	cache = map[string]cacheEntry{}
)

// Invalidate drops every memoised verdict. Called when the configuration changes.
func Invalidate() {
	mu.Lock()
	defer mu.Unlock()
	cache = map[string]cacheEntry{}
}

func cached(login string) *omap.Map {
	mu.Lock()
	defer mu.Unlock()
	entry, ok := cache[login]
	if ok && entry.expires.After(time.Now()) {
		return entry.verdict
	}
	return nil
}

func store(login string, verdict *omap.Map) {
	mu.Lock()
	defer mu.Unlock()
	if len(cache) >= maxEntries {
		cache = map[string]cacheEntry{}
	}
	cache[login] = cacheEntry{expires: time.Now().Add(ttl), verdict: verdict}
}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// teamTarget splits a `teams` key into (enterprise slug, team id).
//
// ghcache identifies an enterprise team by both parts, so a bare team id cannot be looked up. A
// key without a slash is reported rather than guessed at.
func teamTarget(key string) (string, string, bool) {
	slug, teamID, _ := strings.Cut(key, "/")
	slug, teamID = strings.TrimSpace(slug), strings.TrimSpace(teamID)
	if slug == "" || teamID == "" {
		return "", "", false
	}
	return slug, teamID, true
}

// order returns catalog order, so the list a user sees matches the Models page rather than map
// iteration order.
func order(cfg *config.RouterConfig, names map[string]bool) []any {
	out := []any{}
	for _, name := range cfg.Models.Keys() {
		if names[name] {
			out = append(out, name)
		}
	}
	return out
}

func allModels(cfg *config.RouterConfig) []any {
	out := make([]any, 0, cfg.Models.Len())
	for _, name := range cfg.Models.Keys() {
		out = append(out, name)
	}
	return out
}

// Evaluate resolves the effective model set for `login`.
//
// Returns:
//
//	enabled        whether the policy is switched on at all
//	unrestricted   true when the whole catalog applies (policy off, admin, or no binding)
//	models         the effective model names, in catalog order
//	default_group  the group every signed-in user starts from ('' when unset)
//	contributions  one row per grant that applied: {scope, name, group, models, source}
//	reason         a short machine-readable explanation of which of the above decided
//
// Only contributions that *applied* are returned. A regular user's own view is the main
// consumer, and listing the teams and organizations they are **not** in would publish the
// policy tables to everybody who can sign in.
func Evaluate(ctx context.Context, cfg *config.RouterConfig, login string, isAdmin bool) *omap.Map {
	login = strings.ToLower(strings.TrimSpace(login))
	if !cfg.ModelPolicyEnabled() {
		return mapOf(
			"enabled", false,
			"unrestricted", true,
			"models", allModels(cfg),
			"default_group", cfg.DefaultGroup(),
			"contributions", []any{},
			"reason", "policy-disabled",
		)
	}
	if isAdmin {
		return mapOf(
			"enabled", true,
			"unrestricted", true,
			"models", allModels(cfg),
			"default_group", cfg.DefaultGroup(),
			"contributions", []any{},
			"reason", "administrator",
		)
	}
	if verdict := cached(login); verdict != nil {
		return verdict
	}

	policy := cfg.ModelPolicy
	contributions := []any{}
	allowed := map[string]bool{}

	contribute := func(scope, name, group, source string) {
		members := cfg.GroupModels(group)
		memberList := make([]any, len(members))
		for i, model := range members {
			allowed[model] = true
			memberList[i] = model
		}
		contributions = append(contributions, mapOf(
			"scope", scope, "name", name, "group", group,
			"models", memberList, "source", source,
		))
	}

	// 1. The default every signed-in user starts from.
	if group := cfg.DefaultGroup(); group != "" && cfg.HasModelGroup(group) {
		contribute("default", group, group, "config")
	}

	// 2. A binding on this exact login.
	if users := policy.Map("users"); users != nil {
		for _, key := range users.Keys() {
			if strings.ToLower(strings.TrimSpace(key)) != login {
				continue
			}
			group := omap.AsString(users.Value(key))
			if group != "" && cfg.HasModelGroup(group) {
				contribute("user", login, group, "config")
			}
			break
		}
	}

	// 3. Teams and organizations, whose bindings only apply on proven membership. Every lookup
	//    runs concurrently: they are independent, and a serial walk over a dozen scopes would
	//    put a dozen round trips on the first uncached request of each user.
	type binding struct{ name, group string }
	collect := func(field string) []binding {
		out := []binding{}
		table := policy.Map(field)
		if table == nil {
			return out
		}
		for _, name := range table.Keys() {
			group := omap.AsString(table.Value(name))
			if group != "" && cfg.HasModelGroup(group) {
				out = append(out, binding{name: name, group: group})
			}
		}
		return out
	}
	orgs := collect("organizations")
	teams := collect("teams")

	type answer struct {
		member bool
		source string
	}
	orgResults := make([]answer, len(orgs))
	teamResults := make([]answer, len(teams))
	var wg sync.WaitGroup
	for i, org := range orgs {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil { // see the package docstring: one scope fails open
					log.Printf("WARNING mr: model policy: org %s membership check failed: %v", name, r)
					orgResults[i] = answer{false, "error"}
				}
			}()
			member, source := ghcache.IsOrgMember(ctx, cfg, name, login)
			orgResults[i] = answer{member, source}
		}(i, org.name)
	}
	for i, team := range teams {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			slug, teamID, ok := teamTarget(key)
			if !ok {
				log.Printf("WARNING mr: model policy: teams key %q is not '<enterprise-slug>/<team-id>', ignoring", key)
				teamResults[i] = answer{false, "malformed"}
				return
			}
			defer func() {
				if r := recover(); r != nil {
					log.Printf("WARNING mr: model policy: team %s membership check failed: %v", key, r)
					teamResults[i] = answer{false, "error"}
				}
			}()
			member, source := ghcache.IsTeamMember(ctx, cfg, slug, teamID, login)
			teamResults[i] = answer{member, source}
		}(i, team.name)
	}
	wg.Wait()

	for i, org := range orgs {
		if orgResults[i].member {
			contribute("organization", org.name, org.group, orgResults[i].source)
		}
	}
	for i, team := range teams {
		if teamResults[i].member {
			contribute("team", team.name, team.group, teamResults[i].source)
		}
	}

	var verdict *omap.Map
	if len(contributions) == 0 {
		// Nothing is bound to this caller at all. See point 3 of the package docstring: this is
		// "unconfigured", not "configured to nothing", and it must not lock anyone out.
		verdict = mapOf(
			"enabled", true,
			"unrestricted", true,
			"models", allModels(cfg),
			"default_group", cfg.DefaultGroup(),
			"contributions", []any{},
			"reason", "no-binding",
		)
	} else {
		reason := "empty-group"
		if len(allowed) > 0 {
			reason = "union"
		}
		verdict = mapOf(
			"enabled", true,
			"unrestricted", false,
			"models", order(cfg, allowed),
			"default_group", cfg.DefaultGroup(),
			"contributions", contributions,
			"reason", reason,
		)
	}
	store(login, verdict)
	return verdict
}

// AllowedModels returns the effective model names, or nil when the caller is unrestricted.
//
// nil rather than the full catalog, so a caller can tell "no policy applies" from "the policy
// happens to allow everything" -- the router only narrows its catalog for the former.
func AllowedModels(ctx context.Context, cfg *config.RouterConfig, login string, isAdmin bool) []string {
	verdict := Evaluate(ctx, cfg, login, isAdmin)
	if verdict.Bool("unrestricted", false) {
		return nil
	}
	models := verdict.Slice("models")
	out := make([]string, 0, len(models))
	for _, item := range models {
		out = append(out, omap.AsString(item))
	}
	return out
}
