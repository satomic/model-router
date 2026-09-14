package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// request builds one arriving from `peer` with forwarded headers a client could have set itself.
func request(peer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://router.internal/v1/auth/status", nil)
	r.RemoteAddr = peer
	r.Host = "router.internal"
	r.Header.Set("x-forwarded-proto", "https")
	r.Header.Set("x-forwarded-host", "evil.example.com")
	return r
}

func restoreDefaults(t *testing.T) {
	t.Helper()
	saved, savedAll := trustedProxies, trustAll
	t.Cleanup(func() { trustedProxies, trustAll = saved, savedAll })
	trustedProxies = map[string]bool{"127.0.0.1": true, "::1": true}
	trustAll = false
}

// TestForwardedHeadersAreIgnoredFromAnUntrustedPeer is the security property: these headers are
// client-supplied, and honouring them from anyone lets a caller dictate the OAuth callback
// origin and strip the Secure flag off a session cookie.
func TestForwardedHeadersAreIgnoredFromAnUntrustedPeer(t *testing.T) {
	restoreDefaults(t)
	r := request("203.0.113.9:44321")

	if got := origin(r); got != "http://router.internal" {
		t.Errorf("origin = %q, want the real Host -- a stranger set the forwarded one", got)
	}
	if isSecure(r) {
		t.Error("an untrusted caller was able to claim the connection was HTTPS")
	}
	if got := CallbackURL(r, ""); got != "http://router.internal"+CallbackPath {
		t.Errorf("callback = %q, want it built from the real Host", got)
	}
}

// TestForwardedHeadersAreHonouredFromTheProxy: the feature still has to work, or every
// deployment behind TLS termination builds the wrong callback URL.
func TestForwardedHeadersAreHonouredFromTheProxy(t *testing.T) {
	restoreDefaults(t)
	SetTrustedProxies([]string{"10.0.0.2"})
	r := request("10.0.0.2:9999")

	if got := origin(r); got != "https://evil.example.com" {
		t.Errorf("origin = %q, want the value the trusted proxy supplied", got)
	}
	if !isSecure(r) {
		t.Error("the trusted proxy said https and the cookie was not marked Secure")
	}
	// And a peer that is not the configured proxy is still refused.
	if got := origin(request("203.0.113.9:1")); got != "http://router.internal" {
		t.Errorf("naming a proxy also trusted everyone else: %q", got)
	}
}

// TestLoopbackIsTrustedByDefault mirrors uvicorn's default, so a deployment that passes no flag
// behaves the way the Python backend does.
func TestLoopbackIsTrustedByDefault(t *testing.T) {
	restoreDefaults(t)
	if got := origin(request("127.0.0.1:5050")); got != "https://evil.example.com" {
		t.Errorf("origin = %q, want loopback to be trusted by default", got)
	}
	if got := TrustedProxyDescription(); got != "127.0.0.1,::1" {
		t.Errorf("description = %q, want the loopback default", got)
	}
}

// TestTrustAllIsOptIn: some topologies need it (a rotating ingress, a mesh sidecar), but it is
// the thing the documentation warns against, so it must never be the default.
func TestTrustAllIsOptIn(t *testing.T) {
	restoreDefaults(t)
	SetTrustedProxies([]string{"*"})
	if got := origin(request("203.0.113.9:1")); got != "https://evil.example.com" {
		t.Errorf("origin = %q, want '*' to trust every peer", got)
	}
	if got := TrustedProxyDescription(); got != "*" {
		t.Errorf("description = %q, want *", got)
	}
}

// TestEmptyConfigurationKeepsTheDefault: an unset flag must not widen trust to nobody-or-everyone
// by accident.
func TestEmptyConfigurationKeepsTheDefault(t *testing.T) {
	restoreDefaults(t)
	SetTrustedProxies([]string{"", "  "})
	if got := TrustedProxyDescription(); got != "127.0.0.1,::1" {
		t.Errorf("description = %q, want the loopback default untouched", got)
	}
}
