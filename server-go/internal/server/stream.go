package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/upstream"
	"github.com/satomic/model-router/server-go/internal/wire"
)

// chunkSource yields canonical OpenAI chunks. Next returns (chunk, ok, error); ok is false at
// the end of the stream. Close releases the upstream connection.
type chunkSource interface {
	Next() (*omap.Map, bool, error)
	Close()
}

// sliceSource is a stream of already-materialised chunks, for an upstream that cannot stream.
type sliceSource struct {
	items []*omap.Map
	index int
}

func (s *sliceSource) Next() (*omap.Map, bool, error) {
	if s.index >= len(s.items) {
		return nil, false, nil
	}
	item := s.items[s.index]
	s.index++
	return item, true, nil
}

func (s *sliceSource) Close() {}

// chunksFromCompletion is a one-chunk stream, for an upstream that cannot stream (the Responses
// API path, and the AI-credit gate's own note).
func chunksFromCompletion(completion *omap.Map) chunkSource {
	choice := firstChoice(completion)
	message := choice.Map("message")
	if message == nil {
		message = omap.New()
	}
	finish := choice.Value("finish_reason")
	if finish == nil || omap.AsString(finish) == "" {
		finish = "stop"
	}
	return &sliceSource{items: []*omap.Map{mapOf(
		"id", completion.Value("id"),
		"object", "chat.completion.chunk",
		"created", completion.Value("created"),
		"model", completion.Value("model"),
		"choices", []any{mapOf(
			"index", json.Number("0"),
			"delta", mapOf("role", "assistant", "content", message.Value("content")),
			"finish_reason", finish,
		)},
		"usage", nilIfEmptyMap(completion.Map("usage")),
	)}}
}

// openAIStreamSource relays an OpenAI-protocol SSE stream unchanged: the chunks already are the
// canonical form.
type openAIStreamSource struct{ stream *upstream.Stream }

func (s *openAIStreamSource) Next() (*omap.Map, bool, error) {
	_, data, ok, err := s.stream.NextEvent()
	return data, ok, err
}

func (s *openAIStreamSource) Close() { s.stream.Close() }

// openOpenAIStream opens an OpenAI-protocol stream.
//
// The response status is checked inside OpenStream rather than on the first chunk, so an
// upstream rejection (a bad key, an unknown deployment) still becomes a 502 with a message,
// instead of a 200 whose body dies on its first chunk.
func openOpenAIStream(ctx context.Context, client *upstream.Client, model string, body *omap.Map) (chunkSource, error) {
	stream, err := client.OpenStream(ctx, model, body)
	if err != nil {
		return nil, err
	}
	return &openAIStreamSource{stream: stream}, nil
}

// anthropicStreamSource decodes an Anthropic upstream's events into canonical chunks.
type anthropicStreamSource struct {
	stream  *upstream.Stream
	decoder *wire.StreamDecoder
	pending []*omap.Map
}

func (s *anthropicStreamSource) Next() (*omap.Map, bool, error) {
	for {
		if len(s.pending) > 0 {
			item := s.pending[0]
			s.pending = s.pending[1:]
			return item, true, nil
		}
		event, data, ok, err := s.stream.NextEvent()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		chunks, err := s.decoder.Feed(event, data)
		if err != nil {
			return nil, false, err
		}
		s.pending = chunks
	}
}

func (s *anthropicStreamSource) Close() { s.stream.Close() }

// openAnthropicStream opens an Anthropic upstream stream, with the same up-front status check as
// the OpenAI path so an upstream error is a 502 rather than a truncated stream.
func (a *App) openAnthropicStream(ctx context.Context, client *upstream.Client, body *omap.Map, model string) (chunkSource, error) {
	request := body.Clone()
	request.Set("stream", true)
	stream, err := client.OpenStream(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return &anthropicStreamSource{
		stream:  stream,
		decoder: wire.NewStreamDecoder(model, "", time.Now().Unix()),
	}, nil
}

// accumulator rebuilds the whole answer from canonical chunks, for the trace record.
//
// Tool calls stream in fragments: the name arrives on the first delta for an index and the
// arguments accumulate over later ones, so they are assembled by index rather than taken from
// any single chunk.
type accumulator struct {
	parts  []string
	finish any
	usage  *omap.Map
	calls  map[int]*omap.Map
}

func newAccumulator() *accumulator {
	return &accumulator{calls: map[int]*omap.Map{}}
}

func (acc *accumulator) feed(chunk *omap.Map) {
	choices := chunk.Slice("choices")
	if len(choices) > 0 {
		if choice, ok := choices[0].(*omap.Map); ok {
			delta := choice.Map("delta")
			if delta == nil {
				delta = omap.New()
			}
			if content := delta.Str("content"); content != "" {
				acc.parts = append(acc.parts, content)
			}
			for _, item := range delta.Slice("tool_calls") {
				call, ok := item.(*omap.Map)
				if !ok {
					continue
				}
				index := call.Int("index", 0)
				slot := acc.calls[index]
				if slot == nil {
					slot = mapOf("id", nil, "type", "function",
						"function", mapOf("name", nil, "arguments", ""))
					acc.calls[index] = slot
				}
				if id := call.Value("id"); id != nil && omap.AsString(id) != "" {
					slot.Set("id", id)
				}
				if kind := call.Value("type"); kind != nil && omap.AsString(kind) != "" {
					slot.Set("type", kind)
				}
				fn := call.Map("function")
				if fn == nil {
					continue
				}
				slotFn := slot.Map("function")
				if name := fn.Value("name"); name != nil && omap.AsString(name) != "" {
					slotFn.Set("name", name)
				}
				if arguments := fn.Str("arguments"); arguments != "" {
					slotFn.Set("arguments", slotFn.Str("arguments")+arguments)
				}
			}
			if finish := choice.Value("finish_reason"); finish != nil && omap.AsString(finish) != "" {
				acc.finish = finish
			}
		}
	}
	if usage := chunk.Map("usage"); usage != nil && usage.Len() > 0 {
		acc.usage = usage
	}
}

func (acc *accumulator) content() string {
	var out string
	for _, part := range acc.parts {
		out += part
	}
	return out
}

func (acc *accumulator) toolCalls() []any {
	if len(acc.calls) == 0 {
		return nil
	}
	indices := make([]int, 0, len(acc.calls))
	for index := range acc.calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	out := make([]any, 0, len(indices))
	for _, index := range indices {
		out = append(out, acc.calls[index])
	}
	return out
}

// streamResponse relays canonical chunks to the caller in whichever protocol they spoke.
//
// The response headers go out before the first chunk is read, so the caller sees a 200 and the
// routing headers immediately; a failure after that point can no longer change the status, which
// is why each protocol has its own way of ending a broken stream.
func (a *App) streamResponse(w http.ResponseWriter, r *http.Request, ctx *callContext, chunks chunkSource) error {
	defer chunks.Close()

	for key, value := range ctx.Headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer an SSE body turn a streamed answer into one that arrives all at once.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	acc := newAccumulator()
	if ctx.ClientProtocol == "anthropic" {
		return a.streamAnthropic(w, r, ctx, chunks, acc, flush)
	}
	return a.streamOpenAI(w, r, ctx, chunks, acc, flush)
}

// streamOpenAI relays canonical chunks as an OpenAI SSE stream.
func (a *App) streamOpenAI(w http.ResponseWriter, r *http.Request, ctx *callContext,
	chunks chunkSource, acc *accumulator, flush func()) error {
	for {
		chunk, ok, err := chunks.Next()
		if err != nil {
			a.finalizeTrace(ctx.Trace, ctx.Start, "error", nil, nil, nil, err.Error(), nil)
			// The status is already sent, so the only honest ending is to stop writing: an
			// OpenAI client reads a stream that ends without [DONE] as incomplete.
			return nil
		}
		if !ok {
			break
		}
		acc.feed(chunk)
		encoded, err := marshalJSON(chunk)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			// The caller hung up. The turn is still recorded: it really did cost an upstream call.
			a.finalizeTrace(ctx.Trace, ctx.Start, "error", nil, nil, nil, err.Error(), nil)
			return nil
		}
		flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
	a.finalizeTrace(ctx.Trace, ctx.Start, "ok",
		acc.content(), acc.usage, acc.finish, "", acc.toolCalls())
	return nil
}

// streamAnthropic relays canonical chunks as an Anthropic SSE stream.
//
// A mid-stream failure is emitted as the protocol's own `error` event rather than by cutting the
// body: the response status is already sent, and Anthropic clients do surface this event to the
// user.
func (a *App) streamAnthropic(w http.ResponseWriter, r *http.Request, ctx *callContext,
	chunks chunkSource, acc *accumulator, flush func()) error {
	encoder := wire.NewEventEncoder(ctx.Model, "")
	write := func(frames []string) bool {
		for _, frame := range frames {
			if _, err := fmt.Fprint(w, frame); err != nil {
				return false
			}
		}
		flush()
		return true
	}
	for {
		chunk, ok, err := chunks.Next()
		if err != nil {
			a.finalizeTrace(ctx.Trace, ctx.Start, "error", nil, nil, nil, err.Error(), nil)
			write([]string{encoder.Error(err.Error())})
			return nil
		}
		if !ok {
			break
		}
		acc.feed(chunk)
		if !write(encoder.Feed(chunk)) {
			a.finalizeTrace(ctx.Trace, ctx.Start, "error", nil, nil, nil, "the client closed the connection", nil)
			return nil
		}
	}
	write(encoder.Finish(acc.usage))
	a.finalizeTrace(ctx.Trace, ctx.Start, "ok",
		acc.content(), acc.usage, acc.finish, "", acc.toolCalls())
	return nil
}
