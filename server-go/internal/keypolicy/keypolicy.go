// Package keypolicy evaluates "who may create API keys".
//
// Background: once GitHub OAuth is configured, any GitHub account can sign in to this service,
// so signing in is not authorization by itself. The real gate sits at **creating an API key** --
// without a key you cannot call /v1/chat/completions, and therefore cannot use BYOK.
//
// Policy shape (auth.key_policy in config.yaml):
//
//	auth:
//	  key_policy:
//	    enabled: true                 # false = any signed-in user may create keys (default)
//	    github_token: 'ghp_...'       # enterprise admin PAT
//	    enterprises:
//	      satomic:
//	        enabled: true             # enterprise master switch
//	        allow_all_orgs: false     # true = membership in any org of the enterprise suffices
//	        organizations: [nekoaru]  # allowed orgs (inert while the master switch is off)
//	        teams: [14501973]         # allowed enterprise teams (numeric ids)
//
// Decision order, and why it is this order:
//  1. Admins (auth.admin_logins) pass immediately -- otherwise a misconfigured policy locks the
//     admin out too, and since admins can edit the policy anyway, blocking them buys no security.
//  2. Policy disabled -> allow (preserves the previous default so upgrades do not break existing
//     deployments).
//  3. Policy enabled but no token -> **deny**, with an explanation. Enabled but unable to query
//     GitHub means there is no evidence at all, and allowing here would make "turn on access
//     control" the least protected state.
//  4. For each enabled enterprise: check the allowed orgs first, then the allowed enterprise
//     teams. Any hit allows.
//  5. Enterprise-level membership is only attempted under allow_all_orgs, and it is tri-state --
//     "cannot tell" counts as no match, and evaluation falls back to probing the enumerated orgs
//     of that enterprise one by one.
//
// Failures are always fail-closed: better to withhold one key than to hand one out wrongly.
//
// Membership questions go through internal/ghcache rather than internal/ghadmin directly: a
// locally cached, complete member list answers them with zero GitHub calls, and anything less
// than a complete list falls through to exactly the live probe this package used to make.
// Every evidence row therefore carries `source` ("cache" / "probe" / "live") -- a decision
// whose provenance is invisible is a decision nobody can debug.
package keypolicy

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghadmin"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// Under allow_all_orgs, when enterprise-level membership is unavailable, how many orgs to probe
// individually at most. Very large enterprises have thousands of orgs, and probing all of them
// is both slow and rate-limited.
const maxOrgProbe = 30

// Verdict kind -> reason code, for the one verdict whose sentence names what was matched.
var memberCodes = map[string]string{
	"organization": "memberOrganization",
	"team":         "memberTeam",
	"enterprise":   "memberEnterprise",
}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// teamNames maps enterprise team id -> name.
//
// The policy stores **numeric ids** (the membership endpoint only accepts ids; a slug 404s),
// but an id means nothing to a user -- "14501973" does not say which team it is. So the id is
// swapped for a name right before display. The listing call is cached and is only reached when
// teams are actually configured, so this adds no extra GitHub traffic.
//
// Returns an empty map when the team listing is unavailable (that endpoint 404s on some
// enterprises); callers then fall back to showing the id.
func teamNames(ctx context.Context, token, slug string) map[string]string {
	listing := ghadmin.ListEnterpriseTeams(ctx, token, slug)
	if listing.Error != "" {
		// Names are cosmetic, so no failure here may affect the authorization decision.
		log.Printf("WARNING mr: failed to resolve enterprise team names ent=%s: %s", slug, listing.Error)
		return map[string]string{}
	}
	out := map[string]string{}
	for _, team := range listing.Teams {
		if team.Value("id") == nil {
			continue
		}
		id := omap.AsString(team.Value("id"))
		name := team.Str("name")
		if name == "" {
			name = team.Str("slug")
		}
		if name == "" {
			name = id
		}
		out[id] = name
	}
	return out
}

type match struct {
	kind       string
	enterprise string
	name       string
	id         string
}

// Evaluate returns {allowed, reason, reason_code, reason_params, detail, matched,
// policy_enabled}.
//
// `reason` is a single sentence for the user; `detail` is the item-by-item evidence (the UI
// renders it as "current permissions and limits"). Never put the token or any other credential
// into the return value.
//
// `reason_code` names the same verdict without saying it in any language, and `reason_params`
// carries the values the sentence interpolates. The console translates that pair, because this
// package is English-only on purpose: the sentence is what goes into the log lines and the 403
// bodies, where a reader's locale is not known and must not change the record.
func Evaluate(ctx context.Context, cfg *config.RouterConfig, login string, isAdmin bool) *omap.Map {
	policy := cfg.KeyPolicy
	if isAdmin {
		return mapOf(
			"allowed", true,
			"reason", "You are an administrator of this service and can create API keys directly.",
			"reason_code", "admin",
			"reason_params", omap.New(),
			"policy_enabled", policy.Bool("enabled", false),
			"matched", mapOf("kind", "admin"),
			"detail", []any{},
		)
	}

	if !policy.Bool("enabled", false) {
		return mapOf(
			"allowed", true,
			"reason", "Enterprise access control is disabled, so any signed-in account can create API keys.",
			"reason_code", "policyOff",
			"reason_params", omap.New(),
			"policy_enabled", false,
			"matched", nil,
			"detail", []any{},
		)
	}

	token := strings.TrimSpace(policy.Str("github_token"))
	if token == "" {
		return mapOf(
			"allowed", false,
			"reason", "Enterprise access control is enabled, but the administrator has not "+
				"configured a GitHub Enterprise token, so the service cannot verify your "+
				"enterprise membership. Please contact your administrator.",
			"reason_code", "noToken",
			"reason_params", omap.New(),
			"policy_enabled", true,
			"matched", nil,
			"detail", []any{},
		)
	}

	enterprises := policy.Map("enterprises")
	activeSlugs := []string{}
	if enterprises != nil {
		for _, slug := range enterprises.Keys() {
			if rule := enterprises.Map(slug); rule != nil && rule.Bool("enabled", false) {
				activeSlugs = append(activeSlugs, slug)
			}
		}
	}
	if len(activeSlugs) == 0 {
		return mapOf(
			"allowed", false,
			"reason", "Enterprise access control is enabled, but the administrator has not "+
				"allowed any enterprise yet. Please contact your administrator.",
			"reason_code", "noEnterprise",
			"reason_params", omap.New(),
			"policy_enabled", true,
			"matched", nil,
			"detail", []any{},
		)
	}

	detail := []any{}
	var matched *match

	for _, slug := range activeSlugs {
		rule := enterprises.Map(slug)
		orgs := trimmedList(rule.Value("organizations"))
		teams := trimmedList(rule.Value("teams"))

		// Org and team checks can run concurrently: each is at worst one REST call, and a cache
		// hit is a set lookup. Team-name resolution joins the same group -- it is just a listing
		// query and does not depend on the membership checks.
		type answer struct {
			member bool
			source string
		}
		orgResults := make([]answer, len(orgs))
		teamResults := make([]answer, len(teams))
		names := map[string]string{}
		var wg sync.WaitGroup
		for i, org := range orgs {
			wg.Add(1)
			go func(i int, org string) {
				defer wg.Done()
				member, source := ghcache.IsOrgMember(ctx, cfg, org, login)
				orgResults[i] = answer{member, source}
			}(i, org)
		}
		for i, team := range teams {
			wg.Add(1)
			go func(i int, team string) {
				defer wg.Done()
				member, source := ghcache.IsTeamMember(ctx, cfg, slug, team, login)
				teamResults[i] = answer{member, source}
			}(i, team)
		}
		if len(teams) > 0 {
			wg.Add(1)
			go func() { defer wg.Done(); names = teamNames(ctx, token, slug) }()
		}
		wg.Wait()

		for i, org := range orgs {
			detail = append(detail, mapOf(
				"enterprise", slug, "kind", "organization", "name", org,
				"member", orgResults[i].member, "source", orgResults[i].source,
			))
			if orgResults[i].member && matched == nil {
				matched = &match{kind: "organization", enterprise: slug, name: org}
			}
		}

		for i, team := range teams {
			// `name` carries the team name (which users understand); the id is kept separately
			// because admins still need it when troubleshooting, and because `name` has to fall
			// back to the id when the team listing is unavailable.
			name := names[team]
			if name == "" {
				name = team
			}
			detail = append(detail, mapOf(
				"enterprise", slug, "kind", "team", "name", name, "id", team,
				"member", teamResults[i].member, "source", teamResults[i].source,
			))
			if teamResults[i].member && matched == nil {
				matched = &match{kind: "team", enterprise: slug, name: name, id: team}
			}
		}

		if matched != nil {
			break
		}

		if rule.Bool("allow_all_orgs", false) {
			// Enterprise-level membership is tri-state: nil = GitHub cannot answer (very large
			// enterprises). Not cached here: this route is tri-state and unavailable on large
			// enterprises, so there is no member list to cache.
			entMember := ghadmin.CheckEnterpriseMember(ctx, token, slug, login)
			detail = append(detail, mapOf(
				"enterprise", slug, "kind", "enterprise", "name", slug,
				"member", boolOrNil(entMember), "source", ghcache.SourceLive,
			))
			if entMember != nil && *entMember {
				matched = &match{kind: "enterprise", enterprise: slug, name: slug}
				break
			}
			// When that is unanswerable (or the user is not a direct enterprise member), fall
			// back to checking the orgs of that enterprise one by one.
			discovered := ghadmin.ListEnterpriseOrgs(ctx, token, slug)
			known := map[string]bool{}
			for _, org := range orgs {
				known[org] = true
			}
			candidates := []string{}
			for _, item := range discovered.Organizations {
				login := item.Str("login")
				if !known[login] {
					candidates = append(candidates, login)
				}
				if len(candidates) >= maxOrgProbe {
					break
				}
			}
			if len(candidates) > 0 {
				probes := make([]answer, len(candidates))
				var probeGroup sync.WaitGroup
				for i, org := range candidates {
					probeGroup.Add(1)
					go func(i int, org string) {
						defer probeGroup.Done()
						member, source := ghcache.IsOrgMember(ctx, cfg, org, login)
						probes[i] = answer{member, source}
					}(i, org)
				}
				probeGroup.Wait()
				for i, org := range candidates {
					if probes[i].member {
						matched = &match{kind: "organization", enterprise: slug, name: org}
						detail = append(detail, mapOf(
							"enterprise", slug, "kind", "organization", "name", org,
							"member", true, "source", probes[i].source,
						))
						break
					}
				}
				if matched == nil {
					detail = append(detail, mapOf(
						"enterprise", slug, "kind", "org-scan", "name", slug,
						"member", false, "scanned", len(candidates),
						"truncated", len(discovered.Organizations) > len(candidates),
					))
				}
			}
			if matched != nil {
				break
			}
		}
	}

	if matched != nil {
		where := matched.name
		switch matched.kind {
		case "organization":
			where = "organization " + matched.name
		case "team":
			where = "enterprise team " + matched.name
		case "enterprise":
			where = "enterprise " + matched.name
		}
		matchedOut := mapOf("kind", matched.kind, "enterprise", matched.enterprise, "name", matched.name)
		if matched.id != "" {
			matchedOut.Set("id", matched.id)
		}
		code := memberCodes[matched.kind]
		if code == "" {
			code = "member"
		}
		return mapOf(
			"allowed", true,
			"reason", fmt.Sprintf("You are a member of %s (enterprise %s), so you can create API keys.",
				where, matched.enterprise),
			// One code per kind rather than one code carrying the kind: each reads as its own
			// sentence in the catalogs, and "organization X" is not a noun phrase every language
			// builds the same way.
			"reason_code", code,
			"reason_params", mapOf("name", matched.name, "enterprise", matched.enterprise),
			"policy_enabled", true,
			"matched", matchedOut,
			"detail", detail,
		)
	}

	return mapOf(
		"allowed", false,
		"reason", "You do not belong to any allowed enterprise, enterprise team or "+
			"organization, so you cannot create an API key and therefore cannot use "+
			"BYOK. To request access, ask your administrator to add your organization "+
			"to the allow list.",
		"reason_code", "noMembership",
		"reason_params", omap.New(),
		"policy_enabled", true,
		"matched", nil,
		"detail", detail,
	)
}

func trimmedList(value any) []string {
	out := []string{}
	for _, item := range omap.StringSlice(value) {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func boolOrNil(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}
