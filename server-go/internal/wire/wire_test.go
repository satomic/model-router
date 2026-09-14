package wire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/satomic/model-router/server-go/internal/omap"
)

// parse is the shorthand every case uses to write its fixture as literal JSON.
func parse(t *testing.T, text string) *omap.Map {
	t.Helper()
	value, err := omap.FromJSON([]byte(text))
	if err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	m, ok := value.(*omap.Map)
	if !ok {
		t.Fatalf("fixture is not an object")
	}
	return m
}

func encode(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("could not encode: %v", err)
	}
	return string(data)
}

func TestOpenAIRequestToAnthropicMergesConsecutiveUserTurns(t *testing.T) {
	// Anthropic rejects two user turns in a row while OpenAI accepts them, and a Copilot-style
	// agent loop does produce them: a tool result followed by a user note is two user turns.
	payload := parse(t, `{
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": "first"},
			{"role": "tool", "tool_call_id": "call_1", "content": "tool said hi"},
			{"role": "user", "content": "second"}
		]
	}`)
	body := OpenAIRequestToAnthropic(payload, "claude-x")

	if got := body.Str("system"); got != "be brief" {
		t.Errorf("system = %q, want %q", got, "be brief")
	}
	messages := body.Slice("messages")
	if len(messages) != 1 {
		t.Fatalf("three consecutive user turns should merge into 1 message, got %d", len(messages))
	}
	merged := messages[0].(*omap.Map)
	if merged.Str("role") != "user" {
		t.Errorf("merged role = %q, want user", merged.Str("role"))
	}
	if blocks := merged.Slice("content"); len(blocks) != 3 {
		t.Errorf("merged content blocks = %d, want 3", len(blocks))
	}
	// A request with no max_tokens still has to be sendable: Anthropic requires the field.
	if got := body.Str("max_tokens"); got != "8192" {
		t.Errorf("max_tokens = %s, want the %d default", got, DefaultMaxTokens)
	}
}

func TestOpenAIRequestToAnthropicClampsTemperature(t *testing.T) {
	// OpenAI allows up to 2 and Anthropic rejects anything above 1 rather than clamping, so the
	// clamp has to happen here or the request fails upstream.
	body := OpenAIRequestToAnthropic(parse(t, `{"messages":[{"role":"user","content":"x"}],"temperature":1.8}`), "m")
	if got := body.Str("temperature"); got != "1" {
		t.Errorf("temperature = %s, want 1", got)
	}
}

func TestOpenAIRequestToAnthropicKeepsAtLeastOneMessage(t *testing.T) {
	// A request whose only content was a system prompt would otherwise produce an empty list,
	// which Anthropic refuses.
	body := OpenAIRequestToAnthropic(parse(t, `{"messages":[{"role":"system","content":"only this"}]}`), "m")
	if len(body.Slice("messages")) != 1 {
		t.Fatalf("want one placeholder user turn, got %d", len(body.Slice("messages")))
	}
}

func TestAnthropicResponseToOpenAIConvertsToolUse(t *testing.T) {
	data := parse(t, `{
		"id": "msg_1",
		"stop_reason": "tool_use",
		"content": [
			{"type": "text", "text": "let me look"},
			{"type": "thinking", "thinking": "should not surface"},
			{"type": "tool_use", "id": "toolu_9", "name": "search", "input": {"q": "go"}}
		],
		"usage": {"input_tokens": 11, "output_tokens": 7, "cache_read_input_tokens": 4}
	}`)
	out := AnthropicResponseToOpenAI(data, "router-name")

	if out.Str("model") != "router-name" {
		// The caller sees the router's own model name; which deployment answered is a routing
		// detail that belongs in the trace.
		t.Errorf("model = %q, want the router's name", out.Str("model"))
	}
	choice := out.Slice("choices")[0].(*omap.Map)
	if got := choice.Str("finish_reason"); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
	message := choice.Map("message")
	if got := message.Str("content"); got != "let me look" {
		// A thinking block must not leak into the content the caller renders as the answer.
		t.Errorf("content = %q, want only the text block", got)
	}
	calls := message.Slice("tool_calls")
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	fn := calls[0].(*omap.Map).Map("function")
	if got := fn.Str("arguments"); got != `{"q":"go"}` {
		t.Errorf("arguments = %s, want the input re-encoded as a JSON string", got)
	}
	usage := out.Map("usage")
	if usage.Str("total_tokens") != "18" {
		t.Errorf("total_tokens = %s, want 18", usage.Str("total_tokens"))
	}
	if usage.Map("prompt_tokens_details").Str("cached_tokens") != "4" {
		t.Errorf("cached tokens were dropped: %s", encode(t, usage))
	}
}

func TestAnthropicRequestToOpenAIOrdersToolResultsBeforeTheirTurn(t *testing.T) {
	// In OpenAI a tool result is its own message answering the previous assistant turn, so it has
	// to come first; in Anthropic it is a block inside the following user turn.
	body := parse(t, `{
		"model": "claude-x",
		"max_tokens": 64,
		"system": "sys",
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "42"},
				{"type": "text", "text": "and now?"}
			]}
		]
	}`)
	payload := AnthropicRequestToOpenAI(body)
	messages := payload.Slice("messages")
	if len(messages) != 3 {
		t.Fatalf("want system + tool + user, got %d: %s", len(messages), encode(t, messages))
	}
	roles := []string{}
	for _, m := range messages {
		roles = append(roles, m.(*omap.Map).Str("role"))
	}
	if roles[0] != "system" || roles[1] != "tool" || roles[2] != "user" {
		t.Errorf("roles = %v, want [system tool user]", roles)
	}
	if payload.Str("model") != "claude-x" {
		t.Errorf("the caller's requested model must be carried through, got %q", payload.Str("model"))
	}
}

func TestStreamDecoderBridgesToolCallIndices(t *testing.T) {
	// Anthropic names a tool call once and then streams its arguments as partial JSON; OpenAI
	// repeats the tool-call index on every fragment. The decoder holds that mapping.
	decoder := NewStreamDecoder("m", "", 1700000000)
	feed := func(event, data string) []*omap.Map {
		chunks, err := decoder.Feed(event, parse(t, data))
		if err != nil {
			t.Fatalf("feed %s: %v", event, err)
		}
		return chunks
	}

	feed("message_start", `{"message":{"id":"msg_up","usage":{"input_tokens":5}}}`)
	feed("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"search"}}`)
	args := feed("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`)
	if len(args) != 1 {
		t.Fatalf("want one chunk for an argument fragment, got %d", len(args))
	}
	call := args[0].Slice("choices")[0].(*omap.Map).Map("delta").Slice("tool_calls")[0].(*omap.Map)
	if call.Str("index") != "0" {
		t.Errorf("tool_calls index = %s, want 0", call.Str("index"))
	}
	feed("message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`)
	final := feed("message_stop", `{}`)
	if len(final) != 1 {
		t.Fatalf("message_stop should emit the closing chunk, got %d", len(final))
	}
	closing := final[0].Slice("choices")[0].(*omap.Map)
	if closing.Str("finish_reason") != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", closing.Str("finish_reason"))
	}
	// Usage rides on the final chunk rather than on one of its own: a client that never asked
	// for stream_options would not expect a trailing chunk with an empty choices array.
	if final[0].Map("usage").Str("total_tokens") != "14" {
		t.Errorf("usage on the final chunk = %s, want 14 total", encode(t, final[0].Map("usage")))
	}
}

func TestStreamDecoderSurfacesAnErrorEvent(t *testing.T) {
	// A stream error has to end the caller's stream as incomplete, which a chunk cannot say.
	decoder := NewStreamDecoder("m", "", 0)
	_, err := decoder.Feed("error", parse(t, `{"error":{"message":"overloaded"}}`))
	if err == nil || err.Error() != "overloaded" {
		t.Fatalf("want the upstream's message, got %v", err)
	}
}

func TestEventEncoderClosesTextBeforeOpeningATool(t *testing.T) {
	// Anthropic content blocks do not interleave, so the open text block must be closed first.
	encoder := NewEventEncoder("m", "msg_fixed")
	frames := encoder.Feed(parse(t, `{"choices":[{"delta":{"content":"thinking"}}]}`))
	frames = append(frames, encoder.Feed(parse(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_1","function":{"name":"go","arguments":"{}"}}]}}]}`))...)
	frames = append(frames, encoder.Finish(parse(t, `{"completion_tokens":3,"prompt_tokens":2}`))...)

	var order []string
	for _, frame := range frames {
		if name, ok := eventName(frame); ok {
			order = append(order, name)
		}
	}
	want := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(order) != len(want) {
		t.Fatalf("frame order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("frame order = %v, want %v", order, want)
		}
	}
}

// eventName pulls the event name off an SSE frame's first line.
func eventName(frame string) (string, bool) {
	rest, found := strings.CutPrefix(frame, "event: ")
	if !found {
		return "", false
	}
	name, _, found := strings.Cut(rest, "\n")
	return name, found
}

func TestRoundTripOpenAIThroughAnthropicAndBack(t *testing.T) {
	// The four protocol combinations reduce to two conversions, so a value that survives both
	// is the strongest single check that neither drops a field the other needs.
	original := parse(t, `{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": "sure",
			"tool_calls": [{"id": "call_1", "type": "function",
			                "function": {"name": "go", "arguments": "{\"a\":1}"}}]}}],
		"usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}
	}`)
	anthropic := OpenAIResponseToAnthropic(original, "m")
	back := AnthropicResponseToOpenAI(anthropic, "m")

	choice := back.Slice("choices")[0].(*omap.Map)
	message := choice.Map("message")
	if message.Str("content") != "sure" {
		t.Errorf("content did not survive the round trip: %q", message.Str("content"))
	}
	if choice.Str("finish_reason") != "tool_calls" {
		t.Errorf("finish_reason did not survive: %q", choice.Str("finish_reason"))
	}
	fn := message.Slice("tool_calls")[0].(*omap.Map).Map("function")
	if fn.Str("name") != "go" || fn.Str("arguments") != `{"a":1}` {
		t.Errorf("tool call did not survive: %s", encode(t, fn))
	}
	if back.Map("usage").Str("total_tokens") != "7" {
		t.Errorf("usage did not survive: %s", encode(t, back.Map("usage")))
	}
}
