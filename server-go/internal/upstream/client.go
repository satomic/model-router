// Package upstream holds the clients that talk to a backend model, and the pool that reuses
// them across requests.
//
// Three client shapes come out of here -- Azure OpenAI, any OpenAI-compatible service, and
// the Anthropic Messages API. They are pooled together rather than in separate registries
// because the lifecycle is identical: created on first use, dropped on a configuration
// reload.
//
// Hand-rolled on net/http rather than wrapping a vendor SDK: the calls this router needs are
// one POST and one streamed POST per protocol, and the request and response shapes are
// already modelled in internal/wire in exactly the form the rest of the code expects.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/omap"
)

const (
	timeout    = 180 * time.Second
	maxRetries = 1
)

// The one Anthropic host that rejects an Authorization header. Everywhere else -- Databricks,
// Bedrock gateways, LiteLLM, self-hosted proxies -- authenticates with a bearer token, while
// the official API authenticates with x-api-key. Both headers are sent unless the host is the
// official one, because guessing wrong means a 401 the operator cannot diagnose from the UI.
const officialAnthropicHost = "api.anthropic.com"

// Error carries an upstream failure with its status code and the upstream's own message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// sharedTransport is one connection pool for every upstream. Reusing it across clients is
// what makes a reconfigured provider cheap: the TLS handshakes already made to an unchanged
// host stay usable.
var sharedTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   128,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   15 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// Client talks one protocol to one configured connection.
type Client struct {
	provider *config.Provider
	kind     string // "chat" or "responses"
	http     *http.Client
}

func newClient(provider *config.Provider, kind string) (*Client, error) {
	if provider.BaseURL == "" {
		return nil, fmt.Errorf("provider %q has no base_url; set it on the \"Backend connections\" page", provider.Name)
	}
	return &Client{
		provider: provider,
		kind:     kind,
		// No client-level timeout: a streaming response is read for as long as the model
		// keeps generating, and a blanket deadline would cut long answers off mid-sentence.
		// The per-request context carries the deadline that applies.
		http: &http.Client{Transport: sharedTransport},
	}, nil
}

// endpoint assembles the URL this call goes to, which is the only thing that differs between
// an Azure deployment and a plain OpenAI-compatible service.
func (c *Client) endpoint(upstreamModel string) string {
	base := strings.TrimRight(c.provider.BaseURL, "/")
	switch c.provider.APIType {
	case "anthropic":
		return anthropicMessagesURL(base)
	case "azure":
		if c.kind == "responses" {
			// Some models (e.g. o3-pro) only support the Responses API, which lives on the
			// Azure v1 endpoint.
			return base + "/openai/v1/responses"
		}
		return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			base, url.PathEscape(upstreamModel), url.QueryEscape(c.provider.APIVersion))
	default:
		if c.kind == "responses" {
			return base + "/responses"
		}
		return base + "/chat/completions"
	}
}

// anthropicMessagesURL resolves the caller's base URL to the messages endpoint.
//
// Operators paste whatever their provider's documentation shows them, which is sometimes the
// bare host, sometimes a path ending in /v1, and sometimes the full endpoint. All three have
// to work, because a 404 from a mis-joined URL looks exactly like a wrong key from the console.
func anthropicMessagesURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/messages") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("content-type", "application/json")
	switch c.provider.APIType {
	case "anthropic":
		req.Header.Set("accept", "application/json")
		req.Header.Set("anthropic-version", c.provider.APIVersion)
		req.Header.Set("x-api-key", c.provider.APIKey)
		if !strings.Contains(c.endpoint(""), officialAnthropicHost) {
			req.Header.Set("authorization", "Bearer "+c.provider.APIKey)
		}
	case "azure":
		req.Header.Set("api-key", c.provider.APIKey)
		if c.kind == "responses" {
			// The Azure v1 endpoint is reached through the OpenAI-shaped client, which sends
			// a bearer token; the api-key header above is what actually authenticates.
			req.Header.Set("authorization", "Bearer "+c.provider.APIKey)
		}
	default:
		req.Header.Set("authorization", "Bearer "+c.provider.APIKey)
	}
}

func (c *Client) post(ctx context.Context, endpoint string, body any, stream bool) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		c.applyHeaders(req)
		if stream {
			req.Header.Set("accept", "text/event-stream")
		}
		resp, err := c.http.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// Retry only what a retry can fix: a cancelled request is the caller going away.
		if ctx.Err() != nil {
			return nil, explainDNS(err)
		}
	}
	return nil, explainDNS(lastErr)
}

// explainDNS turns a name-resolution failure into an error that points at the actual cause.
//
// Inside a container "no such host" is almost never a mistyped endpoint -- the endpoint is the
// one the operator pasted from their provider's portal, and it resolves fine from the host. It
// is the container's own resolver: a DNS server inherited from the host that the container's
// network namespace cannot reach, a VPN resolver, or an embedded DNS that mishandles a long
// CNAME chain (an Azure OpenAI endpoint has seven of them).
//
// The raw message sends the reader to check the URL, which is the one thing that is not wrong.
// This says where to look instead, because the router cannot tell which of those it is and the
// operator can find out in one command.
func explainDNS(err error) error {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return err
	}
	return fmt.Errorf("%w -- the address could not be resolved from inside this container, "+
		"which usually means the container's DNS rather than a wrong endpoint. Check it with "+
		"`docker exec <container> nslookup %s`; if that fails while the same name resolves on "+
		"the host, pass a resolver the container can reach (`docker run --dns 1.1.1.1 ...`)",
		err, dnsErr.Name)
}

// readError pulls the upstream's message out of its error envelope, falling back to the raw
// body -- the operator has to be able to tell a wrong key from a wrong deployment name.
func readError(status int, body []byte) *Error {
	text := string(body)
	value, err := omap.FromJSON(body)
	if err == nil {
		if data, ok := value.(*omap.Map); ok {
			if inner := data.Map("error"); inner != nil && inner.Str("message") != "" {
				return &Error{Status: status, Message: fmt.Sprintf("%d: %s", status, inner.Str("message"))}
			}
			if data.Str("message") != "" {
				return &Error{Status: status, Message: fmt.Sprintf("%d: %s", status, data.Str("message"))}
			}
		}
	}
	if len(text) > 400 {
		text = text[:400]
	}
	return &Error{Status: status, Message: fmt.Sprintf("%d: %s", status, text)}
}

// Create performs a non-streaming call and returns the decoded body.
func (c *Client) Create(ctx context.Context, endpointModel string, body any) (*omap.Map, error) {
	resp, err := c.post(ctx, c.endpoint(endpointModel), body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, readError(resp.StatusCode, payload)
	}
	value, err := omap.FromJSON(payload)
	if err != nil {
		return nil, err
	}
	data, ok := value.(*omap.Map)
	if !ok {
		return nil, errors.New("upstream returned a non-object response")
	}
	return data, nil
}

// Stream is an open SSE response. Next yields one decoded event at a time; the caller must
// Close it.
type Stream struct {
	resp    *http.Response
	scanner *bufio.Scanner
}

// OpenStream performs a streaming call. The response status is checked here rather than on
// the first read, so an upstream rejection (a bad key, an unknown deployment) becomes a 502
// with a message instead of a 200 whose body dies on its first chunk.
func (c *Client) OpenStream(ctx context.Context, endpointModel string, body any) (*Stream, error) {
	resp, err := c.post(ctx, c.endpoint(endpointModel), body, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, readError(resp.StatusCode, payload)
	}
	scanner := bufio.NewScanner(resp.Body)
	// An SSE frame carrying a whole tool-call argument blob is routinely far larger than the
	// 64KB bufio default, and a scanner that overflows would truncate the answer silently.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &Stream{resp: resp, scanner: scanner}, nil
}

// NextEvent returns the next SSE payload as (event name, data, ok). The event name comes from
// the payload's own `type` field when the stream carries one (Anthropic), else from the
// `event:` line.
func (s *Stream) NextEvent() (string, *omap.Map, bool, error) {
	eventName := ""
	for s.scanner.Scan() {
		line := strings.TrimRight(s.scanner.Text(), "\r")
		if line == "" {
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // an SSE comment / keep-alive
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(line[6:])
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		value, err := omap.FromJSON([]byte(payload))
		if err != nil {
			continue
		}
		data, ok := value.(*omap.Map)
		if !ok {
			continue
		}
		name := data.Str("type")
		if name == "" {
			name = eventName
		}
		return name, data, true, nil
	}
	if err := s.scanner.Err(); err != nil {
		return "", nil, false, err
	}
	return "", nil, false, nil
}

func (s *Stream) Close() {
	if s.resp != nil {
		s.resp.Body.Close()
	}
}

// Pool reuses clients per connection, keyed by everything that changes how a call is made.
type Pool struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

func NewPool() *Pool {
	return &Pool{clients: map[string]*Client{}}
}

// Get returns (or creates) the client for this provider. kind is one of {chat, responses}.
//
// An Anthropic connection ignores kind: the Messages API is one endpoint, and there is no
// Responses-API equivalent to separate out.
func (p *Pool) Get(provider *config.Provider, kind string) (*Client, error) {
	key := provider.CacheKey() + "\x00" + kind
	p.mu.RLock()
	client, ok := p.clients[key]
	p.mu.RUnlock()
	if ok {
		return client, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if client, ok := p.clients[key]; ok {
		return client, nil
	}
	client, err := newClient(provider, kind)
	if err != nil {
		return nil, err
	}
	p.clients[key] = client
	return client, nil
}

// Invalidate drops every cached client after a configuration change. The shared transport's
// idle connections stay: a provider whose endpoint did not change reuses them, and one whose
// endpoint did will simply never dial the old host again.
func (p *Pool) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients = map[string]*Client{}
}
