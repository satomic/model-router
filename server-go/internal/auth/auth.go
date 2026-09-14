// Package auth holds GitHub OAuth sign-in, session cookies and API key authentication.
//
// Cookie sessions + an admin allow-list + role separation. The callback URL and the cookie
// Secure flag follow X-Forwarded-Proto / X-Forwarded-Host so this works behind a reverse proxy.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/satomic/model-router/server-go/internal/authstore"
	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/localadmin"
	"github.com/satomic/model-router/server-go/internal/omap"
)

const (
	SessionCookie = "mr_session"
	StateCookie   = "mr_oauth_state"
	CallbackPath  = "/v1/auth/github/callback"

	GitHubAuthorize = "https://github.com/login/oauth/authorize"
	GitHubToken     = "https://github.com/login/oauth/access_token"
	GitHubUser      = "https://api.github.com/user"
)

var loopback = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}

// HTTPError carries a status and a message, mirroring the {"detail": ...} bodies the console
// already parses.
type HTTPError struct {
	Status int
	Detail any
}

func (e *HTTPError) Error() string { return fmt.Sprint(e.Detail) }

func Errorf(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Detail: fmt.Sprintf(format, args...)}
}

// IsLoopback reports whether the request came from the local machine.
func IsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return loopback[host]
}

// -- Trusting a reverse proxy --------------------------------------------------
//
// X-Forwarded-Proto and X-Forwarded-Host decide the OAuth callback origin and whether the
// session cookie is marked Secure, and they are **client-supplied**. Honouring them from any
// caller lets that caller dictate the callback origin, and lets one strip the Secure flag from a
// cookie that is about to cross a plain-HTTP hop.
//
// So they are honoured only when the *direct peer* -- the address the connection actually came
// from, which a client cannot forge -- is a proxy this deployment was told to trust. That is the
// same rule uvicorn applies for the Python backend, down to the default: loopback only, with the
// operator naming their proxy explicitly.
var trustedProxies = map[string]bool{"127.0.0.1": true, "::1": true}

// trustAll is the escape hatch for a deployment whose proxy address is not knowable in advance
// (a rotating ingress, a mesh sidecar). It is not the default, deliberately.
var trustAll bool

// SetTrustedProxies configures which peers may set X-Forwarded-*. A "*" entry trusts everyone,
// which the documentation warns against but some topologies need.
//
// An empty list leaves the loopback default in place, so a deployment that says nothing behaves
// the way the Python backend does when its operator passes no flags.
func SetTrustedProxies(addresses []string) {
	cleaned := map[string]bool{}
	all := false
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		switch address {
		case "":
			continue
		case "*":
			all = true
		default:
			cleaned[address] = true
		}
	}
	trustAll = all
	if len(cleaned) > 0 {
		trustedProxies = cleaned
	}
}

// TrustedProxyDescription is what the startup line reports, so the setting is visible in the log
// rather than being something an operator has to infer from behaviour.
func TrustedProxyDescription() string {
	if trustAll {
		return "*"
	}
	names := make([]string, 0, len(trustedProxies))
	for address := range trustedProxies {
		names = append(names, address)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// fromTrustedProxy reports whether this connection's peer may set the forwarded headers.
func fromTrustedProxy(r *http.Request) bool {
	if trustAll {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return trustedProxies[host]
}

func forwardedScheme(r *http.Request) string {
	if fromTrustedProxy(r) {
		if proto := r.Header.Get("x-forwarded-proto"); proto != "" {
			return strings.TrimSpace(strings.Split(proto, ",")[0])
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func origin(r *http.Request) string {
	host := ""
	if fromTrustedProxy(r) {
		host = r.Header.Get("x-forwarded-host")
	}
	if host == "" {
		host = r.Host
	}
	return forwardedScheme(r) + "://" + strings.TrimSpace(strings.Split(host, ",")[0])
}

func CallbackURL(r *http.Request, configured string) string {
	if trimmed := strings.TrimSpace(configured); trimmed != "" {
		return trimmed
	}
	return origin(r) + CallbackPath
}

func isSecure(r *http.Request) bool { return forwardedScheme(r) == "https" }

func SetSessionCookie(w http.ResponseWriter, r *http.Request, sid string, ttl int) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: sid, MaxAge: ttl,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r), Path: "/",
	})
}

func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r), Path: "/",
	})
}

func SetStateCookie(w http.ResponseWriter, r *http.Request, state string) {
	http.SetCookie(w, &http.Cookie{
		Name: StateCookie, Value: state, MaxAge: 600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r), Path: "/",
	})
}

func ClearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: StateCookie, Value: "", MaxAge: -1, Path: "/"})
}

func Cookie(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// -- OAuth flow ---------------------------------------------------------------

// BuildAuthorizeURL returns (authorize URL, one-time state). The caller must store the state in
// a cookie to guard against CSRF.
func BuildAuthorizeURL(r *http.Request, cfg *config.RouterConfig) (string, string) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// A failing CSPRNG cannot be worked around: there is no safe token to hand back, and
		// net/http contains the panic to this one connection.
		panic(err)
	}
	state := base64.RawURLEncoding.EncodeToString(buf)
	redirectURI := CallbackURL(r, cfg.GHCallbackURL)
	if cfg.GHCallbackURL != "" && !strings.HasPrefix(cfg.GHCallbackURL, origin(r)) {
		log.Printf("WARNING mr: configured callback_url (%s) does not match the current origin (%s); "+
			"the session cookie may not take effect after sign-in", cfg.GHCallbackURL, origin(r))
	}
	query := url.Values{
		"client_id":    {cfg.GHClientID},
		"redirect_uri": {redirectURI},
		"scope":        {"read:user"},
		"state":        {state},
	}
	return GitHubAuthorize + "?" + query.Encode(), state
}

var oauthClient = &http.Client{Timeout: 15 * time.Second}

// ExchangeCodeForUser exchanges the code for a token, fetches the user, and returns the value
// to store in the session.
func ExchangeCodeForUser(ctx context.Context, r *http.Request, cfg *config.RouterConfig, code string) (*omap.Map, error) {
	form := url.Values{
		"client_id":     {cfg.GHClientID},
		"client_secret": {cfg.GHClientSecret},
		"code":          {code},
		"redirect_uri":  {CallbackURL(r, cfg.GHCallbackURL)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GitHubToken, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthClient.Do(req)
	if err != nil {
		return nil, Errorf(http.StatusBadGateway, "GitHub token exchange failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, Errorf(http.StatusBadGateway, "GitHub token exchange failed: %d", resp.StatusCode)
	}
	value, err := omap.FromJSON(body)
	if err != nil {
		return nil, Errorf(http.StatusBadGateway, "GitHub returned a non-JSON token response")
	}
	payload, _ := value.(*omap.Map)
	if payload == nil {
		payload = omap.New()
	}
	accessToken := payload.Str("access_token")
	if accessToken == "" {
		detail := payload.Str("error_description")
		if detail == "" {
			encoded, _ := json.Marshal(payload)
			detail = string(encoded)
		}
		return nil, Errorf(http.StatusBadRequest, "GitHub returned no access_token: %s", detail)
	}

	userReq, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubUser, nil)
	if err != nil {
		return nil, err
	}
	userReq.Header.Set("Authorization", "Bearer "+accessToken)
	userReq.Header.Set("Accept", "application/vnd.github+json")
	userResp, err := oauthClient.Do(userReq)
	if err != nil {
		return nil, Errorf(http.StatusBadGateway, "GitHub user lookup failed: %v", err)
	}
	userBody, _ := io.ReadAll(userResp.Body)
	userResp.Body.Close()
	if userResp.StatusCode >= 400 {
		return nil, Errorf(http.StatusBadGateway, "GitHub user lookup failed: %d", userResp.StatusCode)
	}
	userValue, err := omap.FromJSON(userBody)
	if err != nil {
		return nil, Errorf(http.StatusBadGateway, "GitHub returned a non-JSON user response")
	}
	ghUser, _ := userValue.(*omap.Map)
	if ghUser == nil {
		ghUser = omap.New()
	}

	login := ghUser.Str("login")
	if login == "" {
		return nil, Errorf(http.StatusBadRequest, "GitHub user info has no login")
	}
	isAdmin := cfg.IsAdminLogin(login)
	if !isAdmin && !cfg.AllowAnyGitHubUser {
		return nil, Errorf(http.StatusForbidden,
			"only administrators may sign in right now (auth.allow_any_github_user = false)")
	}
	name := ghUser.Str("name")
	if name == "" {
		name = login
	}
	user := omap.New()
	user.Set("login", login)
	user.Set("name", name)
	user.Set("avatar_url", ghUser.Value("avatar_url"))
	user.Set("is_admin", isAdmin)
	return user, nil
}

// -- Session and roles --------------------------------------------------------

// PasswordChangeRequired is the machine-readable detail the console routes on, so it can show
// the change-password form instead of a generic error.
//
// Reachable while the local administrator's default password is still in force: everything else
// is refused, because a super-admin account on a documented credential must not be usable until
// it has been changed.
const PasswordChangeRequired = "password_change_required"

var passwordChangeExempt = map[string]bool{
	"/v1/auth/status":         true,
	"/v1/auth/logout":         true,
	"/v1/auth/local/password": true,
}

// CurrentUser resolves the session cookie into the signed-in user, or nil.
func CurrentUser(r *http.Request, store *authstore.Store, cfg *config.RouterConfig) *omap.Map {
	session := store.GetSession(Cookie(r, SessionCookie))
	if session == nil {
		return nil
	}
	if session.Bool("local_admin", false) {
		// A local administrator's authority comes from auth.local_admin, not from admin_logins --
		// resolving it through IsAdminLogin would silently demote the account one request after
		// sign-in. Recomputed for the same reason as the GitHub branch below: renaming or
		// disabling the account in config.yaml must downgrade sessions already issued to it.
		session.Set("is_admin", cfg.IsLocalAdminLogin(session.Str("login")))
		session.Set("must_change_password", localadmin.MustChange(cfg.LocalAdminSettings()))
	} else {
		// The admin list may change while a session is still valid, so recompute from the
		// configuration on every request.
		session.Set("is_admin", cfg.IsAdminLogin(session.Str("login")))
		session.Set("must_change_password", false)
	}
	return session
}

func RequireUser(r *http.Request, store *authstore.Store, cfg *config.RouterConfig) (*omap.Map, error) {
	user := CurrentUser(r, store, cfg)
	if user == nil {
		return nil, Errorf(http.StatusUnauthorized, "not signed in")
	}
	if user.Bool("must_change_password", false) && !passwordChangeExempt[r.URL.Path] {
		return nil, &HTTPError{Status: http.StatusForbidden, Detail: PasswordChangeRequired}
	}
	return user, nil
}

func RequireAdmin(r *http.Request, store *authstore.Store, cfg *config.RouterConfig) (*omap.Map, error) {
	user, err := RequireUser(r, store, cfg)
	if err != nil {
		return nil, err
	}
	if !user.Bool("is_admin", false) {
		return nil, Errorf(http.StatusForbidden, "administrator privileges required")
	}
	return user, nil
}

// -- API keys -----------------------------------------------------------------

// ExtractAPIKey accepts both `Authorization: Bearer <key>` and `api-key` / `x-api-key`, because
// the two client ecosystems send different headers.
func ExtractAPIKey(r *http.Request) string {
	authHeader := r.Header.Get("authorization")
	if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return strings.TrimSpace(authHeader[7:])
	}
	if key := strings.TrimSpace(r.Header.Get("api-key")); key != "" {
		return key
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func RequireAPIKey(r *http.Request, store *authstore.Store) (*omap.Map, error) {
	plaintext := ExtractAPIKey(r)
	if plaintext == "" {
		return nil, Errorf(http.StatusUnauthorized,
			"missing API key: send Authorization: Bearer <key> "+
				"(create one on the \"API keys\" page of the console)")
	}
	record := store.LookupAPIKey(plaintext)
	if record == nil {
		return nil, Errorf(http.StatusUnauthorized, "API key is invalid or has been revoked")
	}
	store.TouchAPIKey(record.Str("id"))
	return record, nil
}
