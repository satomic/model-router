// Package wire translates between the OpenAI chat-completions wire format and the Anthropic
// Messages wire format, in both directions.
//
// Why this exists: a caller may speak either protocol and an upstream connection may speak
// either protocol, which is four combinations. Writing four pipelines would mean four places
// to fix every bug, so one shape is chosen as canonical and the other is converted at the edges:
//
//	client (either protocol) -> [canonical: OpenAI chat completions] -> upstream (either)
//
// OpenAI is the canonical form because everything between the two edges already reads it: the
// router extracts the prompt from `messages`, the rules match on it, the trace records it, and
// the model policy filters the catalog it belongs to.
//
// Streaming follows the same rule with the same reasoning: an Anthropic upstream's events are
// decoded into OpenAI chunks (StreamDecoder) and an Anthropic client's events are emitted from
// OpenAI chunks (EventEncoder). Two parsers and two emitters over one internal vocabulary,
// rather than four stream pipelines.
//
// Nothing here performs IO or reads configuration: it is pure value-to-value, so it is
// testable without a server and without an upstream.
package wire

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/satomic/model-router/server-go/internal/omap"
)

// DefaultMaxTokens is the ceiling applied when a request arrives without one. Anthropic
// requires max_tokens on every request; OpenAI treats it as optional. Deliberately large:
// this is a cap, not a target, and a small default would truncate long answers for callers
// who never asked for a limit.
const DefaultMaxTokens = 8192

// Anthropic stop_reason -> OpenAI finish_reason. "pause_turn" and "refusal" are newer values;
// both end the turn from the caller's point of view, so they map to "stop" rather than to
// nothing, because a null finish_reason on a completed response makes clients hang.
var stopReasonToFinish = map[string]string{
	"end_turn":      "stop",
	"stop_sequence": "stop",
	"max_tokens":    "length",
	"tool_use":      "tool_calls",
	"pause_turn":    "stop",
	"refusal":       "stop",
}

var finishToStopReason = map[string]string{
	"stop":           "end_turn",
	"length":         "max_tokens",
	"tool_calls":     "tool_use",
	"content_filter": "refusal",
	"function_call":  "tool_use",
}

// Sampling parameters OpenAI has and Anthropic does not. Dropped rather than approximated: a
// frequency penalty silently reinterpreted as something else is worse than one not applied.
var unsupportedByAnthropic = []string{"presence_penalty", "frequency_penalty", "seed", "n", "logprobs"}

func NewID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// A failing CSPRNG cannot be worked around: there is no safe token to hand back, and
		// net/http contains the panic to this one connection.
		panic(err)
	}
	return prefix + hex.EncodeToString(buf)
}

// TextOf flattens a content value (string, or a list of blocks) to plain text.
func TextOf(content any) string {
	switch t := content.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var parts strings.Builder
		for _, item := range t {
			switch block := item.(type) {
			case string:
				parts.WriteString(block)
			case *omap.Map:
				if kind := block.Str("type"); kind == "text" || kind == "input_text" {
					parts.WriteString(block.Str("text"))
				}
			}
		}
		return parts.String()
	default:
		return omap.AsString(content)
	}
}

func numberOf(v float64) json.Number {
	if v == float64(int64(v)) {
		return json.Number(fmt.Sprintf("%d", int64(v)))
	}
	return json.Number(fmt.Sprintf("%g", v))
}

func intNumber(v int) json.Number { return json.Number(fmt.Sprintf("%d", v)) }

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// ---------------------------------------------------------------------------
// OpenAI request -> Anthropic request
// ---------------------------------------------------------------------------

// imageBlockFromOpenAI converts an OpenAI image_url part to an Anthropic image block.
func imageBlockFromOpenAI(part *omap.Map) *omap.Map {
	url := ""
	if inner := part.Map("image_url"); inner != nil {
		url = inner.Str("url")
	}
	if url == "" {
		url = part.Str("url")
	}
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		// data:<media_type>;base64,<data>
		header, data, found := strings.Cut(url, ",")
		if !found {
			return nil
		}
		mediaType, _, _ := strings.Cut(header[5:], ";")
		if mediaType == "" {
			mediaType = "image/png"
		}
		return mapOf("type", "image", "source",
			mapOf("type", "base64", "media_type", mediaType, "data", data))
	}
	return mapOf("type", "image", "source", mapOf("type", "url", "url", url))
}

// contentBlocksFromOpenAI turns OpenAI message content (string or multimodal parts) into
// Anthropic content blocks.
func contentBlocksFromOpenAI(content any) []any {
	if content == nil {
		return nil
	}
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []any{mapOf("type", "text", "text", text)}
	}
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	blocks := []any{}
	for _, item := range items {
		if text, ok := item.(string); ok {
			if text != "" {
				blocks = append(blocks, mapOf("type", "text", "text", text))
			}
			continue
		}
		part, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		switch part.Str("type") {
		case "text", "input_text":
			if text := part.Str("text"); text != "" {
				blocks = append(blocks, mapOf("type", "text", "text", text))
			}
		case "image_url", "input_image", "image":
			if block := imageBlockFromOpenAI(part); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

func toolsToAnthropic(tools []any) []any {
	out := []any{}
	for _, item := range tools {
		tool, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		fn := tool.Map("function")
		if fn == nil {
			fn = tool
		}
		name := fn.Str("name")
		if name == "" {
			continue
		}
		schema := fn.Value("parameters")
		if schema == nil {
			schema = mapOf("type", "object")
		}
		entry := mapOf("name", name, "input_schema", schema)
		if description := fn.Str("description"); description != "" {
			entry.Set("description", description)
		}
		out = append(out, entry)
	}
	return out
}

func toolChoiceToAnthropic(choice any) *omap.Map {
	switch t := choice.(type) {
	case nil:
		return nil // Anthropic's own default
	case string:
		switch t {
		case "auto":
			return nil
		case "required":
			return mapOf("type", "any")
		case "none":
			return mapOf("type", "none")
		}
	case *omap.Map:
		name := ""
		if fn := t.Map("function"); fn != nil {
			name = fn.Str("name")
		}
		if name == "" {
			name = t.Str("name")
		}
		if name != "" {
			return mapOf("type", "tool", "name", name)
		}
	}
	return nil
}

// OpenAIRequestToAnthropic converts a canonical OpenAI chat-completions request into an
// Anthropic Messages request body.
//
// Consecutive same-role messages are merged, because Anthropic rejects two user turns in a
// row while OpenAI accepts them, and Copilot-style agent loops do produce them (a tool result
// followed by a user note is two user turns).
func OpenAIRequestToAnthropic(payload *omap.Map, upstreamModel string) *omap.Map {
	var systemParts []string
	type turn struct {
		role    string
		content []any
	}
	var messages []turn

	for _, item := range payload.Slice("messages") {
		msg, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		switch msg.Str("role") {
		case "system", "developer":
			if text := TextOf(msg.Value("content")); text != "" {
				systemParts = append(systemParts, text)
			}
		case "tool":
			// An OpenAI tool result is its own message; in Anthropic it is a user-turn block.
			block := mapOf(
				"type", "tool_result",
				"tool_use_id", msg.Str("tool_call_id"),
				"content", TextOf(msg.Value("content")),
			)
			messages = append(messages, turn{role: "user", content: []any{block}})
		case "assistant":
			blocks := contentBlocksFromOpenAI(msg.Value("content"))
			for _, callItem := range msg.Slice("tool_calls") {
				call, ok := callItem.(*omap.Map)
				if !ok {
					continue
				}
				fn := call.Map("function")
				if fn == nil {
					fn = omap.New()
				}
				raw := fn.Str("arguments")
				if raw == "" {
					raw = "{}"
				}
				var args any
				if decoded, err := omap.FromJSON([]byte(raw)); err == nil {
					args = decoded
				} else {
					// A partially generated argument string is not worth failing the request
					// over: send it through as a single field the tool can at least see.
					args = mapOf("_raw", fn.Str("arguments"))
				}
				if _, ok := args.(*omap.Map); !ok {
					args = mapOf("_raw", args)
				}
				id := call.Str("id")
				if id == "" {
					id = NewID("toolu_")
				}
				blocks = append(blocks, mapOf(
					"type", "tool_use", "id", id, "name", fn.Str("name"), "input", args))
			}
			if len(blocks) > 0 {
				messages = append(messages, turn{role: "assistant", content: blocks})
			}
		default:
			// Everything else is a user turn.
			if blocks := contentBlocksFromOpenAI(msg.Value("content")); len(blocks) > 0 {
				messages = append(messages, turn{role: "user", content: blocks})
			}
		}
	}

	// Merge consecutive same-role turns.
	var merged []turn
	for _, msg := range messages {
		if len(merged) > 0 && merged[len(merged)-1].role == msg.role {
			merged[len(merged)-1].content = append(merged[len(merged)-1].content, msg.content...)
			continue
		}
		content := make([]any, len(msg.content))
		copy(content, msg.content)
		merged = append(merged, turn{role: msg.role, content: content})
	}
	// Anthropic requires at least one message. An empty list can only come from a request
	// whose only content was a system prompt; a single empty user turn keeps it valid.
	if len(merged) == 0 {
		merged = []turn{{role: "user", content: []any{mapOf("type", "text", "text", "")}}}
	}
	messageList := make([]any, 0, len(merged))
	for _, msg := range merged {
		messageList = append(messageList, mapOf("role", msg.role, "content", msg.content))
	}

	maxTokens := payload.Float("max_tokens", 0)
	if maxTokens <= 0 {
		maxTokens = payload.Float("max_completion_tokens", 0)
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	body := mapOf(
		"model", upstreamModel,
		"messages", messageList,
		"max_tokens", intNumber(int(maxTokens)),
	)
	if len(systemParts) > 0 {
		body.Set("system", strings.Join(systemParts, "\n\n"))
	}
	if payload.Value("temperature") != nil {
		// Anthropic's range is 0..1; OpenAI allows up to 2, and a value above 1 is rejected
		// rather than clamped upstream, so it is clamped here.
		temperature, _ := omap.AsFloat(payload.Value("temperature"))
		if temperature < 0 {
			temperature = 0
		}
		if temperature > 1 {
			temperature = 1
		}
		body.Set("temperature", numberOf(temperature))
	}
	if payload.Value("top_p") != nil {
		topP, _ := omap.AsFloat(payload.Value("top_p"))
		body.Set("top_p", numberOf(topP))
	}
	switch stop := payload.Value("stop").(type) {
	case string:
		if stop != "" {
			body.Set("stop_sequences", []any{stop})
		}
	case []any:
		if len(stop) > 0 {
			body.Set("stop_sequences", stop)
		}
	}
	if payload.Bool("stream", false) {
		// Anthropic reports token counts on message_start / message_delta unconditionally, so
		// unlike OpenAI there is no stream_options flag to ask for usage.
		body.Set("stream", true)
	}
	if tools := toolsToAnthropic(payload.Slice("tools")); len(tools) > 0 {
		body.Set("tools", tools)
		if choice := toolChoiceToAnthropic(payload.Value("tool_choice")); choice != nil {
			body.Set("tool_choice", choice)
		}
	}
	if user := payload.Str("user"); user != "" {
		body.Set("metadata", mapOf("user_id", user))
	}
	return body
}

// AnthropicUnsupportedParams lists which OpenAI-only parameters were dropped, for the trace.
func AnthropicUnsupportedParams(payload *omap.Map) []string {
	dropped := []string{}
	for _, key := range unsupportedByAnthropic {
		value := payload.Value(key)
		if value == nil {
			continue
		}
		if f, ok := omap.AsFloat(value); ok && f == 1 {
			continue
		}
		dropped = append(dropped, key)
	}
	if payload.Value("response_format") != nil {
		dropped = append(dropped, "response_format")
	}
	return dropped
}

// ---------------------------------------------------------------------------
// Anthropic response -> OpenAI response
// ---------------------------------------------------------------------------

func usageToOpenAI(usage *omap.Map) *omap.Map {
	if usage == nil {
		usage = omap.New()
	}
	prompt := int(usage.Float("input_tokens", 0))
	completion := int(usage.Float("output_tokens", 0))
	out := mapOf(
		"prompt_tokens", intNumber(prompt),
		"completion_tokens", intNumber(completion),
		"total_tokens", intNumber(prompt+completion),
	)
	if cached := int(usage.Float("cache_read_input_tokens", 0)); cached != 0 {
		out.Set("prompt_tokens_details", mapOf("cached_tokens", intNumber(cached)))
	}
	return out
}

// AnthropicResponseToOpenAI converts an Anthropic Messages response into a chat.completion.
//
// `model` is the router's own model name rather than the upstream one, so a caller always
// sees the name it is entitled to see: which deployment answered is a routing detail that
// belongs in the trace and in x-routed-model, not in the response body.
func AnthropicResponseToOpenAI(data *omap.Map, model string) *omap.Map {
	var textParts strings.Builder
	toolCalls := []any{}
	for _, item := range data.Slice("content") {
		block, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		switch block.Str("type") {
		case "text":
			textParts.WriteString(block.Str("text"))
		case "tool_use":
			id := block.Str("id")
			if id == "" {
				id = NewID("call_")
			}
			input := block.Value("input")
			if input == nil {
				input = omap.New()
			}
			arguments, err := json.Marshal(input)
			if err != nil {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, mapOf(
				"id", id,
				"type", "function",
				"function", mapOf("name", block.Str("name"), "arguments", string(arguments)),
			))
		}
		// thinking / redacted_thinking blocks are deliberately not surfaced: they have no
		// OpenAI equivalent, and inventing one would put reasoning text into the message
		// content that the caller would render as the answer.
	}

	message := omap.New()
	message.Set("role", "assistant")
	if text := textParts.String(); text != "" {
		message.Set("content", text)
	} else {
		message.Set("content", nil)
	}
	if len(toolCalls) > 0 {
		message.Set("tool_calls", toolCalls)
	}
	finish := stopReasonToFinish[data.Str("stop_reason")]
	if finish == "" {
		finish = "stop"
	}
	id := data.Str("id")
	if id == "" {
		id = NewID("chatcmpl-")
	}
	return mapOf(
		"id", id,
		"object", "chat.completion",
		"created", intNumber(int(time.Now().Unix())),
		"model", model,
		"choices", []any{mapOf("index", intNumber(0), "message", message, "finish_reason", finish)},
		"usage", usageToOpenAI(data.Map("usage")),
	)
}
