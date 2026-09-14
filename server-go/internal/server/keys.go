package server

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/keypolicy"
	"github.com/satomic/model-router/server-go/internal/keyscope"
	"github.com/satomic/model-router/server-go/internal/modelpolicy"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/scopepolicy"
)

func toAny(items []*omap.Map) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func (a *App) listKeys(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	// scope is empty only for the administrator's cross-user view, and that view must never
	// carry plaintext: an administrator may disable or delete anybody's key, not read it.
	// Anywhere else scope is the caller's own login, so they see their own keys in full.
	scope := user.Str("login")
	if queryBool(r, "all") && user.Bool("is_admin", false) {
		scope = ""
	}
	writeJSON(w, http.StatusOK, toAny(a.AuthStore.ListAPIKeys(scope, scope != "")), nil)
	return nil
}

func (a *App) createKey(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	cfg := a.Config()
	// This is the enterprise access-control gate: without a key you cannot use BYOK, so the
	// authorization decision lives at "create key" rather than "sign in" -- signing in is not
	// authorization by itself.
	verdict := keypolicy.Evaluate(r.Context(), cfg, user.Str("login"), user.Bool("is_admin", false))
	if !verdict.Bool("allowed", false) {
		logInfo("api key denied user=%s reason=%s", user.Str("login"), verdict.Str("reason"))
		return errorf(http.StatusForbidden, "%s", verdict.Str("reason"))
	}

	scope, err := a.validatedScope(r.Context(), cfg, payload.Value("scope"), user, user)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(payload.Str("name"))
	if name == "" {
		name = "default"
	}
	record, plaintext := a.AuthStore.CreateAPIKey(user.Str("login"), name, scope)
	logInfo("api key created user=%s id=%s scope=%s",
		user.Str("login"), record.Str("id"), keyscope.Describe(scope))
	// record already carries the plaintext (the caller is by definition its owner); the explicit
	// key here keeps the response shape obvious at the call site.
	record.Set("key", plaintext)
	writeJSON(w, http.StatusOK, record, nil)
	return nil
}

func (a *App) updateKey(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	keyID := r.PathValue("key_id")
	record := a.AuthStore.GetAPIKey(keyID)
	if record == nil || (!user.Bool("is_admin", false) && record.Str("user_login") != user.Str("login")) {
		return errorf(http.StatusNotFound, "key not found")
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	// Only the fields actually sent are touched, so the console can save a scope without having
	// to resend a disabled flag it is not editing (and vice versa).
	patch := omap.New()
	if payload.Has("disabled") {
		patch.Set("disabled", payload.Bool("disabled", false))
	}
	if payload.Has("name") {
		name := strings.TrimSpace(payload.Str("name"))
		if name == "" {
			name = "default"
		}
		patch.Set("name", name)
	}
	if payload.Has("scope") {
		// Validated against the key's *owner*, not the caller: an administrator editing somebody
		// else's key must not be able to widen it past what that person may use.
		ownerLogin := record.Str("user_login")
		owner := mapOf("login", ownerLogin, "is_admin", a.isAdminLogin(ownerLogin))
		scope, err := a.validatedScope(r.Context(), a.Config(), payload.Value("scope"), owner, user)
		if err != nil {
			return err
		}
		patch.Set("scope", scope)
	}
	updated := a.AuthStore.SetAPIKeyFields(keyID, patch)
	if updated == nil {
		return errorf(http.StatusNotFound, "key not found")
	}
	fields := patch.Keys()
	sort.Strings(fields)
	logInfo("api key updated user=%s id=%s fields=%s", user.Str("login"), keyID, strings.Join(fields, ","))
	writeJSON(w, http.StatusOK, updated, nil)
	return nil
}

// validatedScope normalizes an incoming key scope, and refuses one the owner could not use
// anyway.
//
// Explicitly named models are checked against the owner's current policy set, because the user
// picked them from a list and a name that is not on it is a mistake worth reporting now.
// Connection types are deliberately not checked: that scope is a rule, so selecting a type
// before an administrator has granted any model of that type is a legitimate thing to do, and it
// starts working on its own once they do.
//
// Two different people are consulted, on purpose. `owner` answers "which models may this key
// reach", because the key belongs to them. `actor` answers "may a scope be set at all", because
// that is a permission to perform an action and the person performing it is the caller -- an
// administrator narrowing somebody else's key is exercising their own authority, not that
// person's. The permission gate lives here rather than in the two endpoints so that no future
// caller can add a third way to set a scope and forget it.
func (a *App) validatedScope(ctx context.Context, cfg *config.RouterConfig, raw any, owner, actor *omap.Map) (*omap.Map, error) {
	scope, err := keyscope.Normalize(raw, cfg)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "%s", err.Error())
	}
	if scope.Str("kind") != keyscope.KindAll {
		// Only narrowing is gated. Widening a key back to "everything its owner may reach" is
		// always allowed: it is the documented default, it carries no cost risk, and refusing it
		// would trap an already-narrowed key in place once the permission was withdrawn.
		verdict := scopepolicy.Evaluate(ctx, cfg, actor.Str("login"), actor.Bool("is_admin", false))
		if !verdict.Bool("allowed", false) {
			logInfo("key scope denied user=%s scope=%s reason=%s",
				actor.Str("login"), keyscope.Describe(scope), verdict.Str("reason"))
			return nil, errorf(http.StatusForbidden, "%s", verdict.Str("reason"))
		}
	}
	if scope.Str("kind") == "models" {
		allowed := modelpolicy.AllowedModels(ctx, cfg, owner.Str("login"), owner.Bool("is_admin", false))
		if allowed != nil {
			permitted := map[string]bool{}
			for _, model := range allowed {
				permitted[model] = true
			}
			denied := []string{}
			for _, model := range omap.StringSlice(scope.Value("models")) {
				if !permitted[model] {
					denied = append(denied, model)
				}
			}
			if len(denied) > 0 {
				return nil, errorf(http.StatusBadRequest, "not available to you: %s", strings.Join(denied, ", "))
			}
		}
	}
	return scope, nil
}

func (a *App) deleteKey(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	keyID := r.PathValue("key_id")
	record := a.AuthStore.GetAPIKey(keyID)
	if record == nil || (!user.Bool("is_admin", false) && record.Str("user_login") != user.Str("login")) {
		return errorf(http.StatusNotFound, "key not found")
	}
	a.AuthStore.DeleteAPIKey(keyID)
	logInfo("api key deleted user=%s id=%s", user.Str("login"), keyID)
	writeJSON(w, http.StatusOK, mapOf("ok", true), nil)
	return nil
}
