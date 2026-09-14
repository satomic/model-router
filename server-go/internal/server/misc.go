package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/satomic/model-router/server-go/internal/aicredits"
	"github.com/satomic/model-router/server-go/internal/cronexpr"
	"github.com/satomic/model-router/server-go/internal/release"
	"github.com/satomic/model-router/server-go/internal/traces"
	"github.com/satomic/model-router/server-go/internal/usagestats"
	"github.com/satomic/model-router/server-go/internal/version"
)

// -- Usage statistics ---------------------------------------------------------

// usage serves the Usage page: non-admins see only their own data; admins see everything or one
// named user.
//
// Served entirely from the background rollup -- no trace file is opened here, which is what
// keeps the page fast once the trace directory is large.
func (a *App) usage(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	scope := user.Str("login")
	if user.Bool("is_admin", false) {
		scope = r.URL.Query().Get("user_id")
	}
	days := queryInt(r, "days", 7)
	writeJSON(w, http.StatusOK, usagestats.Report(days, scope, user.Bool("is_admin", false)), nil)
	return nil
}

// usageRefresh recomputes the rollup now, for the console's Refresh button.
//
// Open to any signed-in user rather than admins only: a regular user reads their own numbers
// from the same file and has the same reason to want them current. The single-flight lease in
// internal/usagestats is what bounds the cost -- a second request while one is running is a no-op.
func (a *App) usageRefresh(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.user(r); err != nil {
		return err
	}
	started, err := usagestats.Refresh(a.Traces, a.Config().UsageRollupDays(usagestats.DefaultDays))
	if err != nil {
		return errorf(http.StatusInternalServerError, "usage rollup failed: %v", err)
	}
	out := usagestats.Status()
	out.Set("started", started)
	writeJSON(w, http.StatusOK, out, nil)
	return nil
}

// -- Copilot AI credits (administrators only) -----------------------------------

// creditsStatus returns the last pool snapshot plus the gate settings and schedule state.
func (a *App) creditsStatus(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, aicredits.Status(a.Config()), nil)
	return nil
}

// creditsRefresh polls GitHub now, regardless of the schedule. It runs inline so the caller gets
// the fresh figures (or the error) back in the same response.
func (a *App) creditsRefresh(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	cfg := a.Config()
	if cfg.GHAdminToken() == "" {
		return errorf(http.StatusBadRequest,
			"configure the GitHub enterprise administrator token under "+
				"Access control -> Key policy first")
	}
	if !aicredits.AcquireLease() {
		return errorf(http.StatusConflict, "a refresh is already running on another worker")
	}
	_, err := aicredits.Refresh(r.Context(), cfg)
	aicredits.ReleaseLease()
	if err != nil {
		return errorf(http.StatusBadGateway, "%s", err.Error())
	}
	writeJSON(w, http.StatusOK, aicredits.Status(cfg), nil)
	return nil
}

// creditsSchedulePreview validates a cron expression and lists its next few firing times, so the
// console can show what a schedule means before it is saved.
func (a *App) creditsSchedulePreview(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	payload, err := readJSONObject(r)
	if err != nil {
		return err
	}
	expr := payload.Str("schedule")
	if problem := cronexpr.Validate(expr); problem != "" {
		writeJSON(w, http.StatusOK, mapOf("valid", false, "error", problem, "next", []any{}), nil)
		return nil
	}
	runs := []any{}
	t := time.Now().UTC()
	for i := 0; i < 5; i++ {
		next, err := cronexpr.NextAfter(expr, t)
		if err != nil {
			writeJSON(w, http.StatusOK, mapOf("valid", false, "error", err.Error(), "next", []any{}), nil)
			return nil
		}
		t = next
		runs = append(runs, json.Number(strconv.FormatFloat(float64(next.UnixNano())/1e9, 'f', -1, 64)))
	}
	writeJSON(w, http.StatusOK, mapOf("valid", true, "error", nil, "next", runs), nil)
	return nil
}

// -- Trace queries ------------------------------------------------------------

// listTraces returns a page of call-trace summaries, newest first, read off disk.
//
// {total, items, offset, limit, truncated} rather than a bare list, because a console that pages
// needs to know how much there is beyond the page it holds. Non-admin users are forcibly
// filtered down to their own records -- an overwrite rather than a rejection, so a client
// passing someone else's user_id simply sees its own.
func (a *App) listTraces(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	userID := query.Get("user_id")
	if !user.Bool("is_admin", false) {
		userID = user.Str("login")
	}
	limit := queryInt(r, "limit", 50)
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	result := a.Traces.Query(
		query.Get("date"), userID, query.Get("trace_id"), query.Get("session_id"),
		queryInt(r, "offset", 0), limit, traces.MaxQueryDates,
	)
	writeJSON(w, http.StatusOK, mapOf(
		"total", json.Number(strconv.Itoa(result.Total)),
		"items", toAny(result.Items),
		"offset", json.Number(strconv.Itoa(result.Offset)),
		"limit", json.Number(strconv.Itoa(result.Limit)),
		"truncated", result.Truncated,
	), nil)
	return nil
}

// deleteTraces is administrators only: delete every trace matching date and/or user.
//
// A criterion is required. An unfiltered DELETE would be a wipe-all -- not what this is for, and
// not undoable.
func (a *App) deleteTraces(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	query := r.URL.Query()
	date, userID := query.Get("date"), query.Get("user_id")
	if date == "" && userID == "" {
		return errorf(http.StatusUnprocessableEntity, "pass date and/or user_id")
	}
	deleted, err := a.Traces.DeleteMany(date, userID)
	if err != nil {
		return errorf(http.StatusUnprocessableEntity, "%v", err)
	}
	writeJSON(w, http.StatusOK, mapOf("deleted", json.Number(strconv.Itoa(deleted))), nil)
	return nil
}

// deleteTrace is administrators only. A regular user cannot erase the record of their own calls --
// that is the point of keeping one.
func (a *App) deleteTrace(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	if !a.Traces.Delete(r.PathValue("trace_id")) {
		return errorf(http.StatusNotFound, "trace not found")
	}
	writeJSON(w, http.StatusOK, mapOf("ok", true, "deleted", json.Number("1")), nil)
	return nil
}

func (a *App) getTrace(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	trace := a.Traces.Get(r.PathValue("trace_id"))
	if trace == nil {
		return errorf(http.StatusNotFound, "trace not found")
	}
	if !user.Bool("is_admin", false) && trace.Str("user_id") != user.Str("login") {
		return errorf(http.StatusNotFound, "trace not found")
	}
	writeJSON(w, http.StatusOK, trace, nil)
	return nil
}

// recentDecisions is the legacy endpoint: routing-decision summaries derived from traces.
func (a *App) recentDecisions(w http.ResponseWriter, r *http.Request) error {
	user, err := a.user(r)
	if err != nil {
		return err
	}
	scope := ""
	if !user.Bool("is_admin", false) {
		scope = user.Str("login")
	}
	items := a.Traces.List(queryInt(r, "limit", 50), "", scope, "")
	// Reversed, so the legacy consumer keeps seeing oldest-first.
	out := make([]any, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		out = append(out, items[i])
	}
	writeJSON(w, http.StatusOK, out, nil)
	return nil
}

// -- Health and release -------------------------------------------------------

func (a *App) healthz(w http.ResponseWriter, r *http.Request) error {
	cfg := a.Config()
	providers := []any{}
	for _, name := range cfg.Providers.Keys() {
		providers = append(providers, name)
	}
	writeJSON(w, http.StatusOK, mapOf(
		"status", "ok",
		"strategy", cfg.Strategy,
		"sticky", cfg.Sticky,
		"providers", providers,
		// The console's header reads the version and the project links from here rather than
		// from a second request: it already polls this endpoint, and a build's identity belongs
		// with the rest of what it reports about itself.
		"version", version.Version,
		"repo_url", version.RepoURL,
		"issues_url", version.IssuesURL,
		"releases_url", version.ReleasesURL,
	), nil)
	return nil
}

// releaseStatus is the last answer from the release check. Public, like /healthz.
//
// Never triggers a check of its own: the console asks on every load, and a page open in ten tabs
// must not become ten calls to github.com.
func (a *App) releaseStatus(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, release.Status(), nil)
	return nil
}

// releaseCheck forces a check now. Administrators only: it makes an outbound request.
func (a *App) releaseCheck(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, release.Check(r.Context()), nil)
	return nil
}
