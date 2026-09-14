package upstream

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

// TestExplainDNSPointsAtTheContainer: the raw "no such host" sends an operator to check the
// endpoint, which is the one thing that is not wrong. The wrapped message has to name the real
// suspect and keep the original error intact for anything that inspects it.
func TestExplainDNSPointsAtTheContainer(t *testing.T) {
	// The shape a failed http.Client.Do actually produces: url.Error wrapping net.OpError
	// wrapping net.DNSError.
	dnsErr := &net.DNSError{Err: "no such host", Name: "example.openai.azure.com", IsNotFound: true}
	wrapped := &url.Error{
		Op:  "Post",
		URL: "https://example.openai.azure.com/openai/deployments/gpt-4o/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: dnsErr},
	}

	got := explainDNS(wrapped)
	text := got.Error()

	if !strings.Contains(text, "example.openai.azure.com") {
		t.Errorf("the message should name the host that failed:\n%s", text)
	}
	for _, want := range []string{"container", "nslookup", "--dns"} {
		if !strings.Contains(text, want) {
			t.Errorf("the message should mention %q so the reader knows where to look:\n%s", want, text)
		}
	}
	// Unwrappable, so a caller that wants to branch on the cause still can.
	var found *net.DNSError
	if !errors.As(got, &found) {
		t.Error("wrapping lost the underlying net.DNSError")
	}
}

// TestExplainDNSLeavesOtherErrorsAlone: a refused connection, a TLS failure or a timeout are
// their own diagnoses and must not be dressed up as a DNS problem.
func TestExplainDNSLeavesOtherErrorsAlone(t *testing.T) {
	for _, err := range []error{
		errors.New("connection refused"),
		fmt.Errorf("tls: handshake failure"),
		&url.Error{Op: "Post", URL: "https://x", Err: errors.New("context deadline exceeded")},
	} {
		if got := explainDNS(err); got.Error() != err.Error() {
			t.Errorf("explainDNS rewrote a non-DNS error:\n  in  %v\n  out %v", err, got)
		}
	}
}

// TestAnthropicMessagesURL: operators paste whatever their provider's documentation shows, and
// a mis-joined URL 404s in a way that looks exactly like a wrong key from the console.
func TestAnthropicMessagesURL(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com":                "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/":               "https://api.anthropic.com/v1/messages",
		"https://host/serving-endpoints/anthropic": "https://host/serving-endpoints/anthropic/v1/messages",
		"https://host/v1":                          "https://host/v1/messages",
		"https://host/v1/messages":                 "https://host/v1/messages",
	}
	for in, want := range cases {
		if got := anthropicMessagesURL(in); got != want {
			t.Errorf("anthropicMessagesURL(%q) = %q, want %q", in, got, want)
		}
	}
}
