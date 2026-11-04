package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// One decoded Anthropic Messages stream event
type anthEvent struct {
	Type    string
	Payload map[string]any
	Raw     string
}

// Source of decoded Messages stream events ending with io.EOF
type anthEventSource interface {
	Next() (*anthEvent, error)
}

// SSE backed event source skipping pings and keepalive frames
type anthSSEEvents struct {
	r       *sseReader
	headers http.Header
}

func newAnthSSEEvents(body io.Reader, headers http.Header) *anthSSEEvents {
	return &anthSSEEvents{r: newSSEReader(body), headers: headers}
}

// Next decodes the next event, io.EOF once the body is exhausted
func (s *anthSSEEvents) Next() (*anthEvent, error) {
	for {
		f, err := s.r.Next()
		if err != nil {
			return nil, err
		}
		if f.Event == "ping" {
			continue
		}
		if strings.TrimSpace(f.Data) == "" {
			if f.Event == "" {
				continue
			}
			return nil, providerStreamErrorFromNonJSON("", s.headers)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
			return nil, providerStreamErrorFromNonJSON(f.Data, s.headers)
		}
		typ := f.Event
		if typ == "" {
			typ = asStr(payload["type"])
		}
		if typ == "ping" {
			continue
		}
		return &anthEvent{Type: typ, Payload: payload, Raw: f.Data}, nil
	}
}

// Delta types appended onto a string field of the open block
var anthAppendDeltas = map[string]string{"text_delta": "text", "thinking_delta": "thinking", "signature_delta": "signature"}

// Tool input that stayed malformed JSON after the stream completed, worded as a provider stream parse error
type anthToolJSONError struct {
	Name   string
	Raw    string
	Detail string
}

func (e *anthToolJSONError) Error() string {
	return fmt.Sprintf("expected value at line 1 column 1: tool_use %q input is not valid JSON (%s)", e.Name, e.Detail)
}

// Event that arrived before message_start, the order the SDK refuses to accumulate
type anthEventOrderError struct{ Type string }

func (e *anthEventOrderError) Error() string {
	return fmt.Sprintf("Unexpected event order, got %s before \"message_start\"", e.Type)
}

// Rebuilds an Anthropic Message from stream events
type anthAccumulator struct {
	started     bool
	message     map[string]any
	blocks      map[int]map[string]any
	partialJSON map[int]string
	sawPartial  map[int]bool
}

func newAnthAccumulator() *anthAccumulator {
	return &anthAccumulator{message: map[string]any{}, blocks: map[int]map[string]any{}, partialJSON: map[int]string{}, sawPartial: map[int]bool{}}
}

// Folds one event into the message snapshot
func (a *anthAccumulator) observe(ev *anthEvent) {
	if ev == nil || ev.Payload == nil {
		return
	}
	p := ev.Payload
	switch ev.Type {
	case "message_start":
		a.started = true
		if m, ok := asMap(p["message"]); ok {
			for _, k := range []string{"id", "type", "role", "model", "usage"} {
				if v, has := m[k]; has {
					a.message[k] = v
				}
			}
		}
	case "content_block_start":
		index, ok := anyInt(p["index"])
		block, isMap := asMap(p["content_block"])
		if ok && isMap {
			copied := make(map[string]any, len(block))
			for k, v := range block {
				copied[k] = v
			}
			a.blocks[index] = copied
		}
	case "content_block_delta":
		index, ok := anyInt(p["index"])
		delta, isMap := asMap(p["delta"])
		if !ok || !isMap {
			return
		}
		block := a.blocks[index]
		if block == nil {
			block = map[string]any{}
			a.blocks[index] = block
		}
		deltaType := asStr(delta["type"])
		if field, has := anthAppendDeltas[deltaType]; has {
			block[field] = asStr(block[field]) + asStr(delta[field])
		} else if deltaType == "input_json_delta" {
			a.partialJSON[index] += asStr(delta["partial_json"])
			a.sawPartial[index] = true
		} else if deltaType == "citations_delta" {
			if cit, has := delta["citation"]; has {
				list, _ := asList(block["citations"])
				block["citations"] = append(list, cit)
			}
		}
	case "message_delta":
		if delta, ok := asMap(p["delta"]); ok {
			for _, k := range []string{"stop_reason", "stop_sequence", "stop_details"} {
				if v, has := delta[k]; has {
					a.message[k] = v
				}
			}
		}
		if usage, has := p["usage"]; has {
			current, curOK := asMap(a.message["usage"])
			incoming, inOK := asMap(usage)
			if curOK && inOK {
				merged := make(map[string]any, len(current)+len(incoming))
				for k, v := range current {
					merged[k] = v
				}
				for k, v := range incoming {
					if v != nil {
						merged[k] = v
					}
				}
				a.message["usage"] = merged
			} else {
				a.message["usage"] = usage
			}
		}
	}
}

// Sorted block indexes seen so far
func (a *anthAccumulator) indexes() []int {
	idx := make([]int, 0, len(a.blocks))
	for i := range a.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	return idx
}

// Assembled message with tool inputs decoded, strict parse failures reported as anthToolJSONError
func (a *anthAccumulator) finalize(strict bool) (map[string]any, error) {
	var firstErr error
	blocks := make([]any, 0, len(a.blocks))
	for _, i := range a.indexes() {
		src := a.blocks[i]
		block := make(map[string]any, len(src)+1)
		for k, v := range src {
			block[k] = v
		}
		if a.sawPartial[i] {
			raw := a.partialJSON[i]
			var decoded any
			if strings.TrimSpace(raw) == "" {
				decoded = map[string]any{}
			} else if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				if strict && firstErr == nil {
					firstErr = &anthToolJSONError{Name: asStr(block["name"]), Raw: raw, Detail: err.Error()}
				}
				decoded = map[string]any{}
			}
			block["input"] = decoded
		} else if asStr(block["type"]) == "tool_use" {
			if _, has := block["input"]; !has {
				block["input"] = map[string]any{}
			}
		}
		blocks = append(blocks, block)
	}
	out := make(map[string]any, len(a.message)+1)
	for k, v := range a.message {
		out[k] = v
	}
	out["content"] = blocks
	return out, firstErr
}

// Raises EmptyStreamError for a message the stream never completed
func anthCheckMessage(message map[string]any, toolDrop bool) error {
	content, _ := asList(message["content"])
	hasStop := message["stop_reason"] != nil
	if len(content) == 0 && !hasStop {
		return &EmptyStreamError{Message: "Provider returned an empty stream with no stop_reason (possible upstream error or malformed event stream)."}
	}
	if toolDrop && !hasStop {
		for _, b := range content {
			if blockType(b) == "tool_use" {
				return &EmptyStreamError{Message: "Stream ended with no stop_reason while a tool_use block was still incomplete; treating as a mid-tool-call stream drop (#80498)."}
			}
		}
	}
	return nil
}

// Streaming attempt state turning events into StreamEvents
type anthStreamState struct {
	c          *anthropicClient
	req        *Request
	fn         StreamFunc
	acc        *anthAccumulator
	hasToolUse bool
	sawEvent   bool
	sawStop    bool
	textWatch  *RunawayStreamWatch
	thinkWatch *RunawayStreamWatch
	runaway    string
	deltasSent bool
	toolSlots  map[int]int
	toolNames  []string
	headers    http.Header
	status     int
	started    time.Time
	estTokens  int
	lifetime   float64
}

func (s *anthStreamState) emit(ev StreamEvent) error {
	if s.fn == nil {
		return nil
	}
	return s.fn(ev)
}

// Folds one event into the snapshot and emits its deltas, true once message_stop or a runaway cut arrived
func (s *anthStreamState) handleEvent(ev *anthEvent) (bool, error) {
	s.sawEvent = true
	if !s.acc.started && ev.Type != "message_start" && ev.Type != "error" {
		return true, &anthEventOrderError{Type: ev.Type}
	}
	switch ev.Type {
	case "error":
		payload := map[string]any{}
		if em, ok := asMap(ev.Payload["error"]); ok {
			payload = em
		} else if ev.Payload["error"] != nil {
			payload["message"] = stringOf(ev.Payload["error"])
		}
		status := statusCodeFromPayload(ev.Payload)
		return true, &ProviderStreamError{StatusCode: status, Body: providerErrorBody(payload, status), RawText: ev.Raw, Headers: s.headers}
	case "message_stop":
		s.sawStop = true
		return true, nil
	case "content_block_start":
		s.acc.observe(ev)
		block, _ := asMap(ev.Payload["content_block"])
		if asStr(block["type"]) == "tool_use" {
			s.hasToolUse = true
			index, _ := anyInt(ev.Payload["index"])
			slot := len(s.toolSlots)
			s.toolSlots[index] = slot
			name := s.c.inboundToolName(asStr(block["name"]))
			if name != "" {
				s.toolNames = append(s.toolNames, name)
				if err := s.emit(StreamEvent{Kind: EventToolCallStart, Index: slot, ToolCall: &state.ToolCall{ID: asStr(block["id"]), Type: "function", Function: state.ToolCallFunction{Name: name}}}); err != nil {
					return true, err
				}
			}
		}
	case "content_block_delta":
		s.acc.observe(ev)
		delta, _ := asMap(ev.Payload["delta"])
		switch asStr(delta["type"]) {
		case "text_delta":
			text := asStr(delta["text"])
			if text != "" && !s.hasToolUse {
				s.deltasSent = true
				if err := s.emit(StreamEvent{Kind: EventTextDelta, Text: text}); err != nil {
					return true, err
				}
			}
			if s.textWatch.Feed(text) {
				s.runaway = "content"
				return true, nil
			}
		case "thinking_delta":
			thinking := asStr(delta["thinking"])
			if thinking != "" {
				s.deltasSent = true
				if err := s.emit(StreamEvent{Kind: EventReasoningDelta, Text: thinking}); err != nil {
					return true, err
				}
				if s.thinkWatch.Feed(thinking) {
					s.runaway = "reasoning"
					return true, nil
				}
			}
		case "input_json_delta":
			index, _ := anyInt(ev.Payload["index"])
			if slot, ok := s.toolSlots[index]; ok {
				frag := asStr(delta["partial_json"])
				block := s.acc.blocks[index]
				if frag != "" {
					if err := s.emit(StreamEvent{Kind: EventToolCallDelta, Index: slot, ToolCall: &state.ToolCall{ID: asStr(block["id"]), Type: "function", Function: state.ToolCallFunction{Name: s.c.inboundToolName(asStr(block["name"])), Arguments: frag}}}); err != nil {
						return true, err
					}
				}
			}
		}
	default:
		s.acc.observe(ev)
	}
	return false, nil
}

// Final response of a completed or runaway cut attempt with the trailing events emitted
func (s *anthStreamState) finish(latency time.Duration) (*Response, error) {
	if s.runaway == "" && !s.sawStop {
		if s.sawEvent {
			return nil, &EmptyStreamError{Message: "Anthropic Messages stream ended before message_stop (possible upstream stream drop)."}
		}
		return nil, &EmptyStreamError{Message: "Provider returned an empty stream with no events (possible upstream error or malformed event stream)."}
	}
	if s.runaway != "" {
		catalogLog.Warn(fmt.Sprintf("Runaway repetition detected in streamed %s; cutting the stream. model=%s", s.runaway, orUnknown2(s.c.route.Model)))
		message, _ := s.acc.finalize(false)
		message["stop_reason"] = "max_tokens"
		resp, err := s.c.normalizeResponse(message, s.status, s.headers, latency)
		if err != nil {
			return nil, err
		}
		anthMarkRaw(resp, ccRawRunaway, true)
		return resp, s.replayTail(resp)
	}
	message, jsonErr := s.acc.finalize(true)
	if jsonErr != nil {
		s.c.markToolInputBuffered()
		return nil, jsonErr
	}
	if err := anthCheckMessage(message, true); err != nil {
		return nil, err
	}
	resp, err := s.c.normalizeResponse(message, s.status, s.headers, latency)
	if err != nil {
		return nil, err
	}
	return resp, s.replayTail(resp)
}

// Emits the completed tool calls, usage and done marker of a finished attempt
func (s *anthStreamState) replayTail(resp *Response) error {
	for i := range resp.Message.ToolCalls {
		tc := resp.Message.ToolCalls[i]
		if err := s.emit(StreamEvent{Kind: EventToolCallDone, Index: i, ToolCall: &tc}); err != nil {
			return err
		}
	}
	usage := resp.Usage
	if err := s.emit(StreamEvent{Kind: EventUsage, Usage: &usage}); err != nil {
		return err
	}
	return s.emit(StreamEvent{Kind: EventDone})
}

// Sets one marker field inside the raw response document
func anthMarkRaw(resp *Response, key string, value any) {
	doc := map[string]any{}
	if len(resp.Raw) > 0 {
		_ = json.Unmarshal(resp.Raw, &doc)
	}
	doc[key] = value
	resp.Raw, _ = json.Marshal(doc)
}

// Reads events on a goroutine and drives the stale, read and lifetime watchdogs until the stream ends
func (c *anthropicClient) consumeStream(ctx context.Context, cancel context.CancelFunc, kill *ccKill, src anthEventSource, s *anthStreamState, t ccTimeouts) (*Response, error) {
	type eventMsg struct {
		ev  *anthEvent
		err error
	}
	events := make(chan eventMsg, 32)
	go func() {
		for {
			ev, err := src.Next()
			select {
			case events <- eventMsg{ev: ev, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	staleTimer := time.NewTimer(t.stale)
	readTimer := time.NewTimer(t.read)
	defer staleTimer.Stop()
	defer readTimer.Stop()
	var lifetimeC <-chan time.Time
	if t.lifetime > 0 {
		lifetimeTimer := time.NewTimer(t.lifetime)
		defer lifetimeTimer.Stop()
		lifetimeC = lifetimeTimer.C
	}
	lastChunk := s.started
	model := orUnknown2(s.req.Model)
	for {
		select {
		case <-ctx.Done():
			return nil, c.attemptError(ctx, kill, ctx.Err(), s)
		case <-staleTimer.C:
			elapsed := time.Since(lastChunk).Seconds()
			catalogLog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.", elapsed, t.stale.Seconds(), model, commaInt(s.estTokens)))
			_ = s.emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ No response from provider for %ds (model: %s, context: ~%s tokens). Reconnecting...", int(elapsed), model, commaInt(s.estTokens))})
			kill.fire("stale", elapsed)
			cancel()
			return nil, c.attemptError(ctx, kill, errors.New("stale stream"), s)
		case <-readTimer.C:
			kill.fire("read", t.read.Seconds())
			cancel()
			return nil, c.attemptError(ctx, kill, errors.New("read timeout"), s)
		case <-lifetimeC:
			elapsed := time.Since(s.started).Seconds()
			catalogLog.Warn(fmt.Sprintf("Stream still running after %.0fs (stream_max_lifetime %.0fs) — killing connection. model=%s", elapsed, t.lifetime.Seconds(), model))
			_ = s.emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ Response still streaming after %ds (stream_max_lifetime %ds, model: %s). Aborting...", int(elapsed), int(t.lifetime.Seconds()), model)})
			kill.fire("lifetime", elapsed)
			cancel()
			return nil, c.attemptError(ctx, kill, errors.New("stream lifetime exceeded"), s)
		case m := <-events:
			if m.err != nil {
				if errors.Is(m.err, io.EOF) {
					return s.finish(time.Since(s.started))
				}
				var pse *ProviderStreamError
				if errors.As(m.err, &pse) {
					return nil, c.streamError(pse)
				}
				return nil, c.attemptError(ctx, kill, m.err, s)
			}
			now := time.Now()
			lastChunk = now
			resetTimer(readTimer, t.read)
			resetTimer(staleTimer, t.stale)
			stop, err := s.handleEvent(m.ev)
			if err != nil {
				cancel()
				var pse *ProviderStreamError
				if errors.As(err, &pse) {
					return nil, c.streamError(pse)
				}
				return nil, err
			}
			if stop {
				cancel()
				return s.finish(time.Since(s.started))
			}
		}
	}
}

// APIError for a provider error encoded inside the stream, keeping the synthetic code and headers
func (c *anthropicClient) streamError(pse *ProviderStreamError) error {
	apiErr := Classify(c.route.Provider, c.route.Model, pse.StatusCode, pse.BodyText(), pse)
	if code := pse.Code(); code != "" {
		apiErr.Code = code
	}
	if apiErr.Message == "" {
		apiErr.Message = pse.Error()
	}
	if pse.Headers != nil && apiErr.RetryAfter == 0 {
		apiErr.RetryAfter = RetryAfterFrom(pse.Headers.Get("Retry-After"))
	}
	return apiErr
}

// Error of a cancelled or failed attempt, naming the watchdog that cut it
func (c *anthropicClient) attemptError(ctx context.Context, kill *ccKill, err error, s *anthStreamState) error {
	reason, elapsed := kill.get()
	var headers http.Header
	if s != nil {
		headers = s.headers
	}
	var base error
	switch reason {
	case "stale":
		base = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: fmt.Errorf("Stream timed out: no chunks received for %.0fs (stale stream threshold, model: %s). Connection killed.", elapsed, orUnknown2(c.route.Model)), headers: headers}}
	case "lifetime":
		capSeconds := 0.0
		if s != nil {
			capSeconds = s.lifetime
		}
		base = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: &StreamLifetimeExceededError{Elapsed: elapsed, Cap: capSeconds}, headers: headers}}
	case "read":
		base = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: fmt.Errorf("Read timed out after %.0fs waiting for the provider stream.", elapsed), headers: headers}}
	default:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		base = ccWrapTransport(err, TimeoutPhaseRead)
		if headers != nil {
			var tpe *TimeoutPhaseError
			if errors.As(base, &tpe) && tpe == base {
				base = &TimeoutPhaseError{Phase: tpe.Phase, Err: &ccHeadersError{err: tpe.Err, headers: headers}}
			} else {
				base = &ccHeadersError{err: base, headers: headers}
			}
		}
	}
	return Classify(c.route.Provider, c.route.Model, 0, "", base)
}
