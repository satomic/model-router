// Package scopepolicy evaluates "who may narrow an API key's scope".
//
// A key scope (internal/keyscope) can only ever subtract from what its owner is allowed, so it
// is not a privilege escalation. It is a **cost** control in reverse: a user who scopes their
// key to one expensive model has pinned every request on that key to it, and the router's whole
// reason for existing -- sending the cheap requests to a cheap model -- stops applying to that
// key. So whether a user may set a scope at all is an administrator's decision, and the default
// is no.
//
// Policy shape (auth.key_scope_policy in config.yaml):
//
//	auth:
//	  key_scope_policy:
//	    enabled: false                   # false (the default) = nobody may narrow a key
//	    users: [alice]                   # GitHub logins
//	    teams: ['satomic/14501973']      # '<enterprise slug>/<team id>'
//	    organizations: [nekoaru]         # organization logins
//
// Decision order:
//
//  1. **Administrators pass.** Same posture as the key and model policies: an admin's authority
//     comes from auth.admin_logins (or the local admin account) and they can edit this very
//     policy, so blocking them buys nothing and a bad save would lock the operator out.
//  2. **Disabled -> deny.** This is the default and it is deliberately the *closed* one, which is
//     the opposite of the key policy's default. Scoping is an extra capability rather than the
//     pre-existing behaviour: a key with no scope reaches everything its owner may reach, which
//     is exactly what every key did before scopes existed, so denying by default changes nothing
//     for anybody and adds no cost risk.
//  3. **Enabled, with at least one of the three levels filled in:** every *configured* level must
//     match (AND), and within one level any single match is enough (OR).
//  4. **Enabled but all three levels empty -> deny**, with an explanation. Enabled-and-nothing-
//     listed is the same trap as the key policy's enabled-but-tokenless: reading it as "allow
//     everybody" would make switching the control on the least protected state it has.
//
// Why AND across the levels rather than the OR the key policy uses: that one answers "is this
// person one of ours", where any single proof of belonging is enough. This answers "may this
// person do a thing that costs money", where the levels are independent conditions an
// organization wants to be able to stack.
//
// An *unconfigured* level abstains rather than denying, which is the one reading that keeps the
// feature usable: under a strict "must match all three" an administrator who lists only an
// organization would grant nobody anything, because no login is in an empty user list.
//
// Failures are fail-closed: a membership lookup that cannot be answered denies the level it
// belongs to. The cost of being wrong in that direction is that a key covers everything its
// owner may reach, i.e. the documented default, whereas the other direction hands out the
// narrow key this policy exists to withhold.
package scopepolicy

import (
	"context"
	"log"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// Levels are the three levels, in the order the console shows them and the order the reason
// strings read in.
var Levels = []string{"user", "team", "organization"}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// entries reads the configured values of one level, trimmed and de-blanked, order preserved.
func entries(policy *omap.Map, field string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, item := range omap.StringSlice(policy.Value(field)) {
		text := strings.TrimSpace(item)
		if text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return out
}

// teamTarget splits a `teams` entry into (enterprise slug, team id).
//
// Same key format and same "report rather than guess" handling as the model policy: ghcache
// identifies an enterprise team by both parts, so a bare id cannot be looked up.
func teamTarget(key string) (string, string, bool) {
	slug, teamID, _ := strings.Cut(key, "/")
	slug, teamID = strings.TrimSpace(slug), strings.TrimSpace(teamID)
	if slug == "" || teamID == "" {
		return "", "", false
	}
	return slug, teamID, true
}

// Evaluate decides whether `login` may set a scope on their API keys.
//
// Returns:
//
//	allowed          the verdict
//	reason           one English sentence, for logs, 403 bodies and API callers
//	reason_code      the same verdict, machine-readable, so the console can translate it
//	reason_params    the values that sentence interpolates ({levels: [...]} where it has any)
//	policy_enabled   whether the control is switched on at all
//	levels           one row per level: {level, configured, passed, matched, source}
//
// Not memoised, unlike the model policy: this runs on key creation and on one console page,
// never on the request path, and the membership lookups underneath are already cache-first.
func Evaluate(ctx context.Context, cfg *config.RouterConfig, login string, isAdmin bool) *omap.Map {
	login = strings.ToLower(strings.TrimSpace(login))
	policy := cfg.KeyScopePolicy

	if isAdmin {
		return mapOf(
			"allowed", true,
			"reason", "Administrators may set a scope on any key.",
			"reason_code", "admin",
			"reason_params", omap.New(),
			"policy_enabled", policy.Bool("enabled", false),
			"levels", []any{},
		)
	}

	if !cfg.KeyScopePolicyEnabled() {
		return mapOf(
			"allowed", false,
			"reason", "Narrowing an API key is not enabled on this deployment, so every key "+
				"covers all models and all connection types. Please contact your "+
				"administrator.",
			"reason_code", "off",
			"reason_params", omap.New(),
			"policy_enabled", false,
			"levels", []any{},
		)
	}

	users := entries(policy, "users")
	teams := entries(policy, "teams")
	orgs := entries(policy, "organizations")

	if len(users) == 0 && len(teams) == 0 && len(orgs) == 0 {
		return mapOf(
			"allowed", false,
			"reason", "Narrowing an API key is enabled, but the administrator has not allowed "+
				"any user, team or organization yet, so every key covers all models and "+
				"all connection types. Please contact your administrator.",
			"reason_code", "nobodyAllowed",
			"reason_params", omap.New(),
			"policy_enabled", true,
			"levels", []any{},
		)
	}

	type answer struct {
		member bool
		source string
	}
	// Team and organization membership are independent lookups, so they run concurrently: a
	// serial walk would put one round trip per scope on the first request after a cache miss.
	teamResults := make([]answer, len(teams))
	orgResults := make([]answer, len(orgs))
	var wg sync.WaitGroup
	for i, key := range teams {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			slug, teamID, ok := teamTarget(key)
			if !ok {
				log.Printf("WARNING mr: key scope policy: teams entry %q is not '<enterprise-slug>/<team-id>', ignoring", key)
				teamResults[i] = answer{false, "malformed"}
				return
			}
			member, source := ghcache.IsTeamMember(ctx, cfg, slug, teamID, login)
			teamResults[i] = answer{member, source}
		}(i, key)
	}
	for i, org := range orgs {
		wg.Add(1)
		go func(i int, org string) {
			defer wg.Done()
			member, source := ghcache.IsOrgMember(ctx, cfg, org, login)
			orgResults[i] = answer{member, source}
		}(i, org)
	}
	wg.Wait()

	firstMatch := func(names []string, results []answer) (string, string) {
		for i, name := range names {
			if results[i].member {
				return name, results[i].source
			}
		}
		return "", ""
	}

	userMatch := ""
	for _, candidate := range users {
		if strings.ToLower(strings.TrimSpace(candidate)) == login {
			userMatch = candidate
			break
		}
	}
	teamMatch, teamSource := firstMatch(teams, teamResults)
	orgMatch, orgSource := firstMatch(orgs, orgResults)

	userSource := ""
	if userMatch != "" {
		userSource = "config"
	}
	levels := []any{
		mapOf("level", "user", "configured", len(users) > 0,
			"passed", len(users) == 0 || userMatch != "", "matched", userMatch, "source", userSource),
		mapOf("level", "team", "configured", len(teams) > 0,
			"passed", len(teams) == 0 || teamMatch != "", "matched", teamMatch, "source", teamSource),
		mapOf("level", "organization", "configured", len(orgs) > 0,
			"passed", len(orgs) == 0 || orgMatch != "", "matched", orgMatch, "source", orgSource),
	}

	failed := []any{}
	failedNames := []string{}
	for _, item := range levels {
		row := item.(*omap.Map)
		if row.Bool("configured", false) && !row.Bool("passed", false) {
			failed = append(failed, row.Str("level"))
			failedNames = append(failedNames, row.Str("level"))
		}
	}
	if len(failed) > 0 {
		return mapOf(
			"allowed", false,
			// Which levels failed, because the user's only route to the capability is asking an
			// administrator for it, and for that they need to know what to ask to be added to.
			"reason", "You are not allowed to narrow an API key: the administrator requires a "+
				"match at "+join(failedNames)+" level, and you do not have one. Every key "+
				"you create covers all models and all connection types. Please contact your "+
				"administrator.",
			"reason_code", "levelsFailed",
			// The level names, not the joined English phrase: the console names and joins them
			// in the reader's language, where the list separator is not a comma everywhere.
			"reason_params", mapOf("levels", failed),
			"policy_enabled", true,
			"levels", levels,
		)
	}

	return mapOf(
		"allowed", true,
		"reason", "You may restrict an API key to specific models or connection types.",
		"reason_code", "allowed",
		"reason_params", omap.New(),
		"policy_enabled", true,
		"levels", levels,
	)
}

// join renders 'the user', 'the user and team', 'the user, team and organization' -- reason
// strings only.
func join(levels []string) string {
	if len(levels) == 1 {
		return "the " + levels[0]
	}
	return "the " + strings.Join(levels[:len(levels)-1], ", ") + " and " + levels[len(levels)-1]
}
