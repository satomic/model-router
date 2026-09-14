package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/satomic/model-router/server-go/internal/aicredits"
	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/keyscope"
	"github.com/satomic/model-router/server-go/internal/modelpolicy"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/routing"
	"github.com/satomic/model-router/server-go/internal/upstream"
	"github.com/satomic/model-router/server-go/internal/wire"
)

// passthroughFields are the request fields passed straight through to the backend model.
var passthroughFields = []string{
	"messages", "temperature", "top_p", "max_tokens", "max_completion_tokens",
	"stop", "n", "presence_penalty", "frequency_penalty", "tools", "tool_choice",
	"response_format", "seed", "user", "stream",
}

// Sensitive request headers are never written to a trace.
var redactedHeaders = map[string]bool{
	"authorization": true, "api-key": true, "cookie": true,
	"proxy-authorization": true, "x-api-key": true,
}

// interactionHeaders is the header GitHub Copilot puts on every request belonging to one user
// interaction: the initial question and each follow-up of its tool-call loop all carry the same
// value, while x-request-id differs per HTTP call. Read in order, so a client that sets a
// plainer name still gets grouped.
var interactionHeaders = []string{"x-interaction-id", "x-conversation-id", "x-copilot-interaction-id"}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

func round1(v float64) json.Number { return json.Number(fmt.Sprintf("%.1f", v)) }

func sanitizedHeaders(r *http.Request) *omap.Map {
	out := omap.New()
	for name, values := range r.Header {
		lowered := strings.ToLower(name)
		if redactedHeaders[lowered] {
			out.Set(lowered, "<redacted>")
			continue
		}
		out.Set(lowered, strings.Join(values, ", "))
	}
	return out
}

func interactionID(r *http.Request) any {
	for _, name := range interactionHeaders {
		value := strings.TrimSpace(r.Header.Get(name))
		if value != "" {
			if len(value) > 128 {
				value = value[:128]
			}
			return value
		}
	}
	return nil
}

func headerOrNil(r *http.Request, name string) any {
	if value := r.Header.Get(name); value != "" {
		return value
	}
	return nil
}

func clientIP(r *http.Request) any {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return nil
	}
	return host
}

// callContext is everything serve needs after routing has happened.
type callContext struct {
	Gated          bool
	Message        string
	Model          string
	Resolved       *config.ResolvedModel
	Payload        *omap.Map
	Trace          *omap.Map
	Headers        map[string]string
	Start          time.Time
	ClientProtocol string
	Cfg            *config.RouterConfig
}

// -- The two protocol entry points --------------------------------------------
// A caller may speak either protocol and an upstream connection may speak either protocol, and
// the two choices are independent: an Anthropic client can be answered by an Azure deployment
// and an OpenAI client by a Claude endpoint. Rather than four pipelines, both entry points
// convert to one canonical form -- an OpenAI chat-completions payload -- which is what routing,
// the model policy and the trace record all already read. See internal/wire.

func (a *App) chatCompletions(w http.ResponseWriter, r *http.Request) error {
	key, err := a.apiKey(r)
	if err != nil {
		return err
	}
	start := time.Now()
	body, err := readJSONObject(r)
	if err != nil {
		return err
	}
	if len(body.Slice("messages")) == 0 {
		return errorf(http.StatusBadRequest, "messages is required")
	}
	ctx, err := a.prepareCall(r, body, key, start, "openai")
	if err != nil {
		return err
	}
	if ctx.Gated {
		return a.serveGated(w, r, ctx)
	}
	return a.serve(w, r, ctx)
}

// anthropicMessages is the Anthropic Messages entry point, so a client configured with
// ANTHROPIC_BASE_URL pointing at this router works unchanged.
//
// Authenticated by the same mr_ key as /v1/chat/completions, accepted from either `x-api-key`
// or `Authorization: Bearer` because the two ecosystems send different headers.
func (a *App) anthropicMessages(w http.ResponseWriter, r *http.Request) error {
	key, err := a.apiKey(r)
	if err != nil {
		return err
	}
	start := time.Now()
	raw, err := readJSONObject(r)
	if err != nil {
		return err
	}
	if len(raw.Slice("messages")) == 0 {
		return errorf(http.StatusBadRequest, "messages is required")
	}
	body := wire.AnthropicRequestToOpenAI(raw)
	ctx, err := a.prepareCall(r, body, key, start, "anthropic")
	if err != nil {
		return err
	}
	if ctx.Gated {
		return a.serveGated(w, r, ctx)
	}
	return a.serve(w, r, ctx)
}

// prepareCall is everything both entry points do before an upstream is touched: resolve what
// the caller may use, route, build the trace, build the response headers.
//
// Split out rather than inlined twice because every line of it -- the policy, the key scope,
// stickiness, the trace shape -- must behave identically whichever protocol the caller spoke,
// and two copies would drift.
func (a *App) prepareCall(r *http.Request, body, key *omap.Map, start time.Time, clientProtocol string) (*callContext, error) {
	cfg := a.Config()
	// user_id comes from the API key's owner: Copilot BYOK never sends x-user-id, so it used to
	// be permanently null.
	userID := key.Str("user_login")
	messages := body.Slice("messages")
	prompt := routing.ExtractUserPrompt(messages)
	interaction := interactionID(r)
	sessionID := strings.TrimSpace(r.Header.Get("x-session-id"))

	// The AI-credit gate comes before everything else on purpose: while the caller's Copilot
	// pool still has credits the request is answered with a note and is neither routed nor shown
	// to the decision model, so it costs nothing on any upstream. Administrators are gated too --
	// the point is the customer's budget, not a privilege boundary.
	if gated := aicredits.Gate(cfg, userID); gated != nil {
		return a.gatedContext(r, body, key, sessionID, start, clientProtocol, gated, prompt, interaction), nil
	}

	// The caller's effective model set, resolved before anything routes. An empty list is a
	// configured outcome rather than an error -- an operator can bind a scope to an empty group,
	// which is how "this user gets nothing yet" is expressed -- so it is refused here with the
	// reason, instead of being handed to a router that has no model to pick.
	allowed := modelpolicy.AllowedModels(r.Context(), cfg, userID, a.isAdminLogin(userID))
	if allowed != nil && len(allowed) == 0 {
		return nil, errorf(http.StatusForbidden,
			"no models are available to you under the current model policy; "+
				"ask an administrator to assign a model group")
	}
	// Then narrowed again by this key's own scope. Reported separately from the policy refusal
	// above, because the two have different owners: the first needs an administrator, the second
	// the user can fix themselves on the API keys page.
	scope := key.Map("scope")
	allowed = keyscope.Narrow(cfg, allowed, scope)
	if allowed != nil && len(allowed) == 0 {
		return nil, errorf(http.StatusForbidden,
			"this API key is scoped to models you cannot currently use; "+
				"widen its scope on the API keys page (scope: %s)", keyscope.Describe(scope))
	}

	model, reason, decisionMS, analysis := a.decideModel(r.Context(), cfg, prompt, sessionID, interaction, allowed)
	resolved := cfg.ResolveModel(model)

	payload := omap.New()
	for _, field := range passthroughFields {
		if body.Has(field) {
			payload.Set(field, body.Value(field))
		}
	}
	if resolved.Reasoning {
		// Reasoning models: max_tokens -> max_completion_tokens, and no sampling params.
		if payload.Has("max_tokens") {
			payload.Set("max_completion_tokens", payload.Value("max_tokens"))
			payload.Delete("max_tokens")
		}
		for _, param := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
			payload.Delete(param)
		}
	}

	params := omap.New()
	for _, field := range body.Keys() {
		if field != "messages" {
			params.Set(field, body.Value(field))
		}
	}
	sentParams := omap.New()
	for _, field := range payload.Keys() {
		if field != "messages" {
			sentParams.Set(field, payload.Value(field))
		}
	}

	trace := mapOf(
		"id", wire.NewID("")[:8],
		"ts", time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00"),
		"user_id", userID,
		"api_key_id", key.Value("id"),
		"api_key_name", key.Value("name"),
		"api_key_scope", keyscope.Describe(scope),
		"session_id", nilIfBlank(sessionID),
		// What makes this request part of a user interaction rather than an isolated call.
		// nil for a client that sends no such header, in which case every request is its own
		// interaction.
		"interaction_id", interaction,
		"request_id", headerOrNil(r, "x-request-id"),
		// Copilot marks a request the user triggered as "user" and the follow-ups of its tool
		// loop as "agent"; recorded per turn so the chain shows which is which.
		"initiator", headerOrNil(r, "x-initiator"),
		"client_ip", clientIP(r),
		// Which protocol the caller spoke. Worth recording even though the stored request is
		// always canonical: "the answer came back in a shape my client could not read" is
		// otherwise impossible to diagnose from a trace.
		"client_protocol", clientProtocol,
		"strategy", cfg.Strategy,
		"sticky", cfg.Sticky,
		"prompt_preview", headRunes(prompt, 120),
		"request", mapOf(
			"headers", sanitizedHeaders(r),
			"messages", messages,
			"params", params,
			"stream", body.Bool("stream", false),
		),
		"routing", mapOf(
			"model", model,
			"reason", reason,
			"decision_ms", round1(decisionMS),
			"analysis", analysis,
		),
		"backend", mapOf(
			"deployment", resolved.UpstreamModel,
			"api", resolved.API,
			"protocol", resolved.Provider.Protocol(),
			"provider", resolved.Provider.Name,
			"base_url", resolved.Provider.BaseURL,
			"api_type", resolved.Provider.APIType,
			"sent_params", sentParams,
		),
		"response", nil,
		"status", "pending",
		"total_ms", nil,
	)
	// Resolved now rather than at write time, so x-trace-id names the interaction record this
	// turn joins -- a per-request id would 404 on GET /v1/traces/<id>.
	a.Traces.ResolveInteraction(trace)
	logInfo("route id=%s user=%s session=%s interaction=%v model=%s provider=%s protocol=%s->%s reason=%s decision_ms=%.1f",
		trace.Str("id"), userID, sessionID, interaction, model, resolved.Provider.Name,
		clientProtocol, resolved.Provider.Protocol(), reason, decisionMS)

	headers := map[string]string{
		"x-trace-id":           trace.Str("id"),
		"x-routed-model":       model,
		"x-router-reason":      reason,
		"x-router-decision-ms": fmt.Sprintf("%.1f", decisionMS),
	}
	if interaction != nil {
		headers["x-router-interaction-id"] = omap.AsString(interaction)
	}

	return &callContext{
		Model:          model,
		Resolved:       resolved,
		Payload:        payload,
		Trace:          trace,
		Headers:        headers,
		Start:          start,
		ClientProtocol: clientProtocol,
		Cfg:            cfg,
	}, nil
}

func nilIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func headRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// decideModel returns (model, reason, decision ms, analysis), including session-sticky logic.
//
// Two binding keys, checked in that order. `interaction` is the tighter one: it holds the model
// constant across the tool-call loop of a single user question, which is what stops one question
// being routed N times. `sessionID` is the broader, opt-in one: it holds a model across a whole
// conversation.
//
// `allowed` is the caller's effective model set from the model policy, or nil when they are
// unrestricted. It narrows the catalog every strategy sees (see RouterConfig.RestrictedTo), so a
// model the caller may not use is unreachable through rules, through the decision model, and
// through the default-model substitution alike -- rather than being caught by a check that each
// of those three paths would have to remember to make.
func (a *App) decideModel(ctx context.Context, cfg *config.RouterConfig, prompt, sessionID string,
	interaction any, allowed []string) (string, string, float64, *omap.Map) {
	start := time.Now()
	elapsed := func() float64 { return float64(time.Since(start).Microseconds()) / 1000 }

	// The narrowed view is what every branch below reads. `cfg` itself is untouched: it is
	// shared by every concurrent request, so a per-caller restriction may never be written into it.
	view := cfg
	if allowed != nil {
		view = cfg.RestrictedTo(allowed)
	}

	if cfg.Sticky {
		bindings := []struct{ key, kind string }{
			{omap.AsString(interaction), "interaction"},
			{sessionID, "session"},
		}
		for _, binding := range bindings {
			if binding.key == "" {
				continue
			}
			cached := a.Sessions().Get(bindKey(binding.kind, binding.key))
			if cached == "" {
				continue
			}
			if !view.Models.Has(cached) {
				// The binding predates a policy change, or two callers with different effective
				// sets share a session id. Either way a stale binding must not resurrect a model
				// this caller may no longer use, so the decision is made again.
				logInfo("sticky %s binding to %s dropped: not in the caller's effective model set",
					binding.kind, cached)
				continue
			}
			analysis := mapOf(
				// The console renders both kinds the same way -- a note saying the decision was
				// skipped -- so they share the analysis type.
				"type", "session",
				"note", fmt.Sprintf("%s %s is already bound to model %s, skipping the routing decision",
					binding.kind, binding.key, cached),
				"bound_by", binding.kind,
			)
			reason := "session-sticky"
			if binding.kind == "interaction" {
				reason = "interaction-sticky"
			}
			return cached, reason, elapsed(), analysis
		}
	}

	var model, reason string
	var analysis *omap.Map
	switch cfg.Strategy {
	case "ai":
		model, reason, analysis = routing.RouteByAI(ctx, prompt, view, a.Pool)
	case "rule-then-ai":
		model, reason, analysis = routing.RouteCombined(ctx, prompt, view, a.Pool)
	default:
		model, reason, analysis = routing.RouteByRules(prompt, view)
	}

	if allowed != nil {
		policyModels := make([]any, 0, view.Models.Len())
		for _, name := range view.Models.Keys() {
			policyModels = append(policyModels, name)
		}
		analysis.Set("policy_models", policyModels)
	}

	if cfg.Sticky {
		if id := omap.AsString(interaction); id != "" {
			a.Sessions().Set(bindKey("interaction", id), model)
			analysis.Set("interaction_bound", id)
		}
		if sessionID != "" {
			a.Sessions().Set(bindKey("session", sessionID), model)
			analysis.Set("session_bound", sessionID)
		}
	}
	return model, reason, elapsed(), analysis
}

// bindKey namespaces the two kinds of sticky key so they share one store without an id of one
// kind ever being able to answer a lookup of the other.
func bindKey(kind, value string) string { return kind + ":" + value }

// finalizeTrace closes this request out as one turn and hands it to the store.
//
// The store decides whether that turn opens a new record or appends to the interaction already
// underway. Everything here describes this HTTP request only; the interaction-level totals are
// computed at merge time.
func (a *App) finalizeTrace(trace *omap.Map, start time.Time, status string,
	content any, usage *omap.Map, finishReason any, errText string, toolCalls []any) {
	trace.Set("status", status)
	totalMS := float64(time.Since(start).Microseconds()) / 1000
	trace.Set("total_ms", round1(totalMS))

	var response *omap.Map
	if errText != "" {
		trace.Set("error", errText)
	} else {
		response = mapOf(
			"content", content,
			"finish_reason", finishReason,
			"usage", nilIfEmptyMap(usage),
			// The tool calls the model asked for. This is the half of an agentic chain that used
			// to be lost: the assistant message carrying them was only ever visible in the *next*
			// request's replayed messages, so the final turn -- which asks for no tools -- made it
			// look as though none had been requested at all.
			"tool_calls", nilIfEmptySlice(toolCalls),
		)
		trace.Set("response", response)
	}

	routing := trace.Map("routing")
	decisionMS := 0.0
	if routing != nil {
		decisionMS = routing.Float("decision_ms", 0)
	}
	if backend := trace.Map("backend"); backend != nil {
		backend.Set("latency_ms", round1(totalMS-decisionMS))
	}

	request := trace.Map("request")
	if request == nil {
		request = omap.New()
	}
	messages := request.Slice("messages")
	turn := mapOf(
		"ts", trace.Value("ts"),
		"request_id", trace.Value("request_id"),
		"initiator", trace.Value("initiator"),
		"message_count", json.Number(fmt.Sprintf("%d", len(messages))),
		// Kept so the merge can tell an appended chain from a rewritten one, then dropped for the
		// appended case -- the full chain is stored once at the top level.
		"messages", messages,
		"params", request.Value("params"),
		"stream", request.Value("stream"),
		"model", valueOf(routing, "model"),
		"deployment", valueOf(trace.Map("backend"), "deployment"),
		// Both protocols are recorded per turn rather than once per interaction: one chain can be
		// answered by an Azure deployment on its first turn and a Claude endpoint on its second,
		// and a turn that does not say which is which cannot explain itself.
		"client_protocol", trace.Value("client_protocol"),
		"protocol", valueOf(trace.Map("backend"), "protocol"),
		"status", status,
		"total_ms", trace.Value("total_ms"),
		"response", nilIfEmptyMap(response),
		"error", nilIfBlank(errText),
	)
	trace.Set("turns", []any{turn})
	a.Traces.Add(trace)
}

func valueOf(m *omap.Map, key string) any {
	if m == nil {
		return nil
	}
	return m.Value(key)
}

func nilIfEmptyMap(m *omap.Map) any {
	if m == nil {
		return nil
	}
	return m
}

func nilIfEmptySlice(items []any) any {
	if len(items) == 0 {
		return nil
	}
	return items
}

// -- The gated answer ---------------------------------------------------------

// gatedContext builds the context for a request the AI-credit gate answers itself.
//
// A trace is still written -- an operator asking "why did nobody route through the router this
// morning" needs to find the answer in the trace list -- with the gate's name in the model and
// reason columns so it neither counts as a backend call nor as an error.
func (a *App) gatedContext(r *http.Request, body, key *omap.Map, sessionID string, start time.Time,
	clientProtocol string, gated *omap.Map, prompt string, interaction any) *callContext {
	cfg := a.Config()
	messages := body.Slice("messages")
	params := omap.New()
	for _, field := range body.Keys() {
		if field != "messages" {
			params.Set(field, body.Value(field))
		}
	}

	note := fmt.Sprintf("answered by the AI-credit gate: %s -- ", omap.AsString(gated.Value("enterprise")))
	if gated.Str("reason") == aicredits.ReasonBudget {
		note += fmt.Sprintf("the caller's user-level budget still has $%s left", commas(gated.Float("headroom_usd", 0)))
	} else {
		note += "the Copilot pool still has credits"
	}
	note += ", so the request was neither routed nor sent to the decision model"

	trace := mapOf(
		"id", wire.NewID("")[:8],
		"ts", time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00"),
		"user_id", key.Value("user_login"),
		"api_key_id", key.Value("id"),
		"api_key_name", key.Value("name"),
		"api_key_scope", keyscope.Describe(key.Map("scope")),
		"session_id", nilIfBlank(sessionID),
		"interaction_id", interaction,
		"request_id", headerOrNil(r, "x-request-id"),
		"initiator", headerOrNil(r, "x-initiator"),
		"client_ip", clientIP(r),
		"client_protocol", clientProtocol,
		"strategy", cfg.Strategy,
		"sticky", cfg.Sticky,
		"prompt_preview", headRunes(prompt, 120),
		"request", mapOf(
			"headers", sanitizedHeaders(r),
			"messages", messages,
			"params", params,
			"stream", body.Bool("stream", false),
		),
		"routing", mapOf(
			"model", aicredits.GateReason,
			"reason", aicredits.GateReason,
			"decision_ms", json.Number("0.0"),
			"analysis", mapOf(
				"type", "gate",
				"note", note,
				"reason", gated.Value("reason"),
				"enterprise", gated.Value("enterprise"),
				"remaining_credits", gated.Value("remaining"),
				"pool_total", gated.Value("pool_total"),
				"headroom_usd", gated.Value("headroom_usd"),
				"budget_usd", gated.Value("budget_usd"),
			),
		),
		"backend", mapOf(
			"deployment", nil, "api", nil, "protocol", nil,
			"provider", nil, "base_url", nil, "api_type", nil, "sent_params", omap.New(),
		),
		"response", nil,
		"status", "pending",
		"total_ms", nil,
	)
	a.Traces.ResolveInteraction(trace)
	logInfo("gated id=%s user=%s enterprise=%v reason=%s remaining=%v headroom=%v",
		trace.Str("id"), key.Str("user_login"), gated.Value("enterprise"), gated.Str("reason"),
		gated.Value("remaining"), gated.Value("headroom_usd"))

	headers := map[string]string{
		"x-trace-id":           trace.Str("id"),
		"x-routed-model":       aicredits.GateReason,
		"x-router-reason":      aicredits.GateReason,
		"x-router-decision-ms": "0.0",
	}
	if interaction != nil {
		headers["x-router-interaction-id"] = omap.AsString(interaction)
	}

	model := omap.AsString(body.Value("model"))
	if model == "" {
		model = aicredits.GateReason
	}
	return &callContext{
		Gated:          true,
		Message:        gated.Str("message"),
		Model:          model,
		Payload:        mapOf("stream", body.Bool("stream", false)),
		Trace:          trace,
		Headers:        headers,
		Start:          start,
		ClientProtocol: clientProtocol,
		Cfg:            cfg,
	}
}

func commas(v float64) string {
	formatted := fmt.Sprintf("%.2f", v)
	whole, fraction, _ := strings.Cut(formatted, ".")
	var grouped strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return grouped.String() + "." + fraction
}

// serveGated answers a gated request with the note as a normal assistant message, in the
// caller's protocol and streaming mode.
//
// A 200 rather than a 4xx because the note is meant to be read in the chat window, and Copilot
// shows an error status as a bare failure.
func (a *App) serveGated(w http.ResponseWriter, r *http.Request, ctx *callContext) error {
	completion := mapOf(
		"id", "chatcmpl-"+ctx.Trace.Str("id"),
		"object", "chat.completion",
		"created", json.Number(fmt.Sprintf("%d", time.Now().Unix())),
		"model", ctx.Model,
		"choices", []any{mapOf(
			"index", json.Number("0"),
			"message", mapOf("role", "assistant", "content", ctx.Message),
			"finish_reason", "stop",
		)},
		"usage", mapOf(
			"prompt_tokens", json.Number("0"),
			"completion_tokens", json.Number("0"),
			"total_tokens", json.Number("0"),
		),
	)
	if ctx.Payload.Bool("stream", false) {
		chunks := chunksFromCompletion(completion)
		return a.streamResponse(w, r, ctx, chunks)
	}
	return a.finishAndRender(w, completion, ctx)
}

// finishAndRender records the turn, then answers in the caller's protocol.
func (a *App) finishAndRender(w http.ResponseWriter, completion *omap.Map, ctx *callContext) error {
	choice := firstChoice(completion)
	message := choice.Map("message")
	if message == nil {
		message = omap.New()
	}
	a.finalizeTrace(ctx.Trace, ctx.Start, "ok",
		message.Value("content"), completion.Map("usage"), choice.Value("finish_reason"),
		"", message.Slice("tool_calls"))
	if ctx.ClientProtocol == "anthropic" {
		writeJSON(w, http.StatusOK, wire.OpenAIResponseToAnthropic(completion, ctx.Model), ctx.Headers)
		return nil
	}
	writeJSON(w, http.StatusOK, completion, ctx.Headers)
	return nil
}

func firstChoice(data *omap.Map) *omap.Map {
	choices := data.Slice("choices")
	if len(choices) == 0 {
		return omap.New()
	}
	if choice, ok := choices[0].(*omap.Map); ok {
		return choice
	}
	return omap.New()
}

// serve calls the upstream in its own protocol and answers in the caller's.
//
// Four combinations reduce to two decisions taken independently: which upstream branch produces
// the canonical result, and which renderer turns it into the caller's shape.
func (a *App) serve(w http.ResponseWriter, r *http.Request, ctx *callContext) error {
	resolved := ctx.Resolved
	payload := ctx.Payload
	streaming := payload.Bool("stream", false)

	fail := func(err error) error {
		if ctx.Trace.Str("status") == "pending" {
			a.finalizeTrace(ctx.Trace, ctx.Start, "error", nil, nil, nil, err.Error(), nil)
		}
		log.Printf("ERROR mr: backend call failed model=%s provider=%s: %v",
			ctx.Model, resolved.Provider.Name, err)
		return errorf(http.StatusBadGateway, "backend model call failed: %v", err)
	}

	var chunks chunkSource
	var completion *omap.Map

	switch {
	case resolved.Provider.Protocol() == "anthropic":
		client, err := a.Pool.Get(resolved.Provider, "chat")
		if err != nil {
			return fail(err)
		}
		body := wire.OpenAIRequestToAnthropic(payload, resolved.UpstreamModel)
		// What actually went on the wire, which for a converted request is not what the caller
		// sent: a trace that showed only the caller's parameters could not explain a max_tokens
		// the caller never set.
		sent := omap.New()
		for _, field := range body.Keys() {
			if field != "messages" && field != "system" {
				sent.Set(field, body.Value(field))
			}
		}
		backend := ctx.Trace.Map("backend")
		backend.Set("sent_params", sent)
		if dropped := wire.AnthropicUnsupportedParams(payload); len(dropped) > 0 {
			items := make([]any, len(dropped))
			for i, name := range dropped {
				items[i] = name
			}
			backend.Set("dropped_params", items)
		}
		if streaming {
			source, err := a.openAnthropicStream(r.Context(), client, body, ctx.Model)
			if err != nil {
				return fail(err)
			}
			chunks = source
		} else {
			data, err := client.Create(r.Context(), resolved.UpstreamModel, body)
			if err != nil {
				return fail(err)
			}
			completion = wire.AnthropicResponseToOpenAI(data, ctx.Model)
		}
	case resolved.API == "responses":
		client, err := a.Pool.Get(resolved.Provider, "responses")
		if err != nil {
			return fail(err)
		}
		completion, err = completeViaResponsesAPI(r.Context(), client, resolved.UpstreamModel, payload)
		if err != nil {
			return fail(err)
		}
		if streaming {
			chunks = chunksFromCompletion(completion)
		}
	default:
		client, err := a.Pool.Get(resolved.Provider, "chat")
		if err != nil {
			return fail(err)
		}
		body := payload.Clone()
		body.Set("model", resolved.UpstreamModel)
		if streaming {
			source, err := openOpenAIStream(r.Context(), client, resolved.UpstreamModel, body)
			if err != nil {
				return fail(err)
			}
			chunks = source
		} else {
			data, err := client.Create(r.Context(), resolved.UpstreamModel, body)
			if err != nil {
				return fail(err)
			}
			completion = data
		}
	}

	if streaming {
		return a.streamResponse(w, r, ctx, chunks)
	}
	return a.finishAndRender(w, completion, ctx)
}

// completeViaResponsesAPI adapts a chat request to the Responses API and converts the result
// back to the chat.completion shape.
func completeViaResponsesAPI(ctx context.Context, client *upstream.Client, model string, payload *omap.Map) (*omap.Map, error) {
	body := mapOf("model", model, "input", responsesInput(payload.Slice("messages")))
	maxOut := payload.Value("max_completion_tokens")
	if maxOut == nil {
		maxOut = payload.Value("max_tokens")
	}
	if maxOut != nil {
		body.Set("max_output_tokens", maxOut)
	}
	resp, err := client.Create(ctx, model, body)
	if err != nil {
		return nil, err
	}
	// Any OpenAI-compatible upstream can be configured, so a missing created_at must not fail
	// the whole request.
	created := resp.Value("created_at")
	if created == nil {
		created = json.Number(fmt.Sprintf("%d", time.Now().Unix()))
	}
	return mapOf(
		"id", resp.Value("id"),
		"object", "chat.completion",
		"created", created,
		"model", model,
		"choices", []any{mapOf(
			"index", json.Number("0"),
			"message", mapOf("role", "assistant", "content", outputText(resp)),
			"finish_reason", "stop",
		)},
		"usage", nilIfEmptyMap(resp.Map("usage")),
	), nil
}

// outputText flattens the Responses API's output array into the plain text a chat completion
// carries. The SDK exposed this as `output_text`; over the wire it has to be assembled.
func outputText(resp *omap.Map) string {
	if text := resp.Str("output_text"); text != "" {
		return text
	}
	var parts strings.Builder
	for _, item := range resp.Slice("output") {
		entry, ok := item.(*omap.Map)
		if !ok || entry.Str("type") != "message" {
			continue
		}
		for _, contentItem := range entry.Slice("content") {
			block, ok := contentItem.(*omap.Map)
			if !ok {
				continue
			}
			if kind := block.Str("type"); kind == "output_text" || kind == "text" {
				parts.WriteString(block.Str("text"))
			}
		}
	}
	return parts.String()
}

// responsesInput converts chat-completions messages into Responses API input items.
//
// The two protocols disagree about tool calls, and the Responses API rejects the chat shape
// outright: an assistant tool call is `content: null` plus `tool_calls` there and a
// `function_call` item here, and a result is a `role: tool` message there and a
// `function_call_output` item here. Without this, every agentic loop routed to a Responses-only
// model died on its second call.
func responsesInput(messages []any) []any {
	items := []any{}
	for _, entry := range messages {
		msg, ok := entry.(*omap.Map)
		if !ok {
			continue
		}
		if msg.Str("role") == "tool" {
			items = append(items, mapOf(
				"type", "function_call_output",
				"call_id", msg.Value("tool_call_id"),
				"output", orEmptyString(msg.Value("content")),
			))
			continue
		}
		calls := msg.Slice("tool_calls")
		content := msg.Value("content")
		// A tool-call turn carries no text, and an empty message item is not worth sending.
		if !isBlank(content) || len(calls) == 0 {
			items = append(items, mapOf("role", msg.Value("role"), "content", orEmptyString(content)))
		}
		for _, callItem := range calls {
			call, ok := callItem.(*omap.Map)
			if !ok {
				continue
			}
			fn := call.Map("function")
			if fn == nil {
				fn = omap.New()
			}
			arguments := fn.Str("arguments")
			if arguments == "" {
				arguments = "{}"
			}
			items = append(items, mapOf(
				"type", "function_call",
				"call_id", call.Value("id"),
				"name", fn.Value("name"),
				"arguments", arguments,
			))
		}
	}
	return items
}

func isBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}

func orEmptyString(v any) any {
	if isBlank(v) {
		return ""
	}
	return v
}
