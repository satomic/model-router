package ghcache

import (
	"context"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghadmin"
	"github.com/satomic/model-router/server-go/internal/jsonfile"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// -- Structure, for the configuration page -------------------------------------

// CachedStructure is the stored discover() payload, or nil when there is nothing usable.
//
// Age is deliberately *not* a disqualifier here: this feeds a configuration page that reports
// the fetch time next to the data, and showing a day-old org list beats making the admin wait
// on GitHub for every page load. Membership decisions apply the age check (see listAnswers);
// displaying a structure does not need to.
func CachedStructure(cfg *config.RouterConfig) *omap.Map {
	data := loadStructure()
	if data.Str("token_fp") != TokenFP(cfg.GHAdminToken()) {
		return nil
	}
	enterprises := data.Slice("enterprises")
	if len(enterprises) == 0 {
		return nil
	}
	return mapOf(
		"enterprises", enterprises,
		"cached", true,
		"fetched_at", data.Value("fetched_at"),
	)
}

// StoreStructure persists a structure that was just fetched live, so the next read is free.
//
// Member lists are deliberately left alone: they belong to Refresh, which is the only thing
// that knows whether each one came back complete.
func StoreStructure(cfg *config.RouterConfig, enterprises []any) {
	if len(enterprises) == 0 {
		return
	}
	_ = jsonfile.Write(StructurePath, mapOf(
		"fetched_at", fnum(now()),
		"token_fp", TokenFP(cfg.GHAdminToken()),
		"enterprises", enterprises,
		"error", "",
	))
}

// Status is everything the admin UI needs to judge the cache: ages, counts, truncation, errors.
//
// Never returns logins -- an org's member list is not something the console needs to render,
// and a cache status panel is the wrong place to publish one.
func Status(cfg *config.RouterConfig) *omap.Map {
	structure := loadStructure()
	members := loadMembers()
	probes := loadProbes()
	stamp := now()

	scopes := []any{}
	if entries := members.Map("entries"); entries != nil {
		keys := entries.Keys()
		sort.Strings(keys)
		for _, key := range keys {
			entry := entries.Map(key)
			if entry == nil {
				entry = omap.New()
			}
			kind, name, _ := strings.Cut(key, ":")
			scopes = append(scopes, mapOf(
				"key", key,
				"kind", kind,
				"name", name,
				"count", num(len(entry.Slice("logins"))),
				"truncated", entry.Bool("truncated", false),
				"error", entry.Str("error"),
				"fetched_at", numberOrZero(entry.Value("fetched_at")),
			))
		}
	}

	ents := []any{}
	for _, item := range structure.Slice("enterprises") {
		enterprise, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		name := enterprise.Str("name")
		if name == "" {
			name = enterprise.Str("slug")
		}
		errText := enterprise.Str("organizations_error")
		if errText == "" {
			errText = enterprise.Str("teams_error")
		}
		ents = append(ents, mapOf(
			"slug", enterprise.Value("slug"),
			"name", name,
			"organizations", num(len(enterprise.Slice("organizations"))),
			"organizations_truncated", enterprise.Bool("organizations_truncated", false),
			"teams", num(len(enterprise.Slice("teams"))),
			"error", errText,
		))
	}

	structureAt := structure.Float("fetched_at", 0)
	membersAt := members.Float("fetched_at", 0)
	interval := DefaultRefreshSeconds
	if cfg != nil {
		interval = RefreshSeconds(cfg)
	}
	// Staleness is judged per scope as well as on the document, because listAnswers ages each
	// entry against its own fetched_at: an entry that has stopped answering membership must not
	// sit behind a panel that calls the cache fresh because *some* refresh ran recently. The
	// oldest scope decides.
	oldestScope := membersAt
	for i, item := range scopes {
		scope := item.(*omap.Map)
		at, _ := omap.AsFloat(scope.Value("fetched_at"))
		if i == 0 || at < oldestScope {
			oldestScope = at
		}
	}
	stale := membersAt == 0 ||
		(stamp-membersAt) > interval*membersTTLSlack ||
		(len(scopes) > 0 && (stamp-oldestScope) > interval*membersTTLSlack)

	truncatedScopes := 0
	erroredScopes := 0
	for _, item := range scopes {
		scope := item.(*omap.Map)
		if scope.Bool("truncated", false) {
			truncatedScopes++
		}
		if scope.Str("error") != "" {
			erroredScopes++
		}
	}

	out := omap.New()
	if cfg != nil {
		out.Set("token_configured", cfg.GHAdminToken() != "")
		// A mismatch means the token was replaced since the last refresh, so nothing cached is
		// being trusted -- worth saying out loud rather than showing a healthy-looking age.
		stored := structure.Str("token_fp")
		if stored == "" {
			stored = members.Str("token_fp")
		}
		out.Set("token_matches", (structureAt != 0 || membersAt != 0) && TokenFP(cfg.GHAdminToken()) == stored)
	} else {
		out.Set("token_configured", nil)
		out.Set("token_matches", nil)
	}
	out.Set("refresh_seconds", fnum(interval))
	out.Set("structure", mapOf(
		"fetched_at", fnum(structureAt),
		"age_seconds", ageOrNil(stamp, structureAt),
		"enterprises", ents,
		"error", structure.Str("error"),
	))
	out.Set("members", mapOf(
		"fetched_at", fnum(membersAt),
		"age_seconds", ageOrNil(stamp, membersAt),
		"scopes", scopes,
		"truncated_scopes", num(truncatedScopes),
		"errored_scopes", num(erroredScopes),
	))
	probeCount := 0
	if entries := probes.Map("entries"); entries != nil {
		probeCount = entries.Len()
	}
	out.Set("probes", mapOf("count", num(probeCount)))
	out.Set("stale", stale)
	return out
}

func ageOrNil(stamp, at float64) any {
	if at == 0 {
		return nil
	}
	return fnum(stamp - at)
}

func numberOrZero(v any) any {
	if v == nil {
		return num(0)
	}
	return v
}

// -- Refresh ------------------------------------------------------------------

type scopeTarget struct {
	key    string
	kind   string
	org    string
	slug   string
	teamID string
}

// scopesToFetch is which member lists this policy actually needs.
//
// Only scopes the policy references are fetched. Caching member lists the policy never
// consults would be pure cost -- and under allow_all_orgs the enumerated orgs are what the key
// policy probes as a fallback, so those are worth having locally too.
func scopesToFetch(cfg *config.RouterConfig, structure *omap.Map) []scopeTarget {
	bySlug := map[string]*omap.Map{}
	for _, item := range structure.Slice("enterprises") {
		if enterprise, ok := item.(*omap.Map); ok && enterprise.Str("slug") != "" {
			bySlug[enterprise.Str("slug")] = enterprise
		}
	}
	out := []scopeTarget{}
	seen := map[string]bool{}
	add := func(target scopeTarget) {
		if !seen[target.key] {
			seen[target.key] = true
			out = append(out, target)
		}
	}

	enterprises := cfg.KeyPolicy.Map("enterprises")
	if enterprises == nil {
		return out
	}
	for _, slug := range enterprises.Keys() {
		rule := enterprises.Map(slug)
		if rule == nil || !rule.Bool("enabled", false) {
			continue
		}
		for _, org := range omap.StringSlice(rule.Value("organizations")) {
			if org = strings.TrimSpace(org); org != "" {
				add(scopeTarget{key: orgKey(org), kind: "org", org: org})
			}
		}
		for _, team := range omap.StringSlice(rule.Value("teams")) {
			if team = strings.TrimSpace(team); team != "" {
				add(scopeTarget{key: teamKey(slug, team), kind: "team", slug: slug, teamID: team})
			}
		}
		if rule.Bool("allow_all_orgs", false) {
			if enterprise := bySlug[slug]; enterprise != nil {
				for _, item := range enterprise.Slice("organizations") {
					org, ok := item.(*omap.Map)
					if !ok {
						continue
					}
					if login := strings.TrimSpace(org.Str("login")); login != "" {
						add(scopeTarget{key: orgKey(login), kind: "org", org: login})
					}
				}
			}
		}
	}
	if len(out) > maxMemberScopes {
		out = out[:maxMemberScopes]
	}
	return out
}

// CachedEnterpriseMembers is every login the member cache places in this enterprise:
// {login: scope key}.
//
// Read from the org and team lists already cached for the key policy, so it only knows about
// the scopes the policy names. Truncated or errored lists are still used here -- a login that
// *is* present is a positive fact even when the list is incomplete; only absence from such a
// list means nothing, and callers treat "not found" as unknown.
func CachedEnterpriseMembers(slug string) map[string]string {
	structure := loadStructure()
	var enterprise *omap.Map
	for _, item := range structure.Slice("enterprises") {
		if candidate, ok := item.(*omap.Map); ok && strings.EqualFold(candidate.Str("slug"), slug) {
			enterprise = candidate
			break
		}
	}
	out := map[string]string{}
	if enterprise == nil {
		return out
	}
	keys := []string{}
	for _, item := range enterprise.Slice("organizations") {
		if org, ok := item.(*omap.Map); ok {
			keys = append(keys, orgKey(org.Str("login")))
		}
	}
	for _, item := range enterprise.Slice("teams") {
		if team, ok := item.(*omap.Map); ok {
			keys = append(keys, teamKey(slug, omap.AsString(team.Value("id"))))
		}
	}
	entries := loadMembers().Map("entries")
	if entries == nil {
		return out
	}
	for _, key := range keys {
		entry := entries.Map(key)
		if entry == nil {
			continue
		}
		for _, item := range entry.Slice("logins") {
			login := strings.ToLower(omap.AsString(item))
			if _, exists := out[login]; !exists {
				out[login] = key
			}
		}
	}
	return out
}

// Refresh rebuilds structure.json and every member list the policy references.
//
// A per-scope failure is recorded in that scope's entry rather than aborting the refresh: one
// unreadable org must not cost the cache every other one.
func Refresh(ctx context.Context, cfg *config.RouterConfig) *omap.Map {
	token := cfg.GHAdminToken()
	if token == "" {
		status := Status(cfg)
		status.Set("error", "no GitHub token is configured")
		return status
	}

	fp := TokenFP(token)
	stamp := now()
	structureError := ""
	var enterprises []any
	discovered, err := ghadmin.Discover(ctx, token)
	if err != nil {
		// Keep whatever structure we already have: a transient GitHub failure should not empty
		// the configuration page.
		structureError = err.Error()
		enterprises = loadStructure().Slice("enterprises")
	} else {
		enterprises = discovered.Slice("enterprises")
	}

	fetchedAt := fnum(stamp)
	if structureError != "" {
		fetchedAt = fnum(loadStructure().Float("fetched_at", 0))
	}
	structureDoc := mapOf(
		"fetched_at", fetchedAt,
		"token_fp", fp,
		"enterprises", enterprises,
		"error", structureError,
	)
	_ = jsonfile.Write(StructurePath, structureDoc)

	targets := scopesToFetch(cfg, structureDoc)
	results := make([]ghadmin.MemberList, len(targets))
	semaphore := make(chan struct{}, fetchConcurrency)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target scopeTarget) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			if target.kind == "org" {
				results[i] = ghadmin.ListOrgMembers(ctx, token, target.org)
			} else {
				results[i] = ghadmin.ListEnterpriseTeamMembers(ctx, token, target.slug, target.teamID)
			}
		}(i, target)
	}
	wg.Wait()

	entries := omap.New()
	complete := map[string]bool{}
	truncatedCount, erroredCount := 0, 0
	for i, target := range targets {
		result := results[i]
		logins := make([]any, len(result.Logins))
		for j, login := range result.Logins {
			logins[j] = login
		}
		entries.Set(target.key, mapOf(
			"kind", target.kind,
			"logins", logins,
			"truncated", result.Truncated,
			"error", result.Error,
			"fetched_at", fnum(now()),
		))
		if result.Truncated {
			truncatedCount++
		}
		if result.Error != "" {
			erroredCount++
		}
		if !result.Truncated && result.Error == "" {
			complete[target.key] = true
		}
	}
	_ = jsonfile.Write(MembersPath, mapOf(
		"fetched_at", fnum(now()),
		"token_fp", fp,
		"entries", entries,
	))

	// Individual probes are now redundant for every scope that got a complete list, and
	// keeping them would let a stale negative outlive the list that contradicts it.
	dropProbesFor(complete)

	log.Printf("INFO mr: GitHub cache refreshed enterprises=%d scopes=%d truncated=%d errored=%d",
		len(enterprises), entries.Len(), truncatedCount, erroredCount)
	return Status(cfg)
}

func dropProbesFor(keys map[string]bool) {
	if len(keys) == 0 {
		return
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	data := jsonfile.Read(ProbePath)
	entries := data.Map("entries")
	if entries == nil {
		return
	}
	removed := false
	for _, key := range entries.Keys() {
		scope := key
		if idx := strings.LastIndex(key, ":"); idx >= 0 {
			scope = key[:idx]
		}
		if keys[scope] {
			entries.Delete(key)
			removed = true
		}
	}
	if removed {
		_ = jsonfile.Write(ProbePath, data)
		probeCache = data
		probeMTime = jsonfile.MTime(ProbePath)
	}
}

// -- The refresh lease --------------------------------------------------------

// AcquireLease is a best-effort "I am the worker that refreshes" lease.
//
// data/ is explicitly shared between workers, so without this every worker would refresh on
// its own timer. It is deliberately not a real lock: writes are atomic, so a duplicate refresh
// is merely wasteful rather than corrupting, and the cost of getting distributed locking wrong
// is far higher than the cost of an occasional extra refresh.
func AcquireLease() bool {
	pid := os.Getpid()
	lease := jsonfile.Read(LockPath)
	if lease.Float("expires_at", 0) > now() && lease.Int("owner_pid", 0) != pid {
		return false
	}
	_ = jsonfile.Write(LockPath, mapOf("owner_pid", num(pid), "expires_at", fnum(now()+leaseTTL)))
	// Re-read: if two workers wrote at once, only the one whose value survived the last atomic
	// replace continues.
	return jsonfile.Read(LockPath).Int("owner_pid", 0) == pid
}

func ReleaseLease() {
	if jsonfile.Read(LockPath).Int("owner_pid", 0) == os.Getpid() {
		_ = jsonfile.Write(LockPath, mapOf("owner_pid", nil, "expires_at", num(0)))
	}
}
