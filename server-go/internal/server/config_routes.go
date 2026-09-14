package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/satomic/model-router/server-go/internal/aicredits"
	"github.com/satomic/model-router/server-go/internal/auth"
	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghadmin"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/modelpolicy"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/routing"
)

// withoutPasswordHash echoes the configuration with the local administrator's password digest
// blanked.
//
// Every other secret in config.yaml (the OAuth client_secret, the enterprise token) is a
// credential the administrator reading this page is expected to manage, so it is returned for
// editing. The password digest is not: an ordinary GitHub administrator is a different principal
// from the local super administrator, and handing them a scrypt digest lets them attempt offline
// recovery of a password they were never given.
//
// The keys are kept and emptied rather than removed, so the console's round-trip (GET -> edit ->
// PUT) keeps its shape; `updated_at` is what the UI reads to tell whether a password has ever
// been set. putConfig restores the stored values on the way back in.
func withoutPasswordHash(raw *omap.Map) *omap.Map {
	doc := raw.Clone()
	if authDoc := doc.Map("auth"); authDoc != nil {
		if la := authDoc.Map("local_admin"); la != nil {
			for _, field := range []string{"password_hash", "password_salt"} {
				if la.Has(field) {
					la.Set(field, "")
				}
			}
		}
	}
	return doc
}

func (a *App) getConfig(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	raw, err := config.LoadRaw()
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not read the configuration: %v", err)
	}
	writeJSON(w, http.StatusOK, withoutPasswordHash(raw), nil)
	return nil
}

// previewDecisionPrompt renders the AI decision prompt (administrators only).
//
// Rendering goes through RouterConfig.RenderDecisionPrompt -- the exact same code path as
// RouteByAI -- so the preview *is* the system content that would be sent, not an approximation
// reimplemented in the frontend.
//
// `models` / `ai_router` may be supplied as **unsaved drafts** in the request, so an admin can
// see the effect of a new catalog or prompt before pressing save.
func (a *App) previewDecisionPrompt(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	raw, err := config.LoadRaw()
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not read the configuration: %v", err)
	}
	raw = raw.Clone()
	if models, ok := payload.Value("models").(*omap.Map); ok {
		raw.Set("models", models)
	}
	if aiRouter, ok := payload.Value("ai_router").(*omap.Map); ok {
		raw.Set("ai_router", aiRouter)
	}
	draft := config.New(raw)

	catalog := draft.ModelCatalogText()
	system := draft.RenderDecisionPrompt(catalog)
	sample := payload.Str("sample_prompt")
	truncated := ""
	if sample != "" {
		truncated = routing.TruncateForDecision(sample, draft.MaxPromptChars)
	}

	// A model with no description is just a bare name in the catalog, leaving the decision model
	// almost nothing to judge it by.
	missingDesc := []any{}
	candidates := []any{}
	for _, name := range draft.Models.Keys() {
		candidates = append(candidates, name)
		if strings.TrimSpace(draft.ModelMeta(name).Str("description")) == "" {
			missingDesc = append(missingDesc, name)
		}
	}

	var defaultModel any
	if draft.Models.Len() > 0 {
		defaultModel = draft.DefaultModel()
	}

	writeJSON(w, http.StatusOK, mapOf(
		"system", system,
		"catalog", catalog,
		"user", truncated,
		"sample_truncated", sample != "" && len([]rune(sample)) > draft.MaxPromptChars,
		"model_count", json.Number(strconv.Itoa(draft.Models.Len())),
		"candidates", candidates,
		"decision_model", draft.DecisionModel,
		"decision_provider", draft.ResolveDecisionModel().Provider.Name,
		"is_default_prompt", draft.DecisionPrompt == config.DefaultDecisionPrompt,
		"has_placeholder", strings.Contains(draft.DecisionPrompt, config.CatalogPlaceholder),
		"models_without_description", missingDesc,
		"default_model", defaultModel,
		"chars", json.Number(strconv.Itoa(len([]rune(system)))),
	), nil)
	return nil
}

// defaultDecisionPrompt is the built-in default, for the UI's "Restore default" button.
func (a *App) defaultDecisionPrompt(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mapOf(
		"prompt", config.DefaultDecisionPrompt,
		"placeholder", config.CatalogPlaceholder,
	), nil)
	return nil
}

// droppedAuthCredentials finds the auth-section credentials this PUT would silently wipe.
//
// SaveRaw replaces whole top-level keys, so submitting only {"auth": {"key_policy": ...}} also
// erases the github credentials and admin_logins in that same section -- the result being that
// nobody can sign in any more, and client_secret cannot be read back from GitHub a second time.
// Rejecting such a submission explicitly avoids that damage.
func droppedAuthCredentials(old, updates *omap.Map) []string {
	if !updates.Has("auth") {
		return nil
	}
	oldAuth := old.Map("auth")
	if oldAuth == nil {
		oldAuth = omap.New()
	}
	newAuth := updates.Map("auth")
	if newAuth == nil {
		newAuth = omap.New()
	}
	lost := []string{}
	oldGH := oldAuth.Map("github")
	if oldGH == nil {
		oldGH = omap.New()
	}
	newGH := newAuth.Map("github")
	if newGH == nil {
		newGH = omap.New()
	}
	for _, field := range []string{"client_id", "client_secret"} {
		if strings.TrimSpace(oldGH.Str(field)) != "" && !newGH.Has(field) {
			lost = append(lost, "auth.github."+field)
		}
	}
	if len(oldAuth.Slice("admin_logins")) > 0 && !newAuth.Has("admin_logins") {
		lost = append(lost, "auth.admin_logins")
	}
	if oldPolicy := oldAuth.Map("key_policy"); oldPolicy != nil &&
		strings.TrimSpace(oldPolicy.Str("github_token")) != "" && !newAuth.Has("key_policy") {
		lost = append(lost, "auth.key_policy.github_token")
	}
	// Losing this one would silently reset the local super administrator to the documented
	// default password -- the most damaging thing a save from an unrelated tab could do. A
	// *blank* submitted hash is not a loss, though: GET /v1/config deliberately blanks it, so
	// every console save carries empty fields and restorePasswordHash puts the stored digest
	// back. Only dropping the key outright is treated as damage.
	if oldLA := oldAuth.Map("local_admin"); oldLA != nil &&
		strings.TrimSpace(oldLA.Str("password_hash")) != "" && !newAuth.Has("local_admin") {
		lost = append(lost, "auth.local_admin.password_hash")
	}
	return lost
}

// restorePasswordHash puts the stored password digest back into an incoming auth section, in
// place.
//
// GET /v1/config blanks the digest, so the console's own round-trip submits empty strings for
// it. Taking those at face value would reset the local super administrator to the default
// password on every unrelated save -- so an empty submitted field means "unchanged", and the
// only way to set the password remains POST /v1/auth/local/password, which requires the current
// one.
func restorePasswordHash(old, updates *omap.Map) {
	newAuth := updates.Map("auth")
	if newAuth == nil {
		return
	}
	newLA := newAuth.Map("local_admin")
	if newLA == nil {
		return
	}
	oldAuth := old.Map("auth")
	if oldAuth == nil {
		return
	}
	oldLA := oldAuth.Map("local_admin")
	if oldLA == nil {
		return
	}
	for _, field := range []string{"password_hash", "password_salt"} {
		if strings.TrimSpace(newLA.Str(field)) == "" && strings.TrimSpace(oldLA.Str(field)) != "" {
			newLA.Set(field, oldLA.Value(field))
		}
	}
}

// putConfig writes the console's configuration back to config.yaml (comments preserved) and
// hot-reloads the runtime configuration.
func (a *App) putConfig(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	updates, err := readJSONObject(r)
	if err != nil {
		return err
	}
	existing, err := config.LoadRaw()
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not read the configuration: %v", err)
	}
	restorePasswordHash(existing, updates)
	if dropped := droppedAuthCredentials(existing, updates); len(dropped) > 0 {
		return &auth.HTTPError{Status: http.StatusUnprocessableEntity, Detail: []any{
			"this submission would clear the following configured items: " +
				strings.Join(dropped, ", ") +
				". The auth section is replaced as a whole, so submit these fields " +
				"along with your changes (pass an empty string or empty list to clear " +
				"one on purpose).",
		}}
	}
	merged := existing.Clone()
	for _, key := range updates.Keys() {
		merged.Set(key, updates.Value(key))
	}
	if errors := config.Validate(merged); len(errors) > 0 {
		detail := make([]any, len(errors))
		for i, message := range errors {
			detail[i] = message
		}
		return &auth.HTTPError{Status: http.StatusUnprocessableEntity, Detail: detail}
	}

	previous := a.Config()
	oldTTL, oldMax := previous.SessionTTL, previous.MaxSessions
	oldPolicy := previous.KeyPolicy
	oldToken := previous.GHAdminToken()

	updated, err := config.SaveRaw(updates)
	if err != nil {
		return errorf(http.StatusInternalServerError, "could not save the configuration: %v", err)
	}
	a.setConfig(updated, updated.SessionTTL != oldTTL || updated.MaxSessions != oldMax)

	// A provider's endpoint/key may have changed, so drop the old clients.
	a.Pool.Invalidate()
	if !sameJSON(updated.ModelPolicy, previous.ModelPolicy) || !sameGroups(updated, previous) {
		// Effective model sets are memoised for a minute; an admin who just edited a group
		// expects the change to be live when they check, not on the next tick.
		modelpolicy.Invalidate()
	}
	if !sameJSON(updated.KeyPolicy, oldPolicy) {
		// The token or the allow list changed, so cached decisions are no longer trustworthy.
		// The on-disk cache has to go too: a new token can see a different set of members, so
		// keeping the old member lists would leave them authoritative under a token that never
		// produced them.
		ghadmin.InvalidateCache()
		ghcache.Invalidate()
		if updated.GHAdminToken() != oldToken {
			// A pool snapshot fetched under another token's visibility must not keep gating.
			aicredits.Invalidate()
		}
	}
	a.AuthStore.RefreshAdminFlags(updated.IsAdminLogin)
	logInfo("config updated: strategy=%s sticky=%v providers=%v",
		updated.Strategy, updated.Sticky, updated.Providers.Keys())
	writeJSON(w, http.StatusOK, mapOf(
		"ok", true, "strategy", updated.Strategy, "sticky", updated.Sticky,
	), nil)
	return nil
}

func sameJSON(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(left) == string(right)
}

func sameGroups(a, b *config.RouterConfig) bool {
	return sameJSON(a.ModelGroups, b.ModelGroups) && sameJSON(a.ModelGroupNames(), b.ModelGroupNames())
}
