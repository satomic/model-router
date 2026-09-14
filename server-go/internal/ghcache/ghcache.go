// Package ghcache is the on-disk cache of the GitHub enterprise / organization / team
// structure and its member lists, plus cache-first membership answers.
//
// Why this exists: every access decision used to be a live GitHub round trip (the key policy
// probes /orgs/{org}/members/{login} per user) behind an in-process TTL cache -- which is keyed
// on a monotonic clock and therefore dies with the process. Restart the service and every
// first request pays GitHub's latency again, and a busy console burns rate limit on questions
// whose answer changes maybe once a week.
//
// So the structure and the member lists are persisted under data/github/ and refreshed on a
// timer, and membership becomes a set lookup:
//
//	data/github/structure.json  the discover() payload: enterprises, their orgs and teams
//	data/github/members.json    one entry per scope: {"org:acme": {logins, truncated, error}}
//	data/github/probe.json      individual live-probe results, positive and negative
//	data/github/refresh.lock    a best-effort lease, so N workers do not all refresh at once
//
// Two rules govern trust, and both matter more than the speed-up:
//
//   - A member list is authoritative only when it is fresh, complete and error-free. A
//     truncated or errored list is never authoritative -- "not in the first 5000 logins I could
//     read" is not "not a member", and reading it as one would deny legitimate users. Those
//     cases fall through to a live probe.
//   - Timestamps are wall-clock, never monotonic. Monotonic values are meaningless once written
//     to disk and read back after a restart.
//
// The token is never stored. TokenFP is sha256(token)[:12], which is enough to notice that the
// token changed (and therefore that the cached answers may reflect a different visibility)
// without keeping the secret in a second place.
package ghcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghadmin"
	"github.com/satomic/model-router/server-go/internal/jsonfile"
	"github.com/satomic/model-router/server-go/internal/omap"
)

var (
	CacheDir      string
	StructurePath string
	MembersPath   string
	ProbePath     string
	LockPath      string
)

func Init(dataDir string) {
	CacheDir = filepath.Join(dataDir, "github")
	StructurePath = filepath.Join(CacheDir, "structure.json")
	MembersPath = filepath.Join(CacheDir, "members.json")
	ProbePath = filepath.Join(CacheDir, "probe.json")
	LockPath = filepath.Join(CacheDir, "refresh.lock")
}

// DefaultRefreshSeconds is how long a refreshed member list stays authoritative. Beyond this
// the entry is still shown in the admin UI (with its age) but no longer answers membership on
// its own.
const DefaultRefreshSeconds = 3600.0

// A list counts as fresh for slack x the refresh interval, so a single skipped refresh does
// not fall back to live probing.
const membersTTLSlack = 2.0

// Live-probe results are cached with asymmetric TTLs. Positives last longer because losing
// access is rarer than gaining it; negatives expire quickly so a user who was just added to an
// org gets in without waiting for the next full refresh.
const (
	probeTTL = 600.0
	negTTL   = 120.0
)

// Upper bound on how many member lists one refresh will fetch. Under allow_all_orgs an
// enterprise can reference thousands of orgs; fetching every member list would be a far bigger
// GitHub bill than the per-user probing this replaces.
const maxMemberScopes = 60

// Concurrency for those fetches: enough to keep the refresh short, low enough to stay clear of
// GitHub's secondary rate limit.
const fetchConcurrency = 4

// Lease lifetime. Long enough for a slow refresh to finish, short enough that a worker killed
// mid-refresh does not block the next one for long.
const leaseTTL = 600.0

// Sources reported alongside every answer, so an admin can see which layer decided.
const (
	SourceCache = "cache" // answered from a complete member list: zero GitHub calls
	SourceProbe = "probe" // answered from a cached individual probe: zero GitHub calls
	SourceLive  = "live"  // a real GitHub call was made
)

// Guards the read-modify-write of probe.json inside one process. Cross-process safety comes
// from the atomic replace in jsonfile.Write plus the mtime re-read below: a lost probe entry
// only costs one extra GitHub call, so a real lock is not warranted.
var (
	probeMu    sync.Mutex
	probeCache *omap.Map
	probeMTime float64
)

// TokenFP is a short fingerprint of the admin token. Never the token itself.
func TokenFP(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func fnum(v float64) json.Number { return omap.Num(v) }
func num(v int) json.Number      { return omap.Int(v) }

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

func orgKey(org string) string { return "org:" + strings.ToLower(strings.TrimSpace(org)) }

func teamKey(slug, teamID string) string {
	return "team:" + strings.ToLower(strings.TrimSpace(slug)) + "/" + teamID
}

// RefreshSeconds is the refresh interval, from auth.key_policy.cache_refresh_seconds.
//
// Clamped to a floor: a misconfigured 1 would turn the background loop into a GitHub hammer,
// which is the opposite of what this package is for.
func RefreshSeconds(cfg *config.RouterConfig) float64 {
	value := cfg.KeyPolicy.Float("cache_refresh_seconds", 0)
	if value <= 0 {
		return DefaultRefreshSeconds
	}
	if value < 60 {
		return 60
	}
	return value
}

// -- Raw file access ----------------------------------------------------------

func loadMembers() *omap.Map {
	data := jsonfile.Read(MembersPath)
	if !data.Has("entries") {
		data.Set("entries", omap.New())
	}
	return data
}

func loadStructure() *omap.Map {
	data := jsonfile.Read(StructurePath)
	if !data.Has("enterprises") {
		data.Set("enterprises", []any{})
	}
	return data
}

// loadProbes reads probe.json through a process-local cache, re-reading only when the file
// changed -- the same mtime-guarded pattern the auth store uses for sessions and keys.
func loadProbes() *omap.Map {
	current := jsonfile.MTime(ProbePath)
	if probeCache == nil || current != probeMTime {
		probeCache = jsonfile.Read(ProbePath)
		if !probeCache.Has("entries") {
			probeCache.Set("entries", omap.New())
		}
		probeMTime = current
	}
	return probeCache
}

func recordProbe(key string, member bool) {
	probeMu.Lock()
	defer probeMu.Unlock()
	data := jsonfile.Read(ProbePath)
	entries := data.Map("entries")
	if entries == nil {
		entries = omap.New()
		data.Set("entries", entries)
	}
	entries.Set(key, mapOf("member", member, "at", fnum(now())))
	// Bound the file: probe entries are one per (scope, login) pair and would otherwise grow
	// without limit on a deployment with many users.
	if entries.Len() > 5000 {
		keys := entries.Keys()
		sort.SliceStable(keys, func(i, j int) bool {
			return entries.Map(keys[i]).Float("at", 0) < entries.Map(keys[j]).Float("at", 0)
		})
		for _, stale := range keys[:1000] {
			entries.Delete(stale)
		}
	}
	_ = jsonfile.Write(ProbePath, data)
	probeCache = data
	probeMTime = jsonfile.MTime(ProbePath)
}

// Invalidate drops every cached answer. Called when the token or the policy changes: the same
// question can have a different answer under a different token, so keeping the old files would
// leave stale member lists authoritative.
func Invalidate() {
	for _, path := range []string{StructurePath, MembersPath, ProbePath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("WARNING mr: could not remove cache file %s: %v", path, err)
		}
	}
	probeMu.Lock()
	probeCache = nil
	probeMTime = 0
	probeMu.Unlock()
}

// -- Membership: cache first, live fallback ------------------------------------

// listAnswers answers from a member list, or returns nil when that list may not be trusted.
//
// Untrusted means: absent, fetched under a different token, stale, truncated, or errored. Each
// of those is a case where "login not in logins" would be a guess, and a wrong guess here
// denies a legitimate user their API key.
func listAnswers(cfg *config.RouterConfig, key, login string) *bool {
	data := loadMembers()
	if data.Str("token_fp") != TokenFP(cfg.GHAdminToken()) {
		return nil
	}
	entries := data.Map("entries")
	if entries == nil {
		return nil
	}
	entry := entries.Map(key)
	if entry == nil {
		return nil
	}
	if entry.Bool("truncated", false) || entry.Str("error") != "" {
		return nil
	}
	if now()-entry.Float("fetched_at", 0) > RefreshSeconds(cfg)*membersTTLSlack {
		return nil
	}
	logins, ok := entry.Value("logins").([]any)
	if !ok {
		return nil
	}
	wanted := strings.ToLower(strings.TrimSpace(login))
	found := false
	for _, item := range logins {
		if omap.AsString(item) == wanted {
			found = true
			break
		}
	}
	return &found
}

// probeAnswers answers from a cached individual probe, honouring the polarity-dependent TTL.
func probeAnswers(key, login string) *bool {
	entries := loadProbes().Map("entries")
	if entries == nil {
		return nil
	}
	entry := entries.Map(key + ":" + strings.ToLower(login))
	if entry == nil {
		return nil
	}
	member := entry.Bool("member", false)
	age := now() - entry.Float("at", 0)
	limit := negTTL
	if member {
		limit = probeTTL
	}
	if age > limit {
		return nil
	}
	return &member
}

// IsOrgMember returns (is a member, source). Cache first; a miss costs exactly one GitHub call.
func IsOrgMember(ctx context.Context, cfg *config.RouterConfig, org, login string) (bool, string) {
	token := cfg.GHAdminToken()
	key := orgKey(org)
	if answer := listAnswers(cfg, key, login); answer != nil {
		return *answer, SourceCache
	}
	if answer := probeAnswers(key, login); answer != nil {
		return *answer, SourceProbe
	}
	if token == "" {
		// The key policy refuses before reaching here, but a tokenless call must still be
		// fail-closed rather than raising into an authorization decision.
		return false, SourceLive
	}
	result := ghadmin.CheckOrgMember(ctx, token, org, login)
	recordProbe(key+":"+strings.ToLower(login), result)
	return result, SourceLive
}

// IsTeamMember returns (is a member, source) for an enterprise team. Same rules as IsOrgMember.
func IsTeamMember(ctx context.Context, cfg *config.RouterConfig, slug, teamID, login string) (bool, string) {
	token := cfg.GHAdminToken()
	key := teamKey(slug, teamID)
	if answer := listAnswers(cfg, key, login); answer != nil {
		return *answer, SourceCache
	}
	if answer := probeAnswers(key, login); answer != nil {
		return *answer, SourceProbe
	}
	if token == "" {
		return false, SourceLive
	}
	result := ghadmin.CheckEnterpriseTeamMember(ctx, token, slug, teamID, login)
	recordProbe(key+":"+strings.ToLower(login), result)
	return result, SourceLive
}

// ScopeMembersPage serves one page of a scope's member roster, from the cache when it is
// complete and fresh, else live from GitHub.
func ScopeMembersPage(ctx context.Context, cfg *config.RouterConfig, kind, name string, page int) (*omap.Map, error) {
	const size = 50
	if cfg.GHAdminToken() == "" {
		return nil, fmt.Errorf("no GitHub Enterprise token configured")
	}
	key := orgKey(name)
	if kind != "organization" {
		key = "team:" + strings.ToLower(name)
	}
	data := loadMembers()
	entries := data.Map("entries")
	if entries == nil {
		entries = omap.New()
	}
	entry := entries.Map(key)
	if entry == nil {
		entry = omap.New()
	}
	fresh := now()-entry.Float("fetched_at", 0) <= RefreshSeconds(cfg)*membersTTLSlack
	logins, hasLogins := entry.Value("logins").([]any)
	if data.Str("token_fp") == TokenFP(cfg.GHAdminToken()) && fresh &&
		entry.Str("error") == "" && !entry.Bool("truncated", false) && hasLogins {
		unique := map[string]bool{}
		for _, item := range logins {
			unique[omap.AsString(item)] = true
		}
		sorted := make([]string, 0, len(unique))
		for login := range unique {
			sorted = append(sorted, login)
		}
		sort.Strings(sorted)
		offset := (page - 1) * size
		users := []any{}
		for i := offset; i < offset+size && i < len(sorted); i++ {
			users = append(users, mapOf("login", sorted[i], "name", sorted[i], "kind", "github"))
		}
		return mapOf(
			"users", users,
			"page", num(page),
			"has_more", offset+size < len(sorted),
			"source", SourceCache,
		), nil
	}

	var path string
	if kind == "organization" {
		path = "/orgs/" + url.PathEscape(name) + "/members"
	} else {
		slug, teamID, _ := strings.Cut(name, "/")
		path = fmt.Sprintf("/enterprises/%s/teams/%s/memberships", url.PathEscape(slug), url.PathEscape(teamID))
	}
	status, body, err := ghadmin.Rest(ctx, cfg.GHAdminToken(),
		fmt.Sprintf("%s?per_page=%d&page=%d", path, size, page), false)
	if err != nil {
		return nil, err
	}
	items, ok := body.([]any)
	if status != 200 || !ok {
		return nil, fmt.Errorf("GitHub member list unavailable")
	}
	users := []any{}
	for _, item := range items {
		record, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		account := record
		if nested := record.Map("user"); nested != nil {
			account = nested
		}
		login := account.Str("login")
		if login == "" {
			continue
		}
		displayName := account.Str("name")
		if displayName == "" {
			displayName = login
		}
		users = append(users, mapOf("login", strings.ToLower(login), "name", displayName, "kind", "github"))
	}
	return mapOf(
		"users", users,
		"page", num(page),
		"has_more", len(items) == size,
		"source", SourceLive,
	), nil
}
