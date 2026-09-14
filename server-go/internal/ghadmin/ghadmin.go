// Package ghadmin is the API client for GitHub Enterprise / Organization access control.
//
// Purpose: by default, once GitHub OAuth is configured, **any** GitHub account can sign in to
// this service. This package uses an enterprise admin PAT to ask GitHub which Enterprise /
// Enterprise Team / Organization a signed-in user actually belongs to, and decides from that
// whether they may create an API key (and therefore use BYOK). Decisions always rest on
// GitHub's live data; the client is never trusted.
//
// Measured capability differences per endpoint (they dictate the shape of the code below --
// do not "simplify" it on the assumption that they behave alike):
//
//	| Goal                       | Route                                                    | Note |
//	|----------------------------|----------------------------------------------------------|------|
//	| List enterprises for a PAT | GraphQL viewer.enterprises                               | no REST equivalent |
//	| Orgs of an enterprise      | GraphQL enterprise(slug:).organizations                  | paginated |
//	| Enterprise teams           | REST /enterprises/{slug}/teams                           | **404s on some enterprises** |
//	| Enterprise membership      | filtered GraphQL -> REST consumed-licenses               | **both unusable on large enterprises** |
//	| Org membership             | REST /orgs/{org}/members/{user}                          | 204 / 404 |
//	| Enterprise team membership | REST /enterprises/{slug}/teams/{id}/memberships/{user}   | 200 / 404 |
//
// Key constraint: enterprise-level membership is **not** determinable on every enterprise. On
// very large ones GraphQL reports RESOURCE_LIMITS_EXCEEDED (even with a login filter) and REST
// consumed-licenses returns 404. So CheckEnterpriseMember is **tri-state** (yes / no / cannot
// tell), "cannot tell" is handled fail-closed, and authorization relies only on the two
// reliable routes: orgs and teams.
package ghadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/omap"
)

const (
	API     = "https://api.github.com"
	GraphQL = API + "/graphql"
	accept  = "application/vnd.github+json"
	timeout = 20 * time.Second
)

// Same wording for every 401, so it is stated once here.
const errToken401 = "token is invalid or expired (GitHub 401)"

const (
	// Cache lifetime for discovery results (enterprise/org/team listings): the admin
	// configuration page reads them repeatedly and these structures rarely change.
	discoveryTTL = 300 * time.Second
	// Membership results are cached for less time: a user removed from an org should lose the
	// ability to create keys reasonably quickly.
	membershipTTL = 60 * time.Second

	// How many orgs to enumerate per enterprise at most: some enterprises have a huge number
	// of orgs, and paging through all of them saturates GraphQL's secondary rate limit.
	// Truncation is reported explicitly to the caller.
	maxOrgs = 200
	orgPage = 100
	// Ceiling on a single member list. An org with more members than this is not listed
	// exhaustively, and the caller is told so via `truncated`.
	maxMembers = 5000
	memberPage = 100
)

// Error reports a GitHub call that failed in a way the administrator needs to see.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func errf(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }

// IsError reports whether err came from this package.
func IsError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}

// -- Cache --------------------------------------------------------------------

type cacheEntry struct {
	at    time.Time
	value any
}

type ttlCache struct {
	mu   sync.Mutex
	data map[string]cacheEntry
}

func (c *ttlCache) get(key string, ttl time.Duration) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.data[key]
	if !ok || time.Since(entry.at) > ttl {
		return nil, false
	}
	return entry.value, true
}

func (c *ttlCache) put(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = map[string]cacheEntry{}
	}
	c.data[key] = cacheEntry{at: time.Now(), value: value}
}

func (c *ttlCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = map[string]cacheEntry{}
}

var cache = &ttlCache{data: map[string]cacheEntry{}}

// InvalidateCache is called after the token or the policy changes, so stale identities or
// structures are not reused.
func InvalidateCache() { cache.clear() }

// noRedirect matters: membership endpoints express their answer in the **status code** with an
// empty body -- 204 = yes, 404 = no, 302 = the caller is not allowed to look. Following the
// redirect would turn that 302 into some other status.
var client = &http.Client{
	Timeout: timeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func headers(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
}

// Rest returns (status, body). body is nil when empty or not JSON.
func Rest(ctx context.Context, token, path string, allow404 bool) (int, any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, API+path, nil)
	if err != nil {
		return 0, nil, err
	}
	headers(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, errf("GitHub request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case 204, 302, 304, 404:
		if resp.StatusCode == 404 && !allow404 {
			return resp.StatusCode, nil, errf("GitHub returned 404: %s", path)
		}
		return resp.StatusCode, nil, nil
	case 401:
		return resp.StatusCode, nil, errf("%s", errToken401)
	case 403:
		return resp.StatusCode, nil, errf("token lacks permission or hit a rate limit (GitHub 403): %s", path)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil, errf("GitHub %d: %s", resp.StatusCode, path)
	}
	if len(body) == 0 {
		return resp.StatusCode, nil, nil
	}
	value, err := omap.FromJSON(body)
	if err != nil {
		// A non-JSON body (very rare) must not take down the whole decision chain.
		log.Printf("WARNING mr: GitHub returned a non-JSON response path=%s", path)
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, value, nil
}

// restPaged walks a paginated REST collection, returning (items, truncated, error text).
//
// A sibling of Rest rather than a wrapper: paging needs the `Link` response header, which Rest
// deliberately discards. Every other REST call here passes a bare per_page=100 and follows
// nothing -- harmless for a team listing that fits on one page, silently wrong for a member
// list, where "page 1 of 12" read as the whole thing would turn members into non-members.
//
// Errors are returned rather than raised: a partially-read list must be reported as incomplete
// (so the caller does not treat it as authoritative), not thrown away.
func restPaged(ctx context.Context, token, path string, cap int) ([]any, bool, string) {
	items := []any{}
	truncated := false
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := fmt.Sprintf("%s%s%sper_page=%d", API, path, sep, memberPage)
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return items, true, err.Error()
		}
		headers(req, token)
		resp, err := client.Do(req)
		if err != nil {
			return items, true, fmt.Sprintf("GitHub request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		linkHeader := resp.Header.Get("Link")
		status := resp.StatusCode
		resp.Body.Close()

		switch {
		case status == 401:
			return items, true, errToken401
		case status == 403:
			return items, true, fmt.Sprintf("token lacks permission or hit a rate limit (GitHub 403): %s", path)
		case status >= 400 || status == 302 || status == 304:
			// 302 here means the same thing as on the membership endpoints: the token is not
			// allowed to look. That is not an empty list.
			return items, true, fmt.Sprintf("GitHub %d: %s", status, path)
		}
		value, err := omap.FromJSON(body)
		if err != nil {
			return items, true, fmt.Sprintf("GitHub returned a non-JSON response: %s", path)
		}
		page, ok := value.([]any)
		if !ok {
			return items, true, fmt.Sprintf("GitHub returned an unexpected payload: %s", path)
		}
		items = append(items, page...)
		if len(items) >= cap {
			items = items[:cap]
			truncated = true
			break
		}
		next = nextLink(linkHeader)
	}
	return items, truncated, ""
}

// nextLink extracts rel="next" from a Link header.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 {
			continue
		}
		target := strings.TrimSpace(segments[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		for _, attr := range segments[1:] {
			attr = strings.TrimSpace(attr)
			if attr == `rel="next"` || attr == "rel=next" {
				return target[1 : len(target)-1]
			}
		}
	}
	return ""
}

// MemberList is a member roster plus how trustworthy it is.
type MemberList struct {
	Logins    []string
	Truncated bool
	Error     string
}

// ListOrgMembers returns every member login of an org.
//
// Logins are lower-cased here: GitHub logins are case-insensitive, and the whole point of
// caching the list is to answer membership as a set lookup, which needs one casing.
func ListOrgMembers(ctx context.Context, token, org string) MemberList {
	items, truncated, errText := restPaged(ctx, token, "/orgs/"+url.PathEscape(org)+"/members", maxMembers)
	seen := map[string]bool{}
	for _, item := range items {
		user, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		if login := user.Str("login"); login != "" {
			seen[strings.ToLower(login)] = true
		}
	}
	return MemberList{Logins: sortedKeys(seen), Truncated: truncated, Error: errText}
}

// ListEnterpriseTeamMembers returns every member login of an enterprise team.
//
// The numeric team id is required, exactly as in CheckEnterpriseTeamMember -- the
// `ent:`-prefixed slug 404s here too.
func ListEnterpriseTeamMembers(ctx context.Context, token, slug, teamID string) MemberList {
	path := fmt.Sprintf("/enterprises/%s/teams/%s/memberships", url.PathEscape(slug), url.PathEscape(teamID))
	items, truncated, errText := restPaged(ctx, token, path, maxMembers)
	seen := map[string]bool{}
	for _, item := range items {
		entry, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		// This endpoint's shape has varied: some responses nest the account under `user`,
		// others carry the login at the top level. Accept both rather than silently producing
		// an empty list against the variant we did not expect.
		login := entry.Str("login")
		if login == "" {
			if user := entry.Map("user"); user != nil {
				login = user.Str("login")
			}
		}
		if login != "" {
			seen[strings.ToLower(login)] = true
		}
	}
	return MemberList{Logins: sortedKeys(seen), Truncated: truncated, Error: errText}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sortStrings(out)
	return out
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// graphQL runs a GraphQL query and returns the payload; callers handle null fields themselves.
//
// GitHub's GraphQL returns data *and* errors together on partial success (e.g. one field
// exceeded resource limits), so this must not fail on sight of errors -- they are handed to
// the caller alongside the data.
func graphQL(ctx context.Context, token, query string, variables *omap.Map) (*omap.Map, error) {
	if variables == nil {
		variables = omap.New()
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GraphQL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	headers(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errf("GitHub request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return nil, errf("%s", errToken401)
	}
	if resp.StatusCode >= 400 {
		snippet := string(raw)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, errf("GitHub GraphQL %d: %s", resp.StatusCode, snippet)
	}
	value, err := omap.FromJSON(raw)
	if err != nil {
		return nil, errf("GitHub GraphQL returned a non-JSON response")
	}
	payload, ok := value.(*omap.Map)
	if !ok {
		return nil, errf("GitHub GraphQL returned an unexpected payload")
	}
	if payload.Value("data") == nil {
		if errs := payload.Slice("errors"); len(errs) > 0 {
			if first, ok := errs[0].(*omap.Map); ok {
				return nil, errf("GraphQL error: %s", first.Str("message"))
			}
		}
	}
	return payload, nil
}

// -- The token itself ---------------------------------------------------------

// VerifyToken validates the token and returns its owner and scopes, so the configuration page
// can confirm whose token this is.
func VerifyToken(ctx context.Context, token string) (*omap.Map, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, API+"/user", nil)
	if err != nil {
		return nil, err
	}
	headers(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errf("GitHub request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return nil, errf("%s", errToken401)
	}
	if resp.StatusCode >= 400 {
		return nil, errf("GitHub %d: /user", resp.StatusCode)
	}
	value, err := omap.FromJSON(body)
	if err != nil {
		return nil, errf("GitHub returned a non-JSON response: /user")
	}
	user, ok := value.(*omap.Map)
	if !ok {
		return nil, errf("GitHub returned an unexpected payload: /user")
	}
	// A classic PAT reports its scopes in a response header; fine-grained tokens do not.
	scopes := []any{}
	for _, scope := range strings.Split(resp.Header.Get("x-oauth-scopes"), ",") {
		if trimmed := strings.TrimSpace(scope); trimmed != "" {
			scopes = append(scopes, trimmed)
		}
	}
	// Listing enterprises needs admin:enterprise; when it is missing the frontend says so
	// explicitly instead of showing an empty list.
	hasEnterpriseScope := len(scopes) == 0
	for _, scope := range scopes {
		switch scope {
		case "admin:enterprise", "manage_billing:enterprise", "read:enterprise":
			hasEnterpriseScope = true
		}
	}
	name := user.Str("name")
	if name == "" {
		name = user.Str("login")
	}
	out := omap.New()
	out.Set("login", user.Value("login"))
	out.Set("name", name)
	out.Set("avatar_url", user.Value("avatar_url"))
	out.Set("scopes", scopes)
	out.Set("has_enterprise_scope", hasEnterpriseScope)
	return out, nil
}
