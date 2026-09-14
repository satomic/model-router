package wire

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/satomic/model-router/server-go/internal/omap"
)

// ---------------------------------------------------------------------------
// Anthropic stream -> OpenAI chunks
// ---------------------------------------------------------------------------

// StreamDecoder turns an Anthropic SSE event stream into OpenAI chat.completion.chunk values.
//
// Stateful because the two formats disagree about where information lives: Anthropic sends a
// tool call's id and name once in content_block_start and then streams its arguments as
// partial JSON, while OpenAI repeats the tool-call index on every fragment. The decoder holds
// the block-index-to-tool-call-index mapping that bridges the two.
type StreamDecoder struct {
	model         string
	id            string
	created       int64
	toolIndex     map[int]int // content-block index -> tool_calls index
	nextToolIndex int
	roleSent      bool
	usage         *omap.Map
	FinishReason  string
}

func NewStreamDecoder(model, responseID string, nowUnix int64) *StreamDecoder {
	if responseID == "" {
		responseID = NewID("chatcmpl-")
	}
	return &StreamDecoder{
		model:     model,
		id:        responseID,
		created:   nowUnix,
		toolIndex: map[int]int{},
		usage:     omap.New(),
	}
}

func (d *StreamDecoder) chunk(delta *omap.Map, finishReason any, usage *omap.Map) *omap.Map {
	chunk := mapOf(
		"id", d.id,
		"object", "chat.completion.chunk",
		"created", intNumber(int(d.created)),
		"model", d.model,
		"choices", []any{mapOf("index", intNumber(0), "delta", delta, "finish_reason", finishReason)},
	)
	if usage != nil {
		chunk.Set("usage", usage)
	}
	return chunk
}

// Feed takes one Anthropic event and returns zero or more OpenAI chunks.
func (d *StreamDecoder) Feed(eventType string, data *omap.Map) ([]*omap.Map, error) {
	out := []*omap.Map{}
	switch eventType {
	case "message_start":
		msg := data.Map("message")
		if msg == nil {
			msg = omap.New()
		}
		if id := msg.Str("id"); id != "" {
			// Keep the upstream id, so a trace and the client agree on which call this was.
			d.id = id
		}
		mergeInto(d.usage, msg.Map("usage"))
		if !d.roleSent {
			d.roleSent = true
			out = append(out, d.chunk(mapOf("role", "assistant", "content", ""), nil, nil))
		}
	case "content_block_start":
		block := data.Map("content_block")
		if block == nil {
			block = omap.New()
		}
		index := data.Int("index", 0)
		switch block.Str("type") {
		case "tool_use":
			toolIndex := d.nextToolIndex
			d.nextToolIndex++
			d.toolIndex[index] = toolIndex
			id := block.Str("id")
			if id == "" {
				id = NewID("call_")
			}
			out = append(out, d.chunk(mapOf("tool_calls", []any{mapOf(
				"index", intNumber(toolIndex),
				"id", id,
				"type", "function",
				"function", mapOf("name", block.Str("name"), "arguments", ""),
			)}), nil, nil))
		case "text":
			if text := block.Str("text"); text != "" {
				out = append(out, d.chunk(mapOf("content", text), nil, nil))
			}
		}
	case "content_block_delta":
		delta := data.Map("delta")
		if delta == nil {
			delta = omap.New()
		}
		index := data.Int("index", 0)
		switch delta.Str("type") {
		case "text_delta":
			if text := delta.Str("text"); text != "" {
				out = append(out, d.chunk(mapOf("content", text), nil, nil))
			}
		case "input_json_delta":
			toolIndex := d.toolIndex[index]
			out = append(out, d.chunk(mapOf("tool_calls", []any{mapOf(
				"index", intNumber(toolIndex),
				"function", mapOf("arguments", delta.Str("partial_json")),
			)}), nil, nil))
		}
		// thinking_delta and signature_delta are dropped for the reason given in
		// AnthropicResponseToOpenAI.
	case "message_delta":
		delta := data.Map("delta")
		if delta != nil {
			if reason := delta.Str("stop_reason"); reason != "" {
				d.FinishReason = stopReasonToFinish[reason]
				if d.FinishReason == "" {
					d.FinishReason = "stop"
				}
			}
		}
		mergeInto(d.usage, data.Map("usage"))
	case "message_stop":
		// Usage rides on the final chunk rather than on a chunk of its own. Anthropic reports
		// token counts unconditionally, so they are known by now, and an extra trailing chunk
		// with an empty choices array is exactly what a client that never asked for
		// stream_options would not expect.
		finish := d.FinishReason
		if finish == "" {
			finish = "stop"
		}
		out = append(out, d.chunk(omap.New(), finish, d.OpenAIUsage()))
	case "error":
		// Surfaced as an error rather than a chunk: the caller's stream has to end in a way
		// that says the answer is incomplete, and a chunk cannot say that.
		message := "anthropic stream error"
		if err := data.Map("error"); err != nil && err.Str("message") != "" {
			message = err.Str("message")
		}
		return nil, errors.New(message)
	}
	return out, nil
}

func (d *StreamDecoder) OpenAIUsage() *omap.Map {
	return usageToOpenAI(d.usage)
}

func mergeInto(target, source *omap.Map) {
	if source == nil {
		return
	}
	for _, key := range source.Keys() {
		target.Set(key, source.Value(key))
	}
}

// SSEEvent is one parsed `event:`/`data:` pair.
type SSEEvent struct {
	Type string
	Data *omap.Map
}

// ParseSSE turns SSE line pairs into (event type, payload) events.
//
// Takes already-decoded lines. Kept separate from the decoder so the decoder can be tested
// with hand-written events and this with hand-written bytes.
func ParseSSE(lines []string) []SSEEvent {
	events := []SSEEvent{}
	eventType := ""
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[6:])
			continue
		}
		if strings.HasPrefix(line, "data:") {
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
			name := eventType
			if name == "" {
				name = data.Str("type")
			}
			events = append(events, SSEEvent{Type: name, Data: data})
		}
	}
	return events
}

// ---------------------------------------------------------------------------
// Anthropic request -> OpenAI request  (an Anthropic client hitting /v1/messages)
// ---------------------------------------------------------------------------

// openAIContentFromAnthropic returns (content, tool calls, tool results) for one Anthropic
// message's blocks.
func openAIContentFromAnthropic(blocks any) (any, []any, []any) {
	parts := []any{}
	toolCalls := []any{}
	toolResults := []any{}
	items, _ := blocks.([]any)
	for _, item := range items {
		if text, ok := item.(string); ok {
			parts = append(parts, mapOf("type", "text", "text", text))
			continue
		}
		block, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		switch block.Str("type") {
		case "text":
			parts = append(parts, mapOf("type", "text", "text", block.Str("text")))
		case "image":
			source := block.Map("source")
			if source == nil {
				source = omap.New()
			}
			url := ""
			if source.Str("type") == "base64" {
				mediaType := source.Str("media_type")
				if mediaType == "" {
					mediaType = "image/png"
				}
				url = "data:" + mediaType + ";base64," + source.Str("data")
			} else {
				url = source.Str("url")
			}
			if url != "" {
				parts = append(parts, mapOf("type", "image_url", "image_url", mapOf("url", url)))
			}
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
		case "tool_result":
			toolResults = append(toolResults, mapOf(
				"role", "tool",
				"tool_call_id", block.Str("tool_use_id"),
				"content", TextOf(block.Value("content")),
			))
		}
	}
	if len(parts) == 1 {
		if only, ok := parts[0].(*omap.Map); ok && only.Str("type") == "text" {
			return only.Str("text"), toolCalls, toolResults
		}
	}
	return parts, toolCalls, toolResults
}

// AnthropicRequestToOpenAI converts an Anthropic Messages request body into the canonical
// OpenAI chat-completions payload.
//
// The `model` field is carried through unchanged: it is the caller's requested model and the
// router decides what to do with it exactly as it does for an OpenAI request.
func AnthropicRequestToOpenAI(body *omap.Map) *omap.Map {
	messages := []any{}
	if system := body.Value("system"); system != nil && TextOf(system) != "" {
		messages = append(messages, mapOf("role", "system", "content", TextOf(system)))
	}

	for _, item := range body.Slice("messages") {
		msg, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		role := msg.Str("role")
		if role == "" {
			role = "user"
		}
		content := msg.Value("content")
		if text, ok := content.(string); ok {
			messages = append(messages, mapOf("role", role, "content", text))
			continue
		}
		payloadContent, toolCalls, toolResults := openAIContentFromAnthropic(content)
		// Tool results have to precede the turn they belong to, because in OpenAI they are
		// their own messages answering the previous assistant turn's tool calls.
		messages = append(messages, toolResults...)
		if len(toolCalls) > 0 {
			entry := omap.New()
			entry.Set("role", "assistant")
			if isEmptyContent(payloadContent) {
				entry.Set("content", nil)
			} else {
				entry.Set("content", payloadContent)
			}
			entry.Set("tool_calls", toolCalls)
			messages = append(messages, entry)
		} else if !isEmptyContent(payloadContent) || len(toolResults) == 0 {
			messages = append(messages, mapOf("role", role, "content", payloadContent))
		}
	}

	payload := mapOf("model", body.Value("model"), "messages", messages)
	if maxTokens := body.Float("max_tokens", 0); maxTokens > 0 {
		payload.Set("max_tokens", intNumber(int(maxTokens)))
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value := body.Value(key); value != nil {
			payload.Set(key, value)
		}
	}
	if stop := body.Slice("stop_sequences"); len(stop) > 0 {
		payload.Set("stop", stop)
	}
	if body.Bool("stream", false) {
		payload.Set("stream", true)
	}
	tools := []any{}
	for _, item := range body.Slice("tools") {
		tool, ok := item.(*omap.Map)
		if !ok || tool.Str("name") == "" {
			continue
		}
		schema := tool.Value("input_schema")
		if schema == nil {
			schema = mapOf("type", "object")
		}
		tools = append(tools, mapOf("type", "function", "function", mapOf(
			"name", tool.Str("name"),
			"description", tool.Str("description"),
			"parameters", schema,
		)))
	}
	if len(tools) > 0 {
		payload.Set("tools", tools)
		if choice := body.Map("tool_choice"); choice != nil {
			switch choice.Str("type") {
			case "any":
				payload.Set("tool_choice", "required")
			case "none":
				payload.Set("tool_choice", "none")
			case "tool":
				if name := choice.Str("name"); name != "" {
					payload.Set("tool_choice", mapOf(
						"type", "function", "function", mapOf("name", name)))
				}
			}
		}
	}
	if metadata := body.Map("metadata"); metadata != nil {
		if user := metadata.Str("user_id"); user != "" {
			payload.Set("user", user)
		}
	}
	return payload
}

func isEmptyContent(content any) bool {
	switch t := content.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}

// ---------------------------------------------------------------------------
// OpenAI response -> Anthropic response  (answering an Anthropic client)
// ---------------------------------------------------------------------------

func OpenAIResponseToAnthropic(data *omap.Map, model string) *omap.Map {
	choice := firstChoice(data)
	message := choice.Map("message")
	if message == nil {
		message = omap.New()
	}
	blocks := []any{}
	text := message.Value("content")
	if _, ok := text.([]any); ok {
		text = TextOf(text)
	}
	if s := omap.AsString(text); s != "" {
		blocks = append(blocks, mapOf("type", "text", "text", s))
	}
	for _, item := range message.Slice("tool_calls") {
		call, ok := item.(*omap.Map)
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
	usage := data.Map("usage")
	if usage == nil {
		usage = omap.New()
	}
	finish := choice.Str("finish_reason")
	if finish == "" {
		finish = "stop"
	}
	stopReason := finishToStopReason[finish]
	if stopReason == "" {
		stopReason = "end_turn"
	}
	id := data.Str("id")
	if id == "" {
		id = NewID("msg_")
	}
	return mapOf(
		"id", id,
		"type", "message",
		"role", "assistant",
		"model", model,
		"content", blocks,
		"stop_reason", stopReason,
		"stop_sequence", nil,
		"usage", mapOf(
			"input_tokens", intNumber(int(usage.Float("prompt_tokens", 0))),
			"output_tokens", intNumber(int(usage.Float("completion_tokens", 0))),
		),
	)
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

// EventEncoder emits Anthropic SSE events from OpenAI chat.completion.chunk values.
//
// Stateful for the mirror of the decoder's reason: Anthropic requires every content block to
// be opened and closed, and requires a tool call's id and name up front, so the encoder has to
// track which blocks it has opened and close them in order at the end.
type EventEncoder struct {
	model        string
	id           string
	started      bool
	textOpen     bool
	nextIndex    int
	textIndex    int
	toolBlocks   map[int]int // OpenAI tool_calls index -> block index
	stopReason   string
	outputTokens int
	inputTokens  int
}

func NewEventEncoder(model, messageID string) *EventEncoder {
	if messageID == "" {
		messageID = NewID("msg_")
	}
	return &EventEncoder{
		model:      model,
		id:         messageID,
		toolBlocks: map[int]int{},
		stopReason: "end_turn",
	}
}

func event(name string, data *omap.Map) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		encoded = []byte("{}")
	}
	return "event: " + name + "\ndata: " + string(encoded) + "\n\n"
}

func (e *EventEncoder) Start(usage *omap.Map) string {
	e.started = true
	if usage != nil {
		e.inputTokens = int(usage.Float("prompt_tokens", 0))
	}
	return event("message_start", mapOf(
		"type", "message_start",
		"message", mapOf(
			"id", e.id,
			"type", "message",
			"role", "assistant",
			"model", e.model,
			"content", []any{},
			"stop_reason", nil,
			"stop_sequence", nil,
			"usage", mapOf("input_tokens", intNumber(e.inputTokens), "output_tokens", intNumber(0)),
		),
	))
}

// Feed takes one OpenAI chunk and returns zero or more Anthropic SSE frames.
func (e *EventEncoder) Feed(chunk *omap.Map) []string {
	frames := []string{}
	if !e.started {
		frames = append(frames, e.Start(chunk.Map("usage")))
	}
	choice := firstChoice(chunk)
	delta := choice.Map("delta")
	if delta == nil {
		delta = omap.New()
	}
	if content := delta.Str("content"); content != "" {
		if !e.textOpen {
			e.textOpen = true
			e.textIndex = e.nextIndex
			e.nextIndex++
			frames = append(frames, event("content_block_start", mapOf(
				"type", "content_block_start",
				"index", intNumber(e.textIndex),
				"content_block", mapOf("type", "text", "text", ""),
			)))
		}
		frames = append(frames, event("content_block_delta", mapOf(
			"type", "content_block_delta",
			"index", intNumber(e.textIndex),
			"delta", mapOf("type", "text_delta", "text", content),
		)))
	}
	for _, item := range delta.Slice("tool_calls") {
		call, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		toolIndex := call.Int("index", 0)
		fn := call.Map("function")
		if fn == nil {
			fn = omap.New()
		}
		if _, known := e.toolBlocks[toolIndex]; !known {
			// A tool call means no more text can be appended to the open text block, so it is
			// closed first: Anthropic blocks do not interleave.
			if e.textOpen {
				e.textOpen = false
				frames = append(frames, event("content_block_stop", mapOf(
					"type", "content_block_stop", "index", intNumber(e.textIndex))))
			}
			blockIndex := e.nextIndex
			e.nextIndex++
			e.toolBlocks[toolIndex] = blockIndex
			id := call.Str("id")
			if id == "" {
				id = NewID("toolu_")
			}
			frames = append(frames, event("content_block_start", mapOf(
				"type", "content_block_start",
				"index", intNumber(blockIndex),
				"content_block", mapOf(
					"type", "tool_use", "id", id, "name", fn.Str("name"), "input", omap.New()),
			)))
		}
		if args := fn.Str("arguments"); args != "" {
			frames = append(frames, event("content_block_delta", mapOf(
				"type", "content_block_delta",
				"index", intNumber(e.toolBlocks[toolIndex]),
				"delta", mapOf("type", "input_json_delta", "partial_json", args),
			)))
		}
	}
	if finish := choice.Str("finish_reason"); finish != "" {
		if mapped, ok := finishToStopReason[finish]; ok {
			e.stopReason = mapped
		} else {
			e.stopReason = "end_turn"
		}
	}
	if usage := chunk.Map("usage"); usage != nil {
		if v := int(usage.Float("completion_tokens", 0)); v != 0 {
			e.outputTokens = v
		}
		if v := int(usage.Float("prompt_tokens", 0)); v != 0 {
			e.inputTokens = v
		}
	}
	return frames
}

// Finish closes every open block and ends the message. Safe to call after zero chunks.
func (e *EventEncoder) Finish(usage *omap.Map) []string {
	frames := []string{}
	if !e.started {
		frames = append(frames, e.Start(usage))
	}
	if usage != nil {
		if v := int(usage.Float("completion_tokens", 0)); v != 0 {
			e.outputTokens = v
		}
		if v := int(usage.Float("prompt_tokens", 0)); v != 0 {
			e.inputTokens = v
		}
	}
	if e.textOpen {
		e.textOpen = false
		frames = append(frames, event("content_block_stop", mapOf(
			"type", "content_block_stop", "index", intNumber(e.textIndex))))
	}
	blocks := make([]int, 0, len(e.toolBlocks))
	for _, blockIndex := range e.toolBlocks {
		blocks = append(blocks, blockIndex)
	}
	sort.Ints(blocks)
	for _, blockIndex := range blocks {
		frames = append(frames, event("content_block_stop", mapOf(
			"type", "content_block_stop", "index", intNumber(blockIndex))))
	}
	e.toolBlocks = map[int]int{}
	frames = append(frames, event("message_delta", mapOf(
		"type", "message_delta",
		"delta", mapOf("stop_reason", e.stopReason, "stop_sequence", nil),
		"usage", mapOf("output_tokens", intNumber(e.outputTokens)),
	)))
	frames = append(frames, event("message_stop", mapOf("type", "message_stop")))
	return frames
}

// Error is an error frame, for a failure that happens after the stream has already opened.
func (e *EventEncoder) Error(message string) string {
	return event("error", mapOf(
		"type", "error",
		"error", mapOf("type", "api_error", "message", message),
	))
}
