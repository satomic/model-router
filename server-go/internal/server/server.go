// Package server wires the HTTP surface: the two protocol entry points, the configuration API,
// and the console.
//
// Authentication model:
//   - /v1/chat/completions, /v1/messages -> API key required (Authorization: Bearer mr_...)
//   - /v1/config                         -> administrators only (GitHub OAuth session)
//   - /v1/keys, /v1/usage, /v1/traces    -> signed-in users; non-admins see only their own data
//   - /v1/auth/*, /healthz, the console  -> public (the console itself is served from /, and
//     every non-API path falls through to it)
//   - /v1/release                        -> public (the header shows the version to everyone);
//     forcing a check is administrators only
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/auth"
	"github.com/satomic/model-router/server-go/internal/authstore"
	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/sessions"
	"github.com/satomic/model-router/server-go/internal/traces"
	"github.com/satomic/model-router/server-go/internal/upstream"
)

// App is the running service. `cfg` and `sessions` are replaced wholesale on a configuration
// save, so both are read under cfgMu -- a request that started before the save keeps the view
// it began with rather than seeing a half-applied one.
type App struct {
	cfgMu    sync.RWMutex
	cfg      *config.RouterConfig
	sessions *sessions.Store

	Traces    *traces.Store
	AuthStore *authstore.Store
	Pool      *upstream.Pool

	dist string // the built console, or "" when it was not bundled
}

func New(cfg *config.RouterConfig, traceStore *traces.Store, store *authstore.Store, dist string) *App {
	return &App{
		cfg:       cfg,
		sessions:  sessions.New(cfg.SessionTTL, cfg.MaxSessions),
		Traces:    traceStore,
		AuthStore: store,
		Pool:      upstream.NewPool(),
		dist:      dist,
	}
}

// Config returns the configuration this request should read.
func (a *App) Config() *config.RouterConfig {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *App) Sessions() *sessions.Store {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.sessions
}

func (a *App) setConfig(cfg *config.RouterConfig, replaceSessions bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	a.cfg = cfg
	if replaceSessions {
		a.sessions = sessions.New(cfg.SessionTTL, cfg.MaxSessions)
	}
}

// -- Response helpers ---------------------------------------------------------

// writeJSON mirrors FastAPI's JSONResponse: a compact body, and no HTML escaping so prompts and
// traces read back exactly as they were sent.
func writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("Content-Type", "application/json")
	encoded, err := marshalJSON(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"failed to encode the response"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func marshalJSON(body any) ([]byte, error) {
	var buf strings.Builder
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(buf.String(), "\n")), nil
}

// writeError answers in the {"detail": ...} shape the console already parses.
func writeError(w http.ResponseWriter, err error) {
	var httpErr *auth.HTTPError
	if errors.As(err, &httpErr) {
		writeJSON(w, httpErr.Status, map[string]any{"detail": httpErr.Detail}, nil)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()}, nil)
}

func errorf(status int, format string, args ...any) error {
	return auth.Errorf(status, format, args...)
}

// readJSONObject decodes a request body into an ordered map, so a configuration round-trip
// keeps the key order the console sent.
func readJSONObject(r *http.Request) (*omap.Map, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "could not read the request body")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return omap.New(), nil
	}
	value, err := omap.FromJSON(data)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "request body is not valid JSON")
	}
	body, ok := value.(*omap.Map)
	if !ok {
		return nil, errorf(http.StatusBadRequest, "request body must be a JSON object")
	}
	return body, nil
}

func queryInt(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func queryBool(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// -- Authentication shortcuts -------------------------------------------------

func (a *App) user(r *http.Request) (*omap.Map, error) {
	return auth.RequireUser(r, a.AuthStore, a.Config())
}

func (a *App) admin(r *http.Request) (*omap.Map, error) {
	return auth.RequireAdmin(r, a.AuthStore, a.Config())
}

func (a *App) apiKey(r *http.Request) (*omap.Map, error) {
	return auth.RequireAPIKey(r, a.AuthStore)
}

// isAdminLogin reports whether this login is an administrator, from either identity source.
//
// An API key record carries no admin flag -- it names its owner and nothing else -- so a call
// authenticated by a key has to ask the configuration, which is also what makes an admin list
// edit take effect on the very next request rather than when the key is next recreated.
func (a *App) isAdminLogin(login string) bool {
	cfg := a.Config()
	return cfg.IsAdminLogin(login) || cfg.IsLocalAdminLogin(login)
}

// -- Routing ------------------------------------------------------------------

// handler adapts a function that can fail into an http.HandlerFunc.
type handler func(http.ResponseWriter, *http.Request) error

func wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, err)
		}
	}
}

// Handler builds the full route table.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// The two protocol entry points.
	mux.HandleFunc("POST /v1/chat/completions", wrap(a.chatCompletions))
	mux.HandleFunc("POST /v1/messages", wrap(a.anthropicMessages))
	mux.HandleFunc("GET /v1/models", wrap(a.listModels))

	// Authentication.
	mux.HandleFunc("GET /v1/auth/status", wrap(a.authStatus))
	mux.HandleFunc("POST /v1/auth/local/login", wrap(a.localLogin))
	mux.HandleFunc("POST /v1/auth/local/password", wrap(a.localAdminPassword))
	mux.HandleFunc("POST /v1/auth/local/enabled", wrap(a.setLocalAdminEnabled))
	mux.HandleFunc("POST /v1/auth/setup", wrap(a.authSetup))
	mux.HandleFunc("GET /v1/auth/github/login", wrap(a.githubLogin))
	mux.HandleFunc("GET /v1/auth/github/callback", wrap(a.githubCallback))
	mux.HandleFunc("POST /v1/auth/logout", wrap(a.logout))

	// API key management.
	mux.HandleFunc("GET /v1/keys", wrap(a.listKeys))
	mux.HandleFunc("POST /v1/keys", wrap(a.createKey))
	mux.HandleFunc("PATCH /v1/keys/{key_id}", wrap(a.updateKey))
	mux.HandleFunc("DELETE /v1/keys/{key_id}", wrap(a.deleteKey))

	// Access control.
	mux.HandleFunc("GET /v1/access/me", wrap(a.accessMe))
	mux.HandleFunc("GET /v1/access/token", wrap(a.accessTokenStatus))
	mux.HandleFunc("POST /v1/access/verify-token", wrap(a.accessVerifyToken))
	mux.HandleFunc("GET /v1/access/discover", wrap(a.accessDiscover))
	mux.HandleFunc("GET /v1/access/cache", wrap(a.accessCacheStatus))
	mux.HandleFunc("POST /v1/access/cache/refresh", wrap(a.accessCacheRefresh))
	mux.HandleFunc("GET /v1/access/users", wrap(a.listSignedInUsers))
	mux.HandleFunc("GET /v1/access/topology/members", wrap(a.topologyMembers))
	mux.HandleFunc("GET /v1/access/topology/user", wrap(a.topologyUser))
	mux.HandleFunc("GET /v1/models/available", wrap(a.myAvailableModels))

	// Usage statistics.
	mux.HandleFunc("GET /v1/usage", wrap(a.usage))
	mux.HandleFunc("POST /v1/usage/refresh", wrap(a.usageRefresh))

	// Copilot AI credits.
	mux.HandleFunc("GET /v1/credits", wrap(a.creditsStatus))
	mux.HandleFunc("POST /v1/credits/refresh", wrap(a.creditsRefresh))
	mux.HandleFunc("POST /v1/credits/schedule/preview", wrap(a.creditsSchedulePreview))

	// Configuration.
	mux.HandleFunc("GET /v1/config", wrap(a.getConfig))
	mux.HandleFunc("PUT /v1/config", wrap(a.putConfig))
	mux.HandleFunc("POST /v1/config/decision-prompt/preview", wrap(a.previewDecisionPrompt))
	mux.HandleFunc("GET /v1/config/decision-prompt/default", wrap(a.defaultDecisionPrompt))

	// Trace queries.
	mux.HandleFunc("GET /v1/traces", wrap(a.listTraces))
	mux.HandleFunc("DELETE /v1/traces", wrap(a.deleteTraces))
	mux.HandleFunc("GET /v1/traces/{trace_id}", wrap(a.getTrace))
	mux.HandleFunc("DELETE /v1/traces/{trace_id}", wrap(a.deleteTrace))
	mux.HandleFunc("GET /v1/router/decisions", wrap(a.recentDecisions))

	mux.HandleFunc("GET /healthz", wrap(a.healthz))
	mux.HandleFunc("GET /v1/release", wrap(a.releaseStatus))
	mux.HandleFunc("POST /v1/release/check", wrap(a.releaseCheck))

	// Registered last, so every concrete route above still wins. This is what lets a real URL
	// such as /config/models survive a reload or a paste into somebody else's browser.
	mux.HandleFunc("/", a.serveConsole)

	return withAccessLog(withCORS(mux))
}

// reserved are the prefixes the API owns. An *unknown* path underneath one of these has to keep
// 404ing as JSON -- answering it with the console shell would turn every client typo into a 200
// page of HTML that a fetch() then fails to parse. "ui/" is listed because the old /ui/*
// namespace is deliberately gone, not silently redirected.
var reserved = []string{"v1/", "healthz", "docs", "redoc", "openapi.json", "ui/"}

// withCORS allows the Vite dev server to call the API with the session cookie attached.
func withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{
		"http://localhost:5173": true,
		"http://127.0.0.1:5173": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// The session cookie must travel cross-origin.
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "*")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "*")
				requested := r.Header.Get("Access-Control-Request-Headers")
				if requested == "" {
					requested = "*"
				}
				w.Header().Set("Access-Control-Allow-Headers", requested)
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func logInfo(format string, args ...any) { log.Printf("INFO mr: "+format, args...) }
func logWarn(format string, args ...any) { log.Printf("WARNING mr: "+format, args...) }
