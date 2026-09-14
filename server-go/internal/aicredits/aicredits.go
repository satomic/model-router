// Package aicredits monitors the GitHub Copilot AI-credit pool, and holds the gate that keeps
// BYOK traffic off third-party models while the pool still has credits in it.
//
// Why this exists: every Copilot Business / Enterprise seat comes with a monthly allowance of
// AI credits that is **pooled across the enterprise** and **forfeited at month end**. A user
// who routes everything through this service on BYOK spends the customer's Azure/OpenAI bill
// while the credits they already paid for expire unused. So this package polls GitHub on a cron
// schedule, works out whether the pool is exhausted, and -- when the operator turns the gate on
// -- answers a chat request with a short note instead of routing it, for as long as the pool
// has credits left.
//
// What GitHub actually offers (the API documents none of the pool arithmetic, so the derivation
// is written down here):
//
//	GET /enterprises/{slug}/settings/billing/usage/summary?product=copilot
//	    Per-SKU month-to-date totals. The SKUs with unitType "ai-units" are the pool:
//	    discountQuantity is what the pool covered, netQuantity is metered overage.
//	    discountQuantity is capped at the pool size, so **netQuantity > 0 means the pool is
//	    exhausted and sum(discountQuantity) is its exact size**. While netQuantity == 0 the size
//	    has to be estimated from seats.
//	GET /enterprises/{slug}/copilot/billing/seats
//	    One entry per licensed user with plan_type (enterprise / business). Sum of the per-plan
//	    included amounts = pool size. **404s on very large enterprises**, in which case the size
//	    is unknown until the pool is exhausted -- but exhaustion itself is still detectable.
//	GET /enterprises/{slug}/settings/billing/budgets  (+ /{id}/user-states)
//	    User-level budgets (sku ai_credits / premium_requests) are the only per-user limit.
package aicredits

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/cronexpr"
	"github.com/satomic/model-router/server-go/internal/ghadmin"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/jsonfile"
	"github.com/satomic/model-router/server-go/internal/omap"
)

var (
	SnapshotPath string
	LockPath     string
)

func Init(dataDir string) {
	SnapshotPath = filepath.Join(dataDir, "ai_credits.json")
	LockPath = filepath.Join(dataDir, "ai_credits.lock")
}

// IncludedCredits is the AI credits per seat per month, from GitHub's published plan table. If
// GitHub changes these the seat-based estimate drifts, but the exact figure takes over the
// moment the pool is exhausted, so the gate itself stays correct.
var IncludedCredits = map[string]float64{"enterprise": 3900, "business": 1900}

const USDPerCredit = 0.01

const (
	accept     = "application/vnd.github+json"
	apiVersion = "2022-11-28"
	timeout    = 30 * time.Second
	pageSize   = 100
	// Ceilings on paginated walks. 50 pages of seats is 5000 users; an enterprise beyond that
	// has most likely 404'd the seats endpoint anyway.
	maxPages = 50
	// The lease keeps N workers from polling GitHub at once; long enough for a slow enterprise.
	leaseTTL = 600.0
	// A snapshot older than this many schedule intervals (and never less than 15 minutes) no
	// longer blocks anyone: a gate that fails closed on stale data would lock users out through
	// a GitHub outage, which is worse than a few requests slipping through to BYOK.
	staleIntervals = 2.0
	staleFloor     = 900.0
)

const (
	PoolAvailable = "available"
	PoolExhausted = "exhausted"
	PoolUnknown   = "unknown"
	PoolNone      = "none" // no Copilot seats at all
)

const GateReason = "ai-credits-gate"

// Why a request was gated: the enterprise pool still has credits, or (per-user mode) the
// caller's own user-level budget does. Each has its own note.
const (
	ReasonPool   = "pool"
	ReasonBudget = "budget"
)

const DefaultMessage = "Your GitHub Copilot AI-credit pool ({enterprise}) still has credits left" +
	"{remaining_note}. Please use Copilot's built-in models first; this BYOK " +
	"route reopens automatically once the pool is used up."

const DefaultMessageBudget = "Your GitHub Copilot user-level budget in {enterprise} still has " +
	"${budget_remaining_usd} of ${budget_total_usd} left. Please use Copilot's built-in " +
	"models first; this BYOK route reopens automatically once your budget is used up."

// Error reports a GitHub call that failed in a way the administrator needs to see.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func errf(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

func fnum(v float64) json.Number { return omap.Num(v) }
func num(v int) json.Number      { return omap.Int(v) }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// -- Settings -----------------------------------------------------------------

// Settings is the `ai_credits` section, normalised so every reader sees the same defaults.
type Settings struct {
	Enabled             bool
	Schedule            string
	Enterprises         []string
	MinRemainingCredits float64
	GateEnabled         bool
	PerUser             bool
	Message             string
	MessageBudget       string
}

func Config(cfg *config.RouterConfig) Settings {
	raw := cfg.AICredits
	gate := raw.Map("gate")
	if gate == nil {
		gate = omap.New()
	}
	floor := raw.Float("min_remaining_credits", 0)
	if floor < 0 {
		floor = 0
	}
	schedule := strings.TrimSpace(raw.Str("schedule"))
	if schedule == "" {
		schedule = cronexpr.DefaultSchedule
	}
	enterprises := []string{}
	for _, slug := range omap.StringSlice(raw.Value("enterprises")) {
		if trimmed := strings.TrimSpace(slug); trimmed != "" {
			enterprises = append(enterprises, strings.ToLower(trimmed))
		}
	}
	return Settings{
		Enabled:             raw.Bool("enabled", false),
		Schedule:            schedule,
		Enterprises:         enterprises,
		MinRemainingCredits: floor,
		GateEnabled:         gate.Bool("enabled", false),
		PerUser:             gate.Bool("per_user", false),
		Message:             strings.TrimSpace(gate.Str("message")),
		MessageBudget:       strings.TrimSpace(gate.Str("message_budget")),
	}
}

func (s Settings) toMap() *omap.Map {
	enterprises := make([]any, len(s.Enterprises))
	for i, slug := range s.Enterprises {
		enterprises[i] = slug
	}
	return mapOf(
		"enabled", s.Enabled,
		"schedule", s.Schedule,
		"enterprises", enterprises,
		"min_remaining_credits", fnum(s.MinRemainingCredits),
		"gate_enabled", s.GateEnabled,
		"per_user", s.PerUser,
		"message", s.Message,
		"message_budget", s.MessageBudget,
	)
}

// -- GitHub calls -------------------------------------------------------------

var client = &http.Client{Timeout: timeout}

// get returns (status, body). 401/403 fail -- they mean the token, not the enterprise.
func get(ctx context.Context, token, path string, params url.Values) (int, *omap.Map, error) {
	endpoint := ghadmin.API + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, errf("GitHub request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == 401:
		return resp.StatusCode, nil, errf("token is invalid or expired (GitHub 401)")
	case resp.StatusCode == 403:
		return resp.StatusCode, nil, errf("token lacks billing permission or hit a rate limit (GitHub 403): %s", path)
	case resp.StatusCode >= 500:
		return resp.StatusCode, nil, errf("GitHub %d: %s", resp.StatusCode, path)
	}
	value, err := omap.FromJSON(body)
	if err != nil {
		return resp.StatusCode, nil, nil
	}
	data, ok := value.(*omap.Map)
	if !ok {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, data, nil
}

func aiUnitItems(summary *omap.Map) []*omap.Map {
	out := []*omap.Map{}
	for _, item := range summary.Slice("usageItems") {
		row, ok := item.(*omap.Map)
		if ok && strings.EqualFold(row.Str("unitType"), "ai-units") {
			out = append(out, row)
		}
	}
	return out
}

// fetchSeats returns (plan counts, {login: plan}, error). counts is nil when the endpoint is
// unavailable.
func fetchSeats(ctx context.Context, token, slug string) (map[string]int, map[string]string, string) {
	counts := map[string]int{}
	logins := map[string]string{}
	total := -1
	for page := 1; page <= maxPages; page++ {
		params := url.Values{"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
		status, body, err := get(ctx, token, "/enterprises/"+url.PathEscape(slug)+"/copilot/billing/seats", params)
		if err != nil {
			return nil, nil, err.Error()
		}
		if status == 404 {
			return nil, nil, ""
		}
		if status != 200 || body == nil {
			return nil, nil, fmt.Sprintf("seats endpoint returned %d", status)
		}
		total = body.Int("total_seats", 0)
		seats := body.Slice("seats")
		for _, item := range seats {
			seat, ok := item.(*omap.Map)
			if !ok {
				continue
			}
			plan := strings.ToLower(seat.Str("plan_type"))
			counts[plan]++
			if assignee := seat.Map("assignee"); assignee != nil {
				if login := strings.ToLower(assignee.Str("login")); login != "" {
					logins[login] = plan
				}
			}
		}
		if len(seats) == 0 || len(logins) >= total {
			break
		}
	}
	if total >= 0 && len(logins) < total {
		return counts, logins, fmt.Sprintf("seat list truncated at %d of %d", len(logins), total)
	}
	return counts, logins, ""
}

type budgetState struct {
	TargetUSD   float64
	ConsumedUSD float64
}

// fetchBudgets reads per-user budget state: ({login: state}, universal budget, error).
//
// Only AI-credit budgets count; an Actions budget says nothing about Copilot. The user-states
// of a multi-user budget already reflect any individual override (GitHub reports the effective
// target), and an individual budget is then applied on top so a user who has not consumed
// anything this cycle -- and so is absent from user-states -- still gets their own limit rather
// than the universal one.
func fetchBudgets(ctx context.Context, token, slug string) (map[string]budgetState, *float64, string) {
	users := map[string]budgetState{}
	var universal *float64
	multi := []string{}
	budgets := []*omap.Map{}

	for page := 1; page <= maxPages; page++ {
		params := url.Values{"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
		status, body, err := get(ctx, token, "/enterprises/"+url.PathEscape(slug)+"/settings/billing/budgets", params)
		if err != nil {
			return nil, nil, err.Error()
		}
		if status == 404 {
			return map[string]budgetState{}, nil, ""
		}
		if status != 200 || body == nil {
			return nil, nil, fmt.Sprintf("budgets endpoint returned %d", status)
		}
		for _, item := range body.Slice("budgets") {
			if budget, ok := item.(*omap.Map); ok {
				budgets = append(budgets, budget)
			}
		}
		if !body.Bool("has_next_page", false) {
			break
		}
	}

	isCreditSKU := func(budget *omap.Map) bool {
		sku := strings.ToLower(budget.Str("budget_product_sku"))
		return sku == "ai_credits" || sku == "premium_requests"
	}
	for _, budget := range budgets {
		if !isCreditSKU(budget) {
			continue
		}
		switch budget.Str("budget_scope") {
		case "multi_user_customer":
			amount := budget.Float("budget_amount", 0)
			universal = &amount
			multi = append(multi, omap.AsString(budget.Value("id")))
		case "multi_user_cost_center":
			multi = append(multi, omap.AsString(budget.Value("id")))
		}
	}
	for _, budgetID := range multi {
		for page := 1; page <= maxPages; page++ {
			params := url.Values{"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
			status, body, err := get(ctx, token,
				fmt.Sprintf("/enterprises/%s/settings/billing/budgets/%s/user-states",
					url.PathEscape(slug), url.PathEscape(budgetID)), params)
			if err != nil || status != 200 || body == nil {
				break
			}
			for _, item := range body.Slice("user_states") {
				state, ok := item.(*omap.Map)
				if !ok {
					continue
				}
				login := strings.ToLower(state.Str("user"))
				if login == "" {
					continue
				}
				users[login] = budgetState{
					TargetUSD:   state.Float("target_amount", 0),
					ConsumedUSD: state.Float("consumed_amount", 0),
				}
			}
			if !body.Bool("has_next_page", false) {
				break
			}
		}
	}
	for _, budget := range budgets {
		if budget.Str("budget_scope") != "user" || !isCreditSKU(budget) {
			continue
		}
		login := strings.ToLower(budget.Str("user"))
		if login == "" {
			login = strings.ToLower(budget.Str("budget_entity_name"))
		}
		if login == "" {
			continue
		}
		users[login] = budgetState{
			TargetUSD:   budget.Float("budget_amount", 0),
			ConsumedUSD: budget.Float("consumed_amount", 0),
		}
	}
	return users, universal, ""
}

// FetchEnterprise reads one enterprise's pool state. Never fails for a per-enterprise problem:
// the error is recorded on the entry so the other enterprises still get their data.
func FetchEnterprise(ctx context.Context, token, slug, name string, perUser bool) *omap.Map {
	if name == "" {
		name = slug
	}
	entry := mapOf(
		"slug", slug, "name", name,
		"pool_total", nil, "pool_total_source", nil,
		"consumed", fnum(0), "metered", fnum(0), "gross", fnum(0),
		"remaining", nil, "state", PoolUnknown,
		"seats", nil, "seat_logins", nil, "member_logins", omap.New(),
		"users", nil, "universal_budget_usd", nil,
		"skus", []any{}, "warnings", []any{}, "error", nil,
	)
	appendWarning := func(text string) {
		entry.Set("warnings", append(entry.Slice("warnings"), text))
	}

	status, summary, err := get(ctx, token,
		"/enterprises/"+url.PathEscape(slug)+"/settings/billing/usage/summary",
		url.Values{"product": {"copilot"}})
	if err != nil {
		entry.Set("error", err.Error())
		return entry
	}
	if status == 404 {
		entry.Set("error", "billing usage endpoint not available for this enterprise (GitHub 404)")
		return entry
	}
	if status != 200 || summary == nil {
		entry.Set("error", fmt.Sprintf("billing usage endpoint returned %d", status))
		return entry
	}

	skus := []any{}
	consumed, metered, gross := 0.0, 0.0, 0.0
	for _, item := range aiUnitItems(summary) {
		covered := item.Float("discountQuantity", 0)
		net := item.Float("netQuantity", 0)
		total := item.Float("grossQuantity", 0)
		consumed += covered
		metered += net
		gross += total
		skus = append(skus, mapOf(
			"sku", item.Value("sku"),
			"gross", fnum(total), "covered", fnum(covered), "metered", fnum(net),
		))
	}
	entry.Set("skus", skus)
	entry.Set("consumed", fnum(consumed))
	entry.Set("metered", fnum(metered))
	entry.Set("gross", fnum(gross))

	counts, logins, seatErr := fetchSeats(ctx, token, slug)
	if seatErr != "" {
		appendWarning(seatErr)
	}
	if counts != nil {
		seatCounts := omap.New()
		planNames := make([]string, 0, len(counts))
		for plan := range counts {
			planNames = append(planNames, plan)
		}
		sort.Strings(planNames)
		for _, plan := range planNames {
			seatCounts.Set(plan, num(counts[plan]))
		}
		entry.Set("seats", seatCounts)
		seatLogins := omap.New()
		loginNames := make([]string, 0, len(logins))
		for login := range logins {
			loginNames = append(loginNames, login)
		}
		sort.Strings(loginNames)
		for _, login := range loginNames {
			seatLogins.Set(login, logins[login])
		}
		entry.Set("seat_logins", seatLogins)
	} else {
		appendWarning("seat list unavailable (GitHub 404 -- typical of very large enterprises); " +
			"pool size is only known once the pool is exhausted")
	}

	if perUser {
		users, universal, budgetErr := fetchBudgets(ctx, token, slug)
		if budgetErr != "" {
			appendWarning(budgetErr)
		}
		userMap := omap.New()
		loginNames := make([]string, 0, len(users))
		for login := range users {
			loginNames = append(loginNames, login)
		}
		sort.Strings(loginNames)
		for _, login := range loginNames {
			state := users[login]
			userMap.Set(login, mapOf(
				"target_usd", fnum(state.TargetUSD),
				"consumed_usd", fnum(state.ConsumedUSD),
			))
		}
		entry.Set("users", userMap)
		if universal != nil {
			entry.Set("universal_budget_usd", fnum(*universal))
		}
	}

	// Who belongs to this enterprise, for the gate's membership test. The seat list is the
	// authoritative answer when GitHub gives one; when it does not (large enterprises) the key
	// policy's cached org / team member lists are the fallback, so a user the access policy
	// already places in this enterprise is still recognised.
	if entry.Value("seat_logins") == nil {
		members := omap.New()
		cached := ghcache.CachedEnterpriseMembers(slug)
		loginNames := make([]string, 0, len(cached))
		for login := range cached {
			loginNames = append(loginNames, login)
		}
		sort.Strings(loginNames)
		for _, login := range loginNames {
			members.Set(login, cached[login])
		}
		entry.Set("member_logins", members)
	}

	derivePool(entry)
	return entry
}

// derivePool fills pool_total / remaining / state from what was fetched. See the package
// docstring for why exhaustion is read off netQuantity rather than off the seat estimate.
func derivePool(entry *omap.Map) {
	consumed := entry.Float("consumed", 0)
	if entry.Float("metered", 0) > 0 {
		entry.Set("pool_total", fnum(consumed))
		entry.Set("pool_total_source", "exact")
		entry.Set("remaining", fnum(0))
		entry.Set("state", PoolExhausted)
		return
	}
	seats := entry.Map("seats")
	if seats != nil {
		total := 0.0
		unknownPlans := []string{}
		for _, plan := range seats.Keys() {
			included, known := IncludedCredits[plan]
			if !known {
				unknownPlans = append(unknownPlans, plan)
				continue
			}
			total += included * float64(seats.Int(plan, 0))
		}
		if len(unknownPlans) > 0 {
			entry.Set("warnings", append(entry.Slice("warnings"),
				fmt.Sprintf("seat plan(s) %s have no known included amount", strings.Join(unknownPlans, ", "))))
		}
		remaining := total - consumed
		if remaining < 0 {
			remaining = 0
		}
		entry.Set("pool_total", fnum(total))
		entry.Set("pool_total_source", "seats")
		entry.Set("remaining", fnum(remaining))
		switch {
		case total <= 0:
			entry.Set("state", PoolNone)
		case remaining > 0:
			entry.Set("state", PoolAvailable)
		default:
			entry.Set("state", PoolExhausted)
		}
		return
	}
	// No seat list and nothing metered: the pool exists if anything has been drawn from it.
	if consumed > 0 {
		entry.Set("state", PoolAvailable)
	} else {
		entry.Set("state", PoolUnknown)
	}
}

// Refresh polls every selected enterprise and persists the snapshot. It fails only when nothing
// at all could be done (no token, enterprise listing failed).
func Refresh(ctx context.Context, cfg *config.RouterConfig) (*omap.Map, error) {
	token := cfg.GHAdminToken()
	if token == "" {
		return nil, errf("no GitHub enterprise administrator token is configured")
	}
	conf := Config(cfg)
	ents, err := ghadmin.ListEnterprises(ctx, token)
	if err != nil {
		return nil, errf("%s", err.Error())
	}
	wanted := map[string]bool{}
	for _, slug := range conf.Enterprises {
		wanted[slug] = true
	}
	selected := []ghadmin.Enterprise{}
	present := map[string]bool{}
	for _, enterprise := range ents {
		present[strings.ToLower(enterprise.Slug)] = true
		if len(wanted) == 0 || wanted[strings.ToLower(enterprise.Slug)] {
			selected = append(selected, enterprise)
		}
	}
	missing := []any{}
	missingNames := []string{}
	for slug := range wanted {
		if !present[slug] {
			missingNames = append(missingNames, slug)
		}
	}
	sort.Strings(missingNames)
	for _, slug := range missingNames {
		missing = append(missing, slug)
	}

	results := make([]*omap.Map, len(selected))
	var wg sync.WaitGroup
	for i, enterprise := range selected {
		wg.Add(1)
		go func(i int, enterprise ghadmin.Enterprise) {
			defer wg.Done()
			results[i] = FetchEnterprise(ctx, token, enterprise.Slug, enterprise.Name, conf.PerUser)
		}(i, enterprise)
	}
	wg.Wait()

	stamp := now()
	enterpriseMap := omap.New()
	states := []string{}
	for _, result := range results {
		enterpriseMap.Set(result.Str("slug"), result)
		states = append(states, result.Str("slug")+"="+result.Str("state"))
	}
	snapshot := mapOf(
		"fetched_at", fnum(stamp),
		"fetched_at_iso", time.Unix(0, int64(stamp*1e9)).UTC().Format(time.RFC3339Nano),
		"token_fp", ghcache.TokenFP(token),
		"schedule", conf.Schedule,
		"per_user", conf.PerUser,
		"enterprises", enterpriseMap,
		"missing_enterprises", missing,
	)
	_ = jsonfile.Write(SnapshotPath, snapshot)
	reloadSnapshot()
	summary := strings.Join(states, ", ")
	if summary == "" {
		summary = "no enterprises"
	}
	log.Printf("INFO mr: AI credits refreshed: %s", summary)
	return snapshot, nil
}

// -- Snapshot access ------------------------------------------------------------

var (
	snapshotMu    sync.Mutex
	snapshotCache *omap.Map
	snapshotMTime float64
)

func reloadSnapshot() *omap.Map {
	snapshotMu.Lock()
	defer snapshotMu.Unlock()
	current := jsonfile.MTime(SnapshotPath)
	if snapshotCache == nil || current != snapshotMTime {
		data := jsonfile.Read(SnapshotPath)
		if data.Float("fetched_at", 0) != 0 {
			snapshotCache = data
		} else {
			snapshotCache = omap.New()
		}
		snapshotMTime = current
	}
	if snapshotCache.Len() == 0 {
		return nil
	}
	return snapshotCache
}

func LoadSnapshot() *omap.Map { return reloadSnapshot() }

// Invalidate drops the snapshot. Called when the token changes: pool figures fetched under
// another token's visibility must not keep gating requests.
func Invalidate() {
	if err := os.Remove(SnapshotPath); err != nil && !os.IsNotExist(err) {
		log.Printf("WARNING mr: could not remove %s: %v", SnapshotPath, err)
	}
	snapshotMu.Lock()
	snapshotCache = nil
	snapshotMTime = 0
	snapshotMu.Unlock()
}

// intervalSeconds is the gap the schedule leaves between the last run and the next -- what
// "stale" is measured against.
func intervalSeconds(schedule string, fetchedAt float64) float64 {
	anchor := time.Unix(0, int64(fetchedAt*1e9)).UTC()
	next, err := cronexpr.NextAfter(schedule, anchor)
	if err != nil {
		return 3600
	}
	after, err := cronexpr.NextAfter(schedule, next)
	if err != nil {
		return 3600
	}
	gap := after.Sub(next).Seconds()
	if gap < 60 {
		return 60
	}
	return gap
}

// NextRunAt is when the next poll is due (epoch seconds), or nil while polling is off.
func NextRunAt(cfg *config.RouterConfig, snapshot *omap.Map) *float64 {
	conf := Config(cfg)
	if !conf.Enabled {
		return nil
	}
	snap := snapshot
	if snap == nil {
		snap = LoadSnapshot()
	}
	anchor := 0.0
	if snap != nil {
		anchor = snap.Float("fetched_at", 0)
	}
	if anchor == 0 {
		stamp := now()
		return &stamp
	}
	next, err := cronexpr.NextAfter(conf.Schedule, time.Unix(0, int64(anchor*1e9)).UTC())
	if err != nil {
		return nil
	}
	seconds := float64(next.UnixNano()) / 1e9
	return &seconds
}

func Due(cfg *config.RouterConfig) bool {
	next := NextRunAt(cfg, nil)
	return next != nil && now() >= *next
}

func IsStale(cfg *config.RouterConfig, snapshot *omap.Map, stamp float64) bool {
	conf := Config(cfg)
	fetched := snapshot.Float("fetched_at", 0)
	if fetched == 0 {
		return true
	}
	limit := staleIntervals * intervalSeconds(conf.Schedule, fetched)
	if limit < staleFloor {
		limit = staleFloor
	}
	if stamp == 0 {
		stamp = now()
	}
	return (stamp - fetched) > limit
}

// AcquireLease claims the right to poll, so N workers do not all poll at once.
func AcquireLease() bool {
	pid := os.Getpid()
	lease := jsonfile.Read(LockPath)
	if lease.Float("expires_at", 0) > now() && lease.Int("owner_pid", 0) != pid {
		return false
	}
	_ = jsonfile.Write(LockPath, mapOf("owner_pid", num(pid), "expires_at", fnum(now()+leaseTTL)))
	return jsonfile.Read(LockPath).Int("owner_pid", 0) == pid
}

func ReleaseLease() {
	if jsonfile.Read(LockPath).Int("owner_pid", 0) == os.Getpid() {
		_ = jsonfile.Write(LockPath, mapOf("owner_pid", nil, "expires_at", num(0)))
	}
}
