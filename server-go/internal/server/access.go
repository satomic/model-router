package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/ghadmin"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/keypolicy"
	"github.com/satomic/model-router/server-go/internal/modelpolicy"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/scopepolicy"
)

// merge copies a verdict's fields into an outer object, the way the Python backend's `**verdict`
// spread did -- the console reads `allowed` and `reason` at the top level.
func merge(out, verdict *omap.Map) *omap.Map {
	for _, key := range verdict.Keys() {
		out.Set(key, verdict.Value(key))
	}
	return out
}

// accessMe answers "may I create API keys, and on what evidence". The UI turns this into an
// explicit explanation.
func (a *App) accessMe(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	cfg := a.Config()
	login := user.Str("login")
	isAdmin := user.Bool("is_admin", false)
	// Both verdicts in one request: the page asks them together, and the second is nested rather
	// than merged because the two share field names ("allowed", "reason") that must not collide.
	var verdict, keyScope *omap.Map
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); verdict = keypolicy.Evaluate(r.Context(), cfg, login, isAdmin) }()
	go func() { defer wg.Done(); keyScope = scopepolicy.Evaluate(r.Context(), cfg, login, isAdmin) }()
	wg.Wait()

	out := mapOf("login", login, "is_admin", isAdmin)
	merge(out, verdict)
	out.Set("key_scope", keyScope)
	writeJSON(w, http.StatusOK, out, nil)
	return nil
}

// accessTokenStatus lets an administrator inspect the current token's owner and scopes. It
// returns **only a mask**; the plaintext is never echoed back.
func (a *App) accessTokenStatus(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	token := a.Config().GHAdminToken()
	if token == "" {
		writeJSON(w, http.StatusOK, mapOf("configured", false, "hint", "", "owner", nil, "error", nil), nil)
		return nil
	}
	hint := "configured"
	if len(token) > 12 {
		hint = token[:7] + "..." + token[len(token)-4:]
	}
	owner, err := ghadmin.VerifyToken(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusOK, mapOf("configured", true, "hint", hint, "owner", nil, "error", err.Error()), nil)
		return nil
	}
	writeJSON(w, http.StatusOK, mapOf("configured", true, "hint", hint, "owner", owner, "error", nil), nil)
	return nil
}

// accessVerifyToken validates a token (possibly an unsaved draft) and returns its owner and scopes.
func (a *App) accessVerifyToken(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(payload.Str("token"))
	if token == "" {
		token = a.Config().GHAdminToken()
	}
	if token == "" {
		return errorf(http.StatusUnprocessableEntity, "enter a token first")
	}
	owner, err := ghadmin.VerifyToken(r.Context(), token)
	if err != nil {
		return errorf(http.StatusBadRequest, "%s", err.Error())
	}
	writeJSON(w, http.StatusOK, owner, nil)
	return nil
}

// accessDiscover finds the enterprises visible to the token, plus each one's orgs and enterprise
// teams, for the policy configuration page to choose from.
//
// Served from data/github/structure.json unless ?refresh=1. That is what removes the latency
// this page used to have: enumerating the orgs of a large enterprise is several paginated
// GraphQL round trips, and the answer changes far less often than the page is opened. The
// response carries `cached` and `fetched_at` so the UI can say which it is rather than
// presenting stale data as live.
func (a *App) accessDiscover(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	cfg := a.Config()
	token := cfg.GHAdminToken()
	if token == "" {
		return errorf(http.StatusUnprocessableEntity,
			"no GitHub Enterprise token configured; enter and save one to fetch "+
				"the enterprise list automatically")
	}
	refresh := queryBool(r, "refresh")
	if !refresh {
		if cached := ghcache.CachedStructure(cfg); cached != nil {
			writeJSON(w, http.StatusOK, cached, nil)
			return nil
		}
	} else {
		ghadmin.InvalidateCache()
	}
	discovered, err := ghadmin.Discover(r.Context(), token)
	if err != nil {
		return errorf(http.StatusBadRequest, "%s", err.Error())
	}
	// Write the fresh structure back so the next page load is free, and so the refresh loop has
	// something to work from before its first tick.
	ghcache.StoreStructure(cfg, discovered.Slice("enterprises"))
	out := discovered.Clone()
	out.Set("cached", false)
	out.Set("fetched_at", json.Number(strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', -1, 64)))
	writeJSON(w, http.StatusOK, out, nil)
	return nil
}

// accessCacheStatus reports the state of the on-disk GitHub cache: ages, per-scope counts,
// truncation, errors.
func (a *App) accessCacheStatus(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ghcache.Status(a.Config()), nil)
	return nil
}

// accessCacheRefresh forces a refresh now, instead of waiting for the background loop.
func (a *App) accessCacheRefresh(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	cfg := a.Config()
	if cfg.GHAdminToken() == "" {
		return errorf(http.StatusUnprocessableEntity, "no GitHub Enterprise token configured")
	}
	writeJSON(w, http.StatusOK, ghcache.Refresh(r.Context(), cfg), nil)
	return nil
}

// -- Model policy -------------------------------------------------------------

// modelAPIType is the connection type a catalog model resolves through, or "" when its
// connection is gone.
//
// Resolved through the same provider lookup the router uses, so a model bound to a renamed or
// deleted connection reports nothing rather than a stale type.
func (a *App) modelAPIType(name string) string {
	cfg := a.Config()
	provider := cfg.GetProvider(cfg.ModelMeta(name).Str("provider"))
	if provider == nil {
		return ""
	}
	return provider.APIType
}

// myAvailableModels answers "which models may I use, and why".
//
// Session-authenticated rather than key-authenticated, because this feeds the console's own
// "Available models" page. It is the same resolution the API path applies -- one call into
// internal/modelpolicy -- so the page cannot drift from what a request would actually be allowed.
//
// `contributions` names only the grants that applied. Listing the teams and organizations the
// caller is *not* in would publish the policy tables to everybody who can sign in.
func (a *App) myAvailableModels(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	cfg := a.Config()
	verdict := modelpolicy.Evaluate(r.Context(), cfg, user.Str("login"), user.Bool("is_admin", false))
	out := mapOf("login", user.Str("login"), "is_admin", user.Bool("is_admin", false))
	merge(out, verdict)

	// The metadata the page renders next to each name. Taken from the catalog rather than
	// duplicated into the group, so a description edit shows up here without touching groups.
	catalog := omap.New()
	models := verdict.Slice("models")
	for _, item := range models {
		name := omap.AsString(item)
		meta := cfg.ModelMeta(name)
		catalog.Set(name, mapOf(
			"description", meta.Str("description"),
			"reasoning", meta.Bool("reasoning", false),
			"default", meta.Bool("default", false),
			// The connection type behind the model. Needed by the key scope editor, which offers
			// "every model of this type" as a scope and cannot describe what that covers without
			// knowing which type each model sits on.
			"api_type", a.modelAPIType(name),
		))
	}
	out.Set("catalog", catalog)

	var defaultModel any
	inVerdict := map[string]bool{}
	for _, item := range models {
		inVerdict[omap.AsString(item)] = true
	}
	if cfg.Models.Len() > 0 && inVerdict[cfg.DefaultModel()] {
		defaultModel = cfg.DefaultModel()
	} else if len(models) > 0 {
		defaultModel = models[0]
	}
	out.Set("default_model", defaultModel)
	writeJSON(w, http.StatusOK, out, nil)
	return nil
}

// How many known logins one request will evaluate for key-creation permission, and how many of
// those evaluations may be in flight at once. Both are guards on a cost that is not the caller's:
// an uncached verdict is one or more live GitHub calls, so an unbounded loop over a long
// known_users.json would turn one page load into hundreds of API calls. Users past the cap are
// reported as "unknown" rather than quietly dropped.
const (
	maxEligibilityUsers    = 200
	eligibilityConcurrency = 8
)

// keyEligibility maps login -> may that login create an API key, under the saved key policy.
//
// Deliberately the same keypolicy.Evaluate the Keys page shows the user themselves, rather than
// a second reading of the config: a page that filters by its own idea of the rule would
// eventually disagree with the rule that is actually enforced, and an administrator would be
// configuring a list against a permission nobody has. Cache-first through ghcache means a
// deployment with a warm member list answers this with zero GitHub calls.
func (a *App) keyEligibility(ctx context.Context, logins []string) map[string]bool {
	cfg := a.Config()
	results := make([]*bool, len(logins))
	semaphore := make(chan struct{}, eligibilityConcurrency)
	var wg sync.WaitGroup
	for i, login := range logins {
		wg.Add(1)
		go func(i int, login string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			defer func() {
				if rec := recover(); rec != nil { // a display filter must not fail the page
					logWarn("key-creation eligibility failed for %s: %v", login, rec)
				}
			}()
			verdict := keypolicy.Evaluate(ctx, cfg, login, cfg.IsAdminLogin(login))
			allowed := verdict.Bool("allowed", false)
			results[i] = &allowed
		}(i, login)
	}
	wg.Wait()
	out := map[string]bool{}
	for i, login := range logins {
		if results[i] != nil {
			out[login] = *results[i]
		}
	}
	return out
}

// listSignedInUsers is administrators only: every login that has ever signed in.
//
// Read from data/known_users.json rather than from the session table -- sessions are purged when
// they expire, so they can only ever answer "who is signed in right now", which is not the
// question. This is what makes assigning a model group to a user possible without asking them to
// spell their GitHub login.
//
// `eligibility=1` adds `can_create_key` per user, which the key-scope allow list needs to offer
// only accounts that may create a key at all. It is opt-in because it costs a policy evaluation
// per user, and the model-policy table that shares this endpoint does not need it.
func (a *App) listSignedInUsers(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	cfg := a.Config()
	users := a.AuthStore.ListKnownUsers()
	// The group each user currently resolves to by binding, so the admin table can show it
	// without a second round trip per row. Bindings are read straight from the policy: doing a
	// full modelpolicy.Evaluate per user would mean a GitHub membership check per row.
	bindings := map[string]any{}
	if table := cfg.ModelPolicy.Map("users"); table != nil {
		for _, key := range table.Keys() {
			bindings[strings.ToLower(strings.TrimSpace(key))] = table.Value(key)
		}
	}

	eligibility := queryBool(r, "eligibility")
	allowed := map[string]bool{}
	truncated := false
	if eligibility {
		logins := []string{}
		for _, user := range users {
			if login := user.Str("login"); login != "" {
				logins = append(logins, login)
			}
		}
		truncated = len(logins) > maxEligibilityUsers
		if truncated {
			logins = logins[:maxEligibilityUsers]
		}
		allowed = a.keyEligibility(r.Context(), logins)
	}

	rows := []any{}
	for _, user := range users {
		row := user.Clone()
		group := bindings[strings.ToLower(user.Str("login"))]
		if group == nil {
			group = ""
		}
		row.Set("model_group", group)
		if eligibility {
			// nil, not false, when the verdict is unknown: "we could not tell" and "not allowed"
			// are different answers, and a page that hides its rows by this field must not hide a
			// row it never managed to evaluate.
			if value, known := allowed[user.Str("login")]; known {
				row.Set("can_create_key", value)
			} else {
				row.Set("can_create_key", nil)
			}
		}
		rows = append(rows, row)
	}

	writeJSON(w, http.StatusOK, mapOf(
		"users", rows,
		"default_group", cfg.DefaultGroup(),
		"policy_enabled", cfg.ModelPolicyEnabled(),
		"key_policy_enabled", cfg.KeyPolicy.Bool("enabled", false),
		"eligibility_evaluated", eligibility,
		"eligibility_truncated", truncated,
	), nil)
	return nil
}

var (
	membershipNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]*(/[0-9]+)?$`)
	topologyLoginPattern  = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)
)

func (a *App) topologyMembers(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	cfg := a.Config()
	query := r.URL.Query()
	kind := query.Get("kind")
	if kind != "organization" && kind != "team" && kind != "known" {
		return errorf(http.StatusUnprocessableEntity, "invalid membership scope")
	}
	page := queryInt(r, "page", 1)
	if page < 1 || page > 10000 {
		return errorf(http.StatusUnprocessableEntity, "invalid membership scope")
	}

	if kind == "known" {
		known := omap.New()
		for _, user := range a.AuthStore.ListKnownUsers() {
			known.Set(strings.ToLower(user.Str("login")), user)
		}
		logins := map[string]bool{}
		for _, login := range cfg.AdminLogins {
			logins[login] = true
		}
		if table := cfg.ModelPolicy.Map("users"); table != nil {
			for _, key := range table.Keys() {
				logins[key] = true
			}
		}
		for _, login := range omap.StringSlice(cfg.KeyScopePolicy.Value("users")) {
			logins[login] = true
		}
		for login := range logins {
			lowered := strings.ToLower(login)
			if !known.Has(lowered) {
				known.Set(lowered, mapOf("login", login, "name", login, "kind", "github"))
			}
		}
		keys := known.Keys()
		sortStrings(keys)
		offset := (page - 1) * 50
		rows := []any{}
		for i := offset; i < offset+50 && i < len(keys); i++ {
			user := known.Map(keys[i])
			name := user.Str("name")
			if name == "" {
				name = user.Str("login")
			}
			entryKind := user.Str("kind")
			if entryKind == "" {
				entryKind = "github"
			}
			rows = append(rows, mapOf("login", user.Value("login"), "name", name, "kind", entryKind))
		}
		writeJSON(w, http.StatusOK, mapOf(
			"users", rows, "page", json.Number(strconv.Itoa(page)),
			"has_more", offset+50 < len(keys), "source", "registry",
		), nil)
		return nil
	}

	name := query.Get("name")
	if len(name) > 200 || !membershipNamePattern.MatchString(name) {
		return errorf(http.StatusUnprocessableEntity, "invalid membership scope")
	}
	if name == "" || (kind == "team") != strings.Contains(name, "/") {
		return errorf(http.StatusUnprocessableEntity, "invalid membership scope")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result, err := ghcache.ScopeMembersPage(ctx, cfg, kind, name, page)
	if err != nil {
		detail := err.Error()
		if detail == "" {
			detail = "GitHub membership request timed out"
		}
		return errorf(http.StatusServiceUnavailable, "%s", detail)
	}
	writeJSON(w, http.StatusOK, result, nil)
	return nil
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func (a *App) topologyUser(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	login := strings.TrimSpace(r.URL.Query().Get("login"))
	if login == "" || len(login) > 100 || !topologyLoginPattern.MatchString(login) {
		return errorf(http.StatusUnprocessableEntity, "invalid login")
	}
	login = strings.ToLower(login)
	cfg := a.Config()
	administrator := a.isAdminLogin(login)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var access, keyScope, modelPolicy *omap.Map
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); access = keypolicy.Evaluate(ctx, cfg, login, administrator) }()
	go func() { defer wg.Done(); keyScope = scopepolicy.Evaluate(ctx, cfg, login, administrator) }()
	go func() { defer wg.Done(); modelPolicy = modelpolicy.Evaluate(ctx, cfg, login, administrator) }()
	wg.Wait()

	writeJSON(w, http.StatusOK, mapOf(
		"login", login,
		"is_admin", administrator,
		"access", nilIfEmptyMap(access),
		"key_scope", nilIfEmptyMap(keyScope),
		"model_policy", nilIfEmptyMap(modelPolicy),
		"keys", toAny(a.AuthStore.ListAPIKeys(login, false)),
	), nil)
	return nil
}
