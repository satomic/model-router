package aicredits

import (
	"fmt"
	"sort"
	"strings"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// UserHeadroomUSD is how much this user may still consume on Copilot under their user-level
// budget. nil = no user-level budget applies, i.e. only the pool limits them.
func UserHeadroomUSD(entry *omap.Map, login string) *float64 {
	users := entry.Map("users")
	if users == nil {
		return nil
	}
	if record := users.Map(login); record != nil {
		headroom := record.Float("target_usd", 0) - record.Float("consumed_usd", 0)
		return &headroom
	}
	if entry.Value("universal_budget_usd") != nil {
		universal := entry.Float("universal_budget_usd", 0)
		return &universal
	}
	return nil
}

func render(template string, entry *omap.Map, headroom, target *float64) string {
	var remaining *float64
	if entry.Value("remaining") != nil {
		v := entry.Float("remaining", 0)
		remaining = &v
	}
	total := entry.Float("pool_total", 0)
	note := ""
	if remaining != nil && total != 0 {
		note = fmt.Sprintf(" (about %s of %s credits)", commas(*remaining, 0), commas(total, 0))
	}
	name := entry.Str("name")
	if name == "" {
		name = entry.Str("slug")
	}
	replacements := [][2]string{
		{"{enterprise}", name},
		{"{slug}", entry.Str("slug")},
		{"{remaining_note}", note},
		{"{remaining}", optional(remaining, 0)},
		{"{total}", zeroAsUnknown(total, 0)},
		{"{budget_remaining_usd}", optional(headroom, 2)},
		{"{budget_total_usd}", optional(target, 2)},
		{"{budget_remaining_credits}", optionalScaled(headroom, 1/USDPerCredit, 0)},
	}
	text := template
	for _, pair := range replacements {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

func optional(v *float64, decimals int) string {
	if v == nil {
		return "?"
	}
	return commas(*v, decimals)
}

func optionalScaled(v *float64, factor float64, decimals int) string {
	if v == nil {
		return "?"
	}
	return commas(*v*factor, decimals)
}

func zeroAsUnknown(v float64, decimals int) string {
	if v == 0 {
		return "?"
	}
	return commas(v, decimals)
}

// commas formats a number with thousands separators, matching the "{:,.Nf}" the Python backend
// used -- the gate's note is user-facing text that must read the same either way.
func commas(v float64, decimals int) string {
	formatted := fmt.Sprintf("%.*f", decimals, v)
	sign := ""
	if strings.HasPrefix(formatted, "-") {
		sign, formatted = "-", formatted[1:]
	}
	whole, fraction, hasFraction := strings.Cut(formatted, ".")
	var grouped strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	out := sign + grouped.String()
	if hasFraction {
		out += "." + fraction
	}
	return out
}

// Membership reports whether this login belongs to the enterprise: true / false / nil (cannot
// tell).
//
// A budget record is proof (the user consumed credits there). The seat list is authoritative
// when GitHub returned one, so absence from it is a real "no". Without a seat list the key
// policy's cached member lists decide, and a login none of them mention is *unknown* rather
// than absent -- those lists only cover the scopes the policy names.
func Membership(entry *omap.Map, login string) *bool {
	yes, no := true, false
	if users := entry.Map("users"); users != nil && users.Has(login) {
		return &yes
	}
	if seatLogins := entry.Map("seat_logins"); seatLogins != nil {
		if seatLogins.Has(login) {
			return &yes
		}
		return &no
	}
	if members := entry.Map("member_logins"); members != nil && members.Has(login) {
		return &yes
	}
	return nil
}

// Evaluate is the gate's verdict for one login against one enterprise, or nil to let through.
//
// Two regimes, chosen by per_user:
//   - enterprise mode: the caller must belong to this enterprise and its pool must have credits
//     left;
//   - per-user mode: the caller's own user-level budget decides when one applies -- headroom
//     left means "use Copilot" even if the pool is exhausted (the budget is pre-approved Copilot
//     spend), none left means GitHub blocks them and BYOK stays open. A caller with no user-level
//     budget falls back to the pool test.
//
// Unknown membership lets the request through: gating someone whose enterprise we cannot place
// would send them to a pool that may not be theirs -- exactly the bug this guards.
func Evaluate(conf Settings, entry *omap.Map, login string) *omap.Map {
	if entry.Value("error") != nil && entry.Str("error") != "" {
		return nil
	}
	login = strings.ToLower(strings.TrimSpace(login))
	member := Membership(entry, login)
	if member == nil || !*member {
		return nil
	}
	remaining := entry.Value("remaining")
	poolOK := entry.Str("state") == PoolAvailable &&
		(remaining == nil || entry.Float("remaining", 0) > conf.MinRemainingCredits)

	base := func() *omap.Map {
		return mapOf(
			"enterprise", entry.Value("slug"),
			"enterprise_name", entry.Value("name"),
			"remaining", remaining,
			"pool_total", entry.Value("pool_total"),
			"pool_ok", poolOK,
		)
	}

	if conf.PerUser {
		if headroom := UserHeadroomUSD(entry, login); headroom != nil {
			if *headroom <= 0 {
				return nil
			}
			var target *float64
			if record := entry.Map("users").Map(login); record != nil && record.Value("target_usd") != nil {
				v := record.Float("target_usd", 0)
				target = &v
			} else if entry.Value("universal_budget_usd") != nil {
				v := entry.Float("universal_budget_usd", 0)
				target = &v
			}
			verdict := base()
			verdict.Set("reason", ReasonBudget)
			verdict.Set("headroom_usd", fnum(*headroom))
			if target != nil {
				verdict.Set("budget_usd", fnum(*target))
			} else {
				verdict.Set("budget_usd", nil)
			}
			template := conf.MessageBudget
			if template == "" {
				template = DefaultMessageBudget
			}
			verdict.Set("message", render(template, entry, headroom, target))
			return verdict
		}
	}
	if poolOK {
		verdict := base()
		verdict.Set("reason", ReasonPool)
		template := conf.Message
		if template == "" {
			template = DefaultMessage
		}
		verdict.Set("message", render(template, entry, nil, nil))
		return verdict
	}
	return nil
}

// Gate decides whether this caller's request is answered with a note instead of being routed.
//
// Returns nil to let the request through, otherwise the verdict from Evaluate (with `message`,
// `reason`, `enterprise`). Every uncertainty resolves to "let it through": a snapshot from
// another token, a stale snapshot, an enterprise the caller cannot be placed in, a user GitHub
// itself would block. The gate protects a budget; it must never be the reason a developer
// cannot work.
func Gate(cfg *config.RouterConfig, login string) *omap.Map {
	conf := Config(cfg)
	if !conf.GateEnabled {
		return nil
	}
	snap := LoadSnapshot()
	if snap == nil || snap.Str("token_fp") != ghcache.TokenFP(cfg.GHAdminToken()) {
		return nil
	}
	if IsStale(cfg, snap, 0) {
		return nil
	}
	enterprises := snap.Map("enterprises")
	if enterprises == nil {
		return nil
	}
	for _, slug := range enterprises.Keys() {
		entry := enterprises.Map(slug)
		if entry == nil {
			continue
		}
		if verdict := Evaluate(conf, entry, login); verdict != nil {
			return verdict
		}
	}
	return nil
}

// -- Status for the console -------------------------------------------------------

// Status is the last pool snapshot plus the gate settings and schedule state.
func Status(cfg *config.RouterConfig) *omap.Map {
	conf := Config(cfg)
	snap := LoadSnapshot()
	stamp := now()
	tokenOK := snap != nil && snap.Str("token_fp") == ghcache.TokenFP(cfg.GHAdminToken())
	next := NextRunAt(cfg, snap)

	included := omap.New()
	planNames := make([]string, 0, len(IncludedCredits))
	for plan := range IncludedCredits {
		planNames = append(planNames, plan)
	}
	sort.Strings(planNames)
	for _, plan := range planNames {
		included.Set(plan, fnum(IncludedCredits[plan]))
	}

	out := mapOf(
		"settings", conf.toMap(),
		"token_configured", cfg.GHAdminToken() != "",
		"default_message", DefaultMessage,
		"default_message_budget", DefaultMessageBudget,
		"included_credits", included,
	)
	if tokenOK {
		out.Set("fetched_at", snap.Value("fetched_at"))
	} else {
		out.Set("fetched_at", nil)
	}
	out.Set("stale", snap != nil && tokenOK && IsStale(cfg, snap, stamp))
	out.Set("token_changed", snap != nil && !tokenOK)
	if next != nil {
		out.Set("next_run_at", fnum(*next))
		out.Set("due", stamp >= *next)
	} else {
		out.Set("next_run_at", nil)
		out.Set("due", false)
	}
	out.Set("snapshot_per_user", snap != nil && snap.Bool("per_user", false))
	if snap != nil && len(snap.Slice("missing_enterprises")) > 0 {
		out.Set("missing_enterprises", snap.Slice("missing_enterprises"))
	} else {
		out.Set("missing_enterprises", []any{})
	}

	rows := []any{}
	if snap != nil && tokenOK {
		if enterprises := snap.Map("enterprises"); enterprises != nil {
			for _, slug := range enterprises.Keys() {
				entry := enterprises.Map(slug)
				if entry == nil {
					continue
				}
				rows = append(rows, enterpriseStatus(conf, entry))
			}
		}
	}
	out.Set("enterprises", rows)
	return out
}

func enterpriseStatus(conf Settings, entry *omap.Map) *omap.Map {
	users := entry.Map("users")
	seatLogins := entry.Map("seat_logins")
	members := entry.Map("member_logins")

	userRows := []any{}
	if users != nil || (seatLogins != nil && seatLogins.Len() > 0) {
		unique := map[string]bool{}
		if seatLogins != nil {
			for _, login := range seatLogins.Keys() {
				unique[login] = true
			}
		}
		if users != nil {
			for _, login := range users.Keys() {
				unique[login] = true
			}
		}
		logins := make([]string, 0, len(unique))
		for login := range unique {
			logins = append(logins, login)
		}
		sort.Strings(logins)
		type row struct {
			consumed float64
			value    *omap.Map
		}
		built := make([]row, 0, len(logins))
		for _, login := range logins {
			var record *omap.Map
			if users != nil {
				record = users.Map(login)
			}
			if record == nil {
				record = omap.New()
			}
			var headroom *float64
			if users != nil {
				headroom = UserHeadroomUSD(entry, login)
			}
			verdict := Evaluate(conf, entry, login)
			item := mapOf(
				"login", login,
				"plan", valueOrNil(seatLogins, login),
				"target_usd", record.Value("target_usd"),
				"consumed_usd", record.Value("consumed_usd"),
				"headroom_usd", floatOrNil(headroom),
				"blocked_on_copilot", headroom != nil && *headroom <= 0,
			)
			// What the gate would do for this login right now, under the saved settings.
			if verdict != nil {
				item.Set("gate", verdict.Value("reason"))
			} else {
				item.Set("gate", nil)
			}
			built = append(built, row{consumed: record.Float("consumed_usd", 0), value: item})
		}
		sort.SliceStable(built, func(i, j int) bool { return built[i].consumed > built[j].consumed })
		for _, item := range built {
			userRows = append(userRows, item.value)
		}
	}

	out := omap.New()
	for _, key := range entry.Keys() {
		if key == "users" || key == "seat_logins" || key == "member_logins" {
			continue
		}
		out.Set(key, entry.Value(key))
	}
	out.Set("users", userRows)
	if seatLogins != nil && seatLogins.Len() > 0 {
		out.Set("seat_count", num(seatLogins.Len()))
	} else {
		out.Set("seat_count", nil)
	}
	switch {
	case seatLogins != nil:
		out.Set("member_source", "seats")
		out.Set("member_count", num(seatLogins.Len()))
	case members != nil && members.Len() > 0:
		out.Set("member_source", "cache")
		out.Set("member_count", num(members.Len()))
	default:
		out.Set("member_source", nil)
		out.Set("member_count", num(0))
	}
	return out
}

func valueOrNil(m *omap.Map, key string) any {
	if m == nil {
		return nil
	}
	if v, ok := m.Get(key); ok {
		return v
	}
	return nil
}

func floatOrNil(v *float64) any {
	if v == nil {
		return nil
	}
	return fnum(*v)
}
