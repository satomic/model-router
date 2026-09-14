package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/satomic/model-router/server-go/internal/auth"
	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/keyscope"
	"github.com/satomic/model-router/server-go/internal/localadmin"
	"github.com/satomic/model-router/server-go/internal/modelpolicy"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// listModels is filtered by the model policy, because this endpoint is what a client's model
// picker is populated from: listing a model the very next request would refuse is worse than not
// listing it. An empty list is a legitimate answer -- see the 403 in prepareCall.
func (a *App) listModels(w http.ResponseWriter, r *http.Request) error {
	key, err := a.apiKey(r)
	if err != nil {
		return err
	}
	cfg := a.Config()
	login := key.Str("user_login")
	allowed := modelpolicy.AllowedModels(r.Context(), cfg, login, a.isAdminLogin(login))
	// Narrowed again by the key's own scope, for the same reason: a scoped key's picker must
	// show what that key can actually reach, not what its owner could reach with another key.
	allowed = keyscope.Narrow(cfg, allowed, key.Map("scope"))
	models := cfg.Models
	if allowed != nil {
		models = cfg.RestrictedTo(allowed).Models
	}
	data := []any{}
	for _, name := range models.Keys() {
		data = append(data, mapOf(
			"id", name,
			"object", "model",
			"description", cfg.ModelMeta(name).Str("description"),
		))
	}
	writeJSON(w, http.StatusOK, mapOf("object", "list", "data", data), nil)
	return nil
}

// authStatus is public: the frontend uses this to choose between the setup wizard, the sign-in
// page and the console.
func (a *App) authStatus(w http.ResponseWriter, r *http.Request) error {
	cfg := a.Config()
	user := auth.CurrentUser(r, a.AuthStore, cfg)
	localUsername := ""
	if cfg.LocalAdminEnabled() {
		localUsername = cfg.LocalAdminUsername()
	}
	writeJSON(w, http.StatusOK, mapOf(
		"configured", cfg.OAuthConfigured(),
		"authenticated", user != nil,
		"user", nilIfEmptyMap(user),
		"can_setup", !cfg.OAuthConfigured() && auth.IsLoopback(r),
		"callback_url", auth.CallbackURL(r, cfg.GHCallbackURL),
		// The console offers the local sign-in form on the strength of this flag, which is what
		// keeps it reachable when github.com is not.
		"local_admin_enabled", cfg.LocalAdminEnabled(),
		"local_admin_username", localUsername,
	), nil)
	return nil
}

// localLogin signs in as the local super administrator.
//
// One message for both a wrong username and a wrong password, so this cannot be used to discover
// whether the account has been renamed. Note there is no rate limiting: this is the app's only
// brute-forceable surface, and a deployment exposed beyond a trusted network should disable the
// account or put a proxy in front of it.
func (a *App) localLogin(w http.ResponseWriter, r *http.Request) error {
	cfg := a.Config()
	if !cfg.LocalAdminEnabled() {
		return errorf(http.StatusServiceUnavailable, "the local administrator account is disabled")
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	username := strings.TrimSpace(payload.Str("username"))
	password := payload.Str("password")
	if !cfg.IsLocalAdminLogin(username) || !localadmin.VerifyPassword(cfg.LocalAdminSettings(), password) {
		logWarn("local admin sign-in failed username=%q", headRunes(username, 64))
		return errorf(http.StatusUnauthorized, "incorrect username or password")
	}

	login := cfg.LocalAdminUsername()
	mustChange := localadmin.MustChange(cfg.LocalAdminSettings())
	sid := a.AuthStore.CreateSession(mapOf(
		"login", login,
		"name", login,
		"avatar_url", nil,
		"is_admin", true,
		// Marks the session's authority as coming from auth.local_admin. internal/auth keys both
		// the is_admin recompute and the forced-change gate off this.
		"local_admin", true,
		"must_change_password", mustChange,
	), cfg.AuthSessionTTL)
	auth.SetSessionCookie(w, r, sid, cfg.AuthSessionTTL)
	logInfo("local admin login login=%s must_change=%v", login, mustChange)
	writeJSON(w, http.StatusOK, mapOf("ok", true, "must_change_password", mustChange), nil)
	return nil
}

// localAdminPassword changes the local administrator's username and/or password.
//
// Exempt from the forced-change gate in auth.RequireUser -- it is the one thing an account still
// on the default credential is allowed to do.
func (a *App) localAdminPassword(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	if !user.Bool("local_admin", false) {
		return errorf(http.StatusForbidden, "only the local administrator can change this credential")
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	cfg := a.Config()
	current := payload.Str("current_password")
	newPassword := payload.Str("new_password")
	newUsername := strings.TrimSpace(payload.Str("new_username"))
	if newUsername == "" {
		newUsername = cfg.LocalAdminUsername()
	}

	if !localadmin.VerifyPassword(cfg.LocalAdminSettings(), current) {
		return errorf(http.StatusForbidden, "the current password is incorrect")
	}
	for _, problem := range []string{
		localadmin.ValidateUsername(newUsername),
		localadmin.ValidateNewPassword(newPassword),
	} {
		if problem != "" {
			return errorf(http.StatusUnprocessableEntity, "%s", problem)
		}
	}

	salt, digest, err := localadmin.HashPassword(newPassword, "")
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not hash the new password: %v", err)
	}
	// The whole auth section has to be written back: SaveRaw replaces top-level keys wholesale,
	// so submitting only local_admin would wipe the OAuth credentials.
	existing, err := config.LoadRaw()
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not read the configuration: %v", err)
	}
	authDoc := existing.Map("auth")
	if authDoc == nil {
		authDoc = omap.New()
	}
	la := authDoc.Map("local_admin")
	if la == nil {
		la = omap.New()
	}
	la.Set("enabled", true)
	la.Set("username", newUsername)
	la.Set("password_hash", digest)
	la.Set("password_salt", salt)
	la.Set("updated_at", time.Now().UTC().Format(time.RFC3339))
	authDoc.Set("local_admin", la)

	updated, err := config.SaveRaw(mapOf("auth", authDoc))
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not save the configuration: %v", err)
	}
	a.setConfig(updated, false)
	a.AuthStore.RekeyLocalAdmin(auth.Cookie(r, auth.SessionCookie), newUsername)
	logInfo("local admin credential changed username=%s", newUsername)
	writeJSON(w, http.StatusOK, mapOf("ok", true, "username", newUsername), nil)
	return nil
}

// setLocalAdminEnabled enables or disables the local administrator account (administrators only).
func (a *App) setLocalAdminEnabled(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	enabled := payload.Bool("enabled", false)
	existing, err := config.LoadRaw()
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not read the configuration: %v", err)
	}
	authDoc := existing.Map("auth")
	if authDoc == nil {
		authDoc = omap.New()
	}
	la := authDoc.Map("local_admin")
	if la == nil {
		la = omap.New()
	}
	la.Set("enabled", enabled)
	authDoc.Set("local_admin", la)
	updated, err := config.SaveRaw(mapOf("auth", authDoc))
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not save the configuration: %v", err)
	}
	a.setConfig(updated, false)
	logInfo("local admin account enabled=%v", enabled)
	writeJSON(w, http.StatusOK, mapOf("ok", true, "enabled", enabled), nil)
	return nil
}

// authSetup is first-run setup: available only while OAuth is unconfigured and the request comes
// from the local machine.
func (a *App) authSetup(w http.ResponseWriter, r *http.Request) error {
	cfg := a.Config()
	if cfg.OAuthConfigured() {
		return errorf(http.StatusConflict, "OAuth is already configured; change it from the console instead")
	}
	if !auth.IsLoopback(r) {
		return errorf(http.StatusForbidden,
			"setup is only allowed from the local machine (127.0.0.1); "+
				"for a remote deployment, edit config.yaml directly")
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	clientID := strings.TrimSpace(payload.Str("client_id"))
	clientSecret := strings.TrimSpace(payload.Str("client_secret"))
	adminLogins := []any{}
	for _, login := range omap.StringSlice(payload.Value("admin_logins")) {
		if trimmed := strings.TrimSpace(login); trimmed != "" {
			adminLogins = append(adminLogins, trimmed)
		}
	}
	if clientID == "" || clientSecret == "" {
		return errorf(http.StatusUnprocessableEntity, "client_id and client_secret are required")
	}
	if len(adminLogins) == 0 {
		return errorf(http.StatusUnprocessableEntity, "at least one administrator GitHub login is required")
	}

	existing, err := config.LoadRaw()
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not read the configuration: %v", err)
	}
	authDoc := existing.Map("auth")
	if authDoc == nil {
		authDoc = omap.New()
	}
	authDoc.Set("github", mapOf(
		"client_id", clientID,
		"client_secret", clientSecret,
		"callback_url", strings.TrimSpace(payload.Str("callback_url")),
	))
	authDoc.Set("admin_logins", adminLogins)
	if !authDoc.Has("allow_any_github_user") {
		authDoc.Set("allow_any_github_user", true)
	}
	if !authDoc.Has("session_ttl_seconds") {
		authDoc.Set("session_ttl_seconds", json.Number("604800"))
	}
	updated, err := config.SaveRaw(mapOf("auth", authDoc))
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not save the configuration: %v", err)
	}
	a.setConfig(updated, false)
	logInfo("OAuth setup completed admins=%v", adminLogins)
	writeJSON(w, http.StatusOK, mapOf(
		"ok", true,
		"callback_url", auth.CallbackURL(r, updated.GHCallbackURL),
	), nil)
	return nil
}

func (a *App) githubLogin(w http.ResponseWriter, r *http.Request) error {
	cfg := a.Config()
	if !cfg.OAuthConfigured() {
		return errorf(http.StatusServiceUnavailable, "GitHub OAuth is not configured")
	}
	url, state := auth.BuildAuthorizeURL(r, cfg)
	auth.SetStateCookie(w, r, state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
	return nil
}

func (a *App) githubCallback(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	if errText := query.Get("error"); errText != "" {
		http.Redirect(w, r, "/?login_error="+errText, http.StatusTemporaryRedirect)
		return nil
	}
	code := query.Get("code")
	if code == "" {
		return errorf(http.StatusBadRequest, "missing code")
	}
	expected := auth.Cookie(r, auth.StateCookie)
	if expected == "" || query.Get("state") != expected {
		return errorf(http.StatusBadRequest, "state verification failed, please sign in again")
	}

	cfg := a.Config()
	user, err := auth.ExchangeCodeForUser(r.Context(), r, cfg, code)
	if err != nil {
		return err
	}
	sid := a.AuthStore.CreateSession(user, cfg.AuthSessionTTL)
	auth.SetSessionCookie(w, r, sid, cfg.AuthSessionTTL)
	auth.ClearStateCookie(w)
	logInfo("login login=%s admin=%v", user.Str("login"), user.Bool("is_admin", false))
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	return nil
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) error {
	a.AuthStore.DeleteSession(auth.Cookie(r, auth.SessionCookie))
	auth.ClearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, mapOf("ok", true), nil)
	return nil
}
