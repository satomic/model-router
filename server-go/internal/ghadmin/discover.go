package ghadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/omap"
)

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// Enterprise is one enterprise visible to the token.
type Enterprise struct {
	Slug string
	Name string
	ID   string
}

// ListEnterprises lists the enterprises visible to the token. REST cannot do this; GraphQL only.
func ListEnterprises(ctx context.Context, token string) ([]Enterprise, error) {
	key := "ents\x00" + token
	if cached, ok := cache.get(key, discoveryTTL); ok {
		return cached.([]Enterprise), nil
	}
	payload, err := graphQL(ctx, token,
		"query { viewer { enterprises(first: 50) { nodes { slug name id } } } }", nil)
	if err != nil {
		return nil, err
	}
	result := []Enterprise{}
	if data := payload.Map("data"); data != nil {
		if viewer := data.Map("viewer"); viewer != nil {
			if enterprises := viewer.Map("enterprises"); enterprises != nil {
				for _, item := range enterprises.Slice("nodes") {
					node, ok := item.(*omap.Map)
					if !ok || node.Str("slug") == "" {
						continue
					}
					name := node.Str("name")
					if name == "" {
						name = node.Str("slug")
					}
					result = append(result, Enterprise{Slug: node.Str("slug"), Name: name, ID: node.Str("id")})
				}
			}
		}
	}
	cache.put(key, result)
	return result, nil
}

// OrgListing is the orgs of an enterprise plus how complete the listing is.
type OrgListing struct {
	Organizations []*omap.Map
	Total         int
	Truncated     bool
	Error         string
}

const orgQuery = `
query($slug: String!, $first: Int!, $after: String) {
  enterprise(slug: $slug) {
    organizations(first: $first, after: $after, orderBy: {field: LOGIN, direction: ASC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes { login name }
    }
  }
}`

// ListEnterpriseOrgs returns the orgs of an enterprise.
//
// The org count can be enormous, so paging stops at maxOrgs and sets Truncated -- better to
// report truncation explicitly than to silently list fewer (which would let an admin believe
// some org does not exist).
func ListEnterpriseOrgs(ctx context.Context, token, slug string) OrgListing {
	key := "orgs\x00" + token + "\x00" + slug
	if cached, ok := cache.get(key, discoveryTTL); ok {
		return cached.(OrgListing)
	}
	orgs := []*omap.Map{}
	total := 0
	truncated := false
	errText := ""
	var cursor any
	for {
		variables := mapOf("slug", slug, "first", json.Number(fmt.Sprintf("%d", orgPage)), "after", cursor)
		payload, err := graphQL(ctx, token, orgQuery, variables)
		if err != nil {
			errText = err.Error()
			break
		}
		data := payload.Map("data")
		var enterprise *omap.Map
		if data != nil {
			enterprise = data.Map("enterprise")
		}
		if enterprise == nil {
			// Common on very large enterprises: data.enterprise is null plus
			// RESOURCE_LIMITS_EXCEEDED.
			errText = "the enterprise organization list is unavailable"
			if errs := payload.Slice("errors"); len(errs) > 0 {
				if first, ok := errs[0].(*omap.Map); ok && first.Str("message") != "" {
					errText = first.Str("message")
				}
			}
			break
		}
		conn := enterprise.Map("organizations")
		if conn == nil {
			break
		}
		total = conn.Int("totalCount", 0)
		for _, item := range conn.Slice("nodes") {
			node, ok := item.(*omap.Map)
			if !ok || node.Str("login") == "" {
				continue
			}
			name := node.Str("name")
			if name == "" {
				name = node.Str("login")
			}
			orgs = append(orgs, mapOf("login", node.Str("login"), "name", name))
		}
		page := conn.Map("pageInfo")
		hasNext := page != nil && page.Bool("hasNextPage", false)
		if !hasNext || len(orgs) >= maxOrgs {
			truncated = hasNext
			break
		}
		cursor = page.Value("endCursor")
	}
	if total == 0 {
		total = len(orgs)
	}
	result := OrgListing{Organizations: orgs, Total: total, Truncated: truncated, Error: errText}
	cache.put(key, result)
	return result
}

// TeamListing is the enterprise teams plus the error that explains an empty list.
type TeamListing struct {
	Teams []*omap.Map
	Error string
}

// ListEnterpriseTeams returns the enterprise teams. This endpoint 404s on some enterprises
// (which is not the same as "has no teams"), so an error is distinguished from an empty list.
//
// Membership checks require the numeric id, not the `ent:`-prefixed slug (measured: the slug
// 404s), so the id is kept here and the policy layer stores ids too.
func ListEnterpriseTeams(ctx context.Context, token, slug string) TeamListing {
	key := "teams\x00" + token + "\x00" + slug
	if cached, ok := cache.get(key, discoveryTTL); ok {
		return cached.(TeamListing)
	}
	teams := []*omap.Map{}
	errText := ""
	status, body, err := Rest(ctx, token, "/enterprises/"+url.PathEscape(slug)+"/teams?per_page=100", true)
	switch {
	case err != nil:
		errText = err.Error()
	case status == 404:
		errText = "this enterprise does not support the Enterprise Teams endpoint (GitHub returned 404)"
	default:
		if items, ok := body.([]any); ok {
			for _, item := range items {
				team, ok := item.(*omap.Map)
				if !ok || team.Value("id") == nil {
					continue
				}
				name := team.Str("name")
				if name == "" {
					name = team.Str("slug")
				}
				if name == "" {
					name = omap.AsString(team.Value("id"))
				}
				teams = append(teams, mapOf("id", team.Value("id"), "slug", team.Value("slug"), "name", name))
			}
		}
	}
	result := TeamListing{Teams: teams, Error: errText}
	cache.put(key, result)
	return result
}

// Discover fetches everything the configuration page needs in one go: enterprises plus each
// one's orgs and teams.
func Discover(ctx context.Context, token string) (*omap.Map, error) {
	enterprises, err := ListEnterprises(ctx, token)
	if err != nil {
		return nil, err
	}
	// Fetched concurrently; the enterprise count is small in practice, so this does not
	// trigger rate limiting.
	orgResults := make([]OrgListing, len(enterprises))
	teamResults := make([]TeamListing, len(enterprises))
	var wg sync.WaitGroup
	for i, enterprise := range enterprises {
		wg.Add(2)
		go func(i int, slug string) { defer wg.Done(); orgResults[i] = ListEnterpriseOrgs(ctx, token, slug) }(i, enterprise.Slug)
		go func(i int, slug string) { defer wg.Done(); teamResults[i] = ListEnterpriseTeams(ctx, token, slug) }(i, enterprise.Slug)
	}
	wg.Wait()

	out := []any{}
	for i, enterprise := range enterprises {
		orgs := orgResults[i]
		teams := teamResults[i]
		entry := mapOf(
			"slug", enterprise.Slug,
			"name", enterprise.Name,
			"id", enterprise.ID,
			"organizations", toAnySlice(orgs.Organizations),
			"organizations_total", json.Number(fmt.Sprintf("%d", orgs.Total)),
			"organizations_truncated", orgs.Truncated,
			"organizations_error", nilIfEmpty(orgs.Error),
			"teams", toAnySlice(teams.Teams),
			"teams_error", nilIfEmpty(teams.Error),
		)
		out = append(out, entry)
	}
	return mapOf("enterprises", out), nil
}

func toAnySlice(items []*omap.Map) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// -- Membership checks --------------------------------------------------------

// CheckOrgMember reports org membership -- the most reliable and cheapest route.
//
// /orgs/{org}/members/{login}: 204 = yes, 404 = no, **302 = this token cannot see that org's
// members** (GitHub uses a redirect for "not allowed to look", which does not mean the user is
// not a member). On 302, fall back to /orgs/{org}/memberships/{login} (needs admin:org) for a
// definite answer; when neither can answer, treat it as fail-closed -- better to withhold a key
// than to hand one out wrongly.
func CheckOrgMember(ctx context.Context, token, org, login string) bool {
	key := strings.Join([]string{"org-member", token, strings.ToLower(org), strings.ToLower(login)}, "\x00")
	if cached, ok := cache.get(key, membershipTTL); ok {
		return cached.(bool)
	}
	status, _, err := Rest(ctx, token,
		fmt.Sprintf("/orgs/%s/members/%s", url.PathEscape(org), url.PathEscape(login)), true)
	if err != nil {
		log.Printf("WARNING mr: org membership check failed org=%s login=%s: %v", org, login, err)
		return false
	}
	var result bool
	if status == 302 {
		result = orgMemberViaMembership(ctx, token, org, login)
	} else {
		result = status == 204
	}
	cache.put(key, result)
	return result
}

// orgMemberViaMembership is the fallback when members/ returns 302: only 200 with state=active
// counts.
func orgMemberViaMembership(ctx context.Context, token, org, login string) bool {
	status, body, err := Rest(ctx, token,
		fmt.Sprintf("/orgs/%s/memberships/%s", url.PathEscape(org), url.PathEscape(login)), true)
	if err != nil {
		log.Printf("WARNING mr: org membership check unavailable (token cannot see this org) org=%s login=%s: %v", org, login, err)
		return false
	}
	data, ok := body.(*omap.Map)
	if status != 200 || !ok {
		return false
	}
	return data.Str("state") == "active"
}

// CheckEnterpriseTeamMember reports enterprise team membership: 200 = member; everything else
// (404 / 302 not-allowed-to-look) counts as not a member. The numeric team id is required.
//
// Only 200 is accepted on purpose -- an empty-bodied 2xx/3xx is not evidence of membership, and
// fail-closed beats issuing a key by mistake.
func CheckEnterpriseTeamMember(ctx context.Context, token, slug, teamID, login string) bool {
	key := strings.Join([]string{"team-member", token, slug, teamID, strings.ToLower(login)}, "\x00")
	if cached, ok := cache.get(key, membershipTTL); ok {
		return cached.(bool)
	}
	status, _, err := Rest(ctx, token, fmt.Sprintf("/enterprises/%s/teams/%s/memberships/%s",
		url.PathEscape(slug), url.PathEscape(teamID), url.PathEscape(login)), true)
	if err != nil {
		log.Printf("WARNING mr: enterprise team membership check failed ent=%s team=%s login=%s: %v",
			slug, teamID, login, err)
		return false
	}
	result := status == 200
	cache.put(key, result)
	return result
}

const entMemberQuery = `
query($slug: String!, $q: String!) {
  enterprise(slug: $slug) {
    members(first: 10, query: $q) {
      nodes {
        __typename
        ... on EnterpriseUserAccount { login }
        ... on User { login }
      }
    }
  }
}`

// CheckEnterpriseMember reports enterprise membership. **Tri-state**: true / false / nil
// (GitHub cannot answer).
//
// nil occurs on very large enterprises: GraphQL reports RESOURCE_LIMITS_EXCEEDED (even with a
// login filter) and consumed-licenses returns 404. Callers must treat nil as "no match" and
// fall back to org/team checks -- never as true.
func CheckEnterpriseMember(ctx context.Context, token, slug, login string) *bool {
	key := strings.Join([]string{"ent-member", token, slug, strings.ToLower(login)}, "\x00")
	if cached, ok := cache.get(key, membershipTTL); ok {
		if text, isText := cached.(string); isText && text == "unknown" {
			return nil
		}
		if value, isBool := cached.(bool); isBool {
			return &value
		}
	}

	var result *bool
	// Route 1: login-filtered GraphQL member query (works on small/medium enterprises).
	payload, err := graphQL(ctx, token, entMemberQuery, mapOf("slug", slug, "q", login))
	if err != nil {
		log.Printf("INFO mr: enterprise member check via GraphQL unavailable ent=%s: %v", slug, err)
	} else if data := payload.Map("data"); data != nil {
		if enterprise := data.Map("enterprise"); enterprise != nil {
			found := false
			if members := enterprise.Map("members"); members != nil {
				for _, item := range members.Slice("nodes") {
					if node, ok := item.(*omap.Map); ok && strings.EqualFold(node.Str("login"), login) {
						found = true
					}
				}
			}
			result = &found
		}
	}

	// Route 2: consumed-licenses (fallback when GraphQL is unavailable; can also 404).
	if result == nil {
		status, body, err := Rest(ctx, token,
			"/enterprises/"+url.PathEscape(slug)+"/consumed-licenses?per_page=100", true)
		if err != nil {
			log.Printf("INFO mr: enterprise member check via licenses unavailable ent=%s: %v", slug, err)
		} else if data, ok := body.(*omap.Map); ok && status != 404 {
			found := false
			for _, item := range data.Slice("users") {
				if user, ok := item.(*omap.Map); ok && strings.EqualFold(user.Str("github_com_login"), login) {
					found = true
				}
			}
			result = &found
		}
	}

	if result == nil {
		cache.put(key, "unknown")
	} else {
		cache.put(key, *result)
	}
	return result
}
