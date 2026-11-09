package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Attempt error already shaped for the retry loop, returned by the consume loop as it is
type ccFinalError struct{ err error }

func (e *ccFinalError) Error() string { return e.err.Error() }

func (e *ccFinalError) Unwrap() error { return e.err }

// Source of SSE frames a chat completions consume loop reads until io.EOF
type ccFrameSource interface {
	Next() (*sseFrame, error)
}

// Source a consume loop hands back once its reader goroutine stops reading
type frameSourceReleaser interface{ release() }

// Releases the source on the goroutine that read it when the source owns a managed stream
func releaseFrameSource(src any) {
	if r, ok := src.(frameSourceReleaser); ok {
		r.release()
	}
}

// True while token still owns the agent delta sink, warning once a newer stream claimed the writer slot
func writerStillCurrent(fence any, token int, label, model string) bool {
	if token == 0 || StreamWriterIsCurrent(fence, token) {
		return true
	}
	catalogLog.Warn(fmt.Sprintf("%s attempt superseded by a newer stream; stopping consumption to preserve the single-writer invariant (model=%s).", label, model))
	return false
}

// Claims the agent delta sink for this attempt once its response headers arrived
func (s *ccStreamState) claimWriter(ctx context.Context) {
	s.fence = streamCallFrom(ctx).hooks.fence
	s.writerToken = ClaimStreamWriter(s.fence)
}

// True while the attempt owns the delta sink, a marker only finish chunk always passes
func (s *ccStreamState) writerStillCurrent(chunk map[string]any) bool {
	return ccMarkerOnlyFinish(chunk) || writerStillCurrent(s.fence, s.writerToken, "Streaming", s.req.Model)
}

// Claims the agent delta sink for this Messages attempt once its response headers arrived
func (s *anthStreamState) claimWriter(ctx context.Context) {
	s.fence = streamCallFrom(ctx).hooks.fence
	s.writerToken = ClaimStreamWriter(s.fence)
}

// True for a chunk carrying a finish reason and no content, tool call or reasoning delta
func ccMarkerOnlyFinish(chunk map[string]any) bool {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	if !pyTruthy(choice["finish_reason"]) {
		return false
	}
	delta, _ := choice["delta"].(map[string]any)
	for _, key := range []string{"content", "tool_calls", "reasoning_content", "reasoning"} {
		if pyTruthy(delta[key]) {
			return false
		}
	}
	return true
}

// Rebuilds a chat completion from the post intercept chunk dicts Relay records
type relayChatAccumulator struct {
	mu               sync.Mutex
	content          []string
	reasoning        []string
	refusal          []string
	reasoningDetails []any
	tools            *ccToolCallAccumulator
	model            any
	usage            any
	finishReason     any
	role             any
}

func newRelayChatAccumulator() *relayChatAccumulator {
	return &relayChatAccumulator{tools: newCCToolCallAccumulator(), role: "assistant"}
}

// RelayChatAccumulator folds relayed chat completion chunks into one response payload
type RelayChatAccumulator = relayChatAccumulator

// NewRelayChatAccumulator is an empty accumulator ready to observe relayed chunks
func NewRelayChatAccumulator() *RelayChatAccumulator { return newRelayChatAccumulator() }

// Python list extend items of a value, a dict yielding its keys and a string its characters
func relayExtendItems(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, k)
		}
		return out
	}
	return nil
}

// Folds one chunk dict into the recorded message, ignoring anything that is not a dict
func (a *relayChatAccumulator) Observe(chunk any) {
	m, ok := chunk.(map[string]any)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if pyTruthy(m["model"]) {
		a.model = m["model"]
	}
	if pyTruthy(m["usage"]) {
		a.usage = m["usage"]
	}
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	if pyTruthy(choice["finish_reason"]) {
		a.finishReason = choice["finish_reason"]
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return
	}
	if pyTruthy(delta["role"]) {
		a.role = delta["role"]
	}
	if text := flattenMessageText(delta["content"], ""); text != "" {
		a.content = append(a.content, text)
	}
	if pyTruthy(delta["reasoning_details"]) {
		a.reasoningDetails = append(a.reasoningDetails, relayExtendItems(delta["reasoning_details"])...)
	}
	reasoning := delta["reasoning_content"]
	if !pyTruthy(reasoning) {
		reasoning = delta["reasoning"]
	}
	if pyTruthy(reasoning) {
		previous := ""
		if n := len(a.reasoning); n > 0 {
			previous = a.reasoning[n-1]
		}
		a.reasoning = append(a.reasoning, separateGluedReasoningBlocks(previous, reasoning))
	}
	if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
		a.refusal = append(a.refusal, refusal)
	}
	if pyTruthy(delta["tool_calls"]) {
		for _, tc := range relayExtendItems(delta["tool_calls"]) {
			if tcm, ok := tc.(map[string]any); ok {
				a.tools.Feed(tcm)
			}
		}
	}
}

// Joined parts, nil when they join to nothing
func relayJoinedOrNil(parts []string) any {
	if joined := strings.Join(parts, ""); joined != "" {
		return joined
	}
	return nil
}

// Chat completion dict of everything observed, finish reason stop when none arrived
func (a *relayChatAccumulator) Finalize() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	acc := a.tools.Materialize()
	var calls []any
	for _, idx := range a.tools.Slots() {
		e := acc[idx]
		call := map[string]any{"id": e.ID, "type": e.Type, "function": map[string]any{"name": e.Name, "arguments": e.Arguments}, "extra_content": e.ExtraContent}
		if e.ThoughtSignature != "" {
			call["thought_signature"] = e.ThoughtSignature
		}
		calls = append(calls, call)
	}
	message := map[string]any{"role": a.role, "content": relayJoinedOrNil(a.content), "reasoning_content": relayJoinedOrNil(a.reasoning),
		"refusal": relayJoinedOrNil(a.refusal), "reasoning_details": nil, "tool_calls": nil}
	if len(a.reasoningDetails) > 0 {
		message["reasoning_details"] = a.reasoningDetails
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := a.finishReason
	if !pyTruthy(finish) {
		finish = "stop"
	}
	return map[string]any{"model": a.model, "usage": a.usage, "choices": []any{map[string]any{"message": message, "finish_reason": finish}}}
}

// Completed response body a streaming request returned in place of an event stream
type relayCompletedBody struct {
	payload map[string]any
	status  int
	headers http.Header
}

// One Relay managed provider stream, opened and closed on the consume loop's reader goroutine
type managedRelaySource struct {
	ctx      context.Context
	open     func() (*ManagedLlmStream, error)
	stream   *ManagedLlmStream
	mu       sync.Mutex
	ended    bool
	final    any
	modified bool
}

// Next chunk of the managed stream, recording its final response and rewrite flag at the end
func (m *managedRelaySource) pull() (any, error) {
	if m.stream == nil {
		stream, err := m.open()
		if err != nil {
			return nil, err
		}
		m.stream = stream
	}
	chunk, err := m.stream.Next(m.ctx)
	m.mu.Lock()
	m.modified = m.stream.OutputModified
	if errors.Is(err, io.EOF) {
		m.ended, m.final = true, m.stream.FinalResponse
	}
	m.mu.Unlock()
	return chunk, err
}

// Closes the managed stream, a cleanup failure only reaching the debug log
func (m *managedRelaySource) release() {
	if m.stream == nil {
		return
	}
	if err := m.stream.Close(); err != nil {
		slog.Debug("Managed provider stream cleanup failed", "error", err)
	}
}

// Final response and rewrite flag of a stream that reached its end
func (m *managedRelaySource) outcome() (bool, any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ended, m.final, m.modified
}

// Sends a liveness beat to the consume loop without ever blocking the provider read
func relayBeat(beat chan<- bool, live bool) {
	select {
	case beat <- live:
	default:
	}
}

// Managed chat completions attempt of the consume loop, the Relay stream and its recorder
type ccManagedRelay struct {
	src *managedRelaySource
	acc *relayChatAccumulator
}

// Next post intercept chunk as an SSE data frame for the consume loop
func (r *ccManagedRelay) Next() (*sseFrame, error) {
	chunk, err := r.src.pull()
	if err != nil {
		return nil, err
	}
	payload, ok := tools.RelayJSONable(chunk).(map[string]any)
	if !ok {
		return nil, &ccFinalError{err: fmt.Errorf("'%s' object has no attribute 'choices'", pyrepr.TypeName(chunk))}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, &ccFinalError{err: err}
	}
	return &sseFrame{Data: string(data)}, nil
}

func (r *ccManagedRelay) release() { r.src.release() }

// Raw chat completions chunks of one provider response, decoded the way the consume loop decodes them
type ccRelayChunks struct {
	c       *chatCompletionsClient
	r       *sseReader
	body    io.Closer
	headers http.Header
	beat    chan<- bool
	ctx     context.Context
	kill    *ccKill
	s       *ccStreamState
}

func (r *ccRelayChunks) Next(context.Context) (any, error) {
	for {
		f, err := r.r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, &ccFinalError{err: r.c.attemptError(r.ctx, r.kill, err, r.s)}
		}
		relayBeat(r.beat, false)
		chunk, skip, err := r.c.decodeFrame(f, r.headers)
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if err != nil {
			return nil, &ccFinalError{err: err}
		}
		if !skip {
			return chunk, nil
		}
	}
}

func (r *ccRelayChunks) Close() error { return r.body.Close() }

// Response of the attempt the Relay stream completed without chunks, switching the session to non streaming
func (s *ccStreamState) adoptRelayFinal(latency time.Duration) (*Response, bool, error) {
	if s.relay == nil {
		return nil, false, nil
	}
	ended, final, _ := s.relay.src.outcome()
	if !ended || final == nil {
		return nil, false, nil
	}
	c := s.c
	catalogLog.Info(fmt.Sprintf("Streaming request returned a final response object instead of an iterator; switching %s/%s to non-streaming for this session.", orUnknown2(c.route.Provider), orUnknown2(c.route.Model)))
	c.mu.Lock()
	c.nonStreaming = true
	c.mu.Unlock()
	payload, status, headers := relayJSONableDict(final), s.status, s.headers
	if body, ok := final.(*relayCompletedBody); ok {
		payload, status, headers = body.payload, body.status, body.headers
	}
	resp, err := c.normalizeResponse(payload, status, headers, latency)
	if err != nil {
		return nil, true, err
	}
	return resp, true, c.replay(resp, s.fn)
}

// Chat completions attempt under an active Relay turn, the provider stream managed and recorded by Relay
func chatManagedRelayStream(ctx context.Context, c *chatCompletionsClient, req *Request, p *ccPrepared, fn StreamFunc) (*Response, bool, error) {
	t := c.streamTimeouts(req, p.settings, p.estTokens)
	s := c.newStreamState(ctx, req, p, fn, t)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	kill := &ccKill{}
	hooks := streamCallFrom(ctx).hooks
	identity := hooks.relayIdentity(c.route, "provider")
	beat := make(chan bool, 16)
	var writerToken atomic.Int64
	acc := newRelayChatAccumulator()
	factory := func(_ context.Context, request map[string]any) (any, error) {
		body, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		q := *p
		q.body, q.wire = body, request
		httpReq, err := c.newRequest(attemptCtx, &q)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient(t.connect).Do(httpReq)
		if err != nil {
			return nil, &ccFinalError{err: c.attemptError(attemptCtx, kill, err, s)}
		}
		s.headers, s.status = resp.Header.Clone(), resp.StatusCode
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := readLimited(resp.Body, ccErrorBodyLimit)
			_ = resp.Body.Close()
			return nil, &ccFinalError{err: c.httpError(resp, data)}
		}
		StreamDiagCaptureResponse(s.diag, resp)
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return nil, &ccFinalError{err: c.attemptError(attemptCtx, kill, err, s)}
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				return nil, &ccFinalError{err: c.streamError(providerStreamErrorFromNonJSON(string(data), s.headers))}
			}
			if eo, ok := payload["error"]; ok && eo != nil {
				if em, ok := eo.(map[string]any); !ok || len(em) > 0 {
					return nil, &ccFinalError{err: c.httpError(resp, data)}
				}
			}
			return &relayCompletedBody{payload: payload, status: resp.StatusCode, headers: s.headers}, nil
		}
		return &ccRelayChunks{c: c, r: newSSEReader(resp.Body), body: resp.Body, headers: s.headers, beat: beat, ctx: attemptCtx, kill: kill, s: s}, nil
	}
	src := &managedRelaySource{ctx: attemptCtx}
	src.open = func() (*ManagedLlmStream, error) {
		request := make(map[string]any, len(p.wire))
		for k, v := range p.wire {
			request[k] = v
		}
		return NewManagedLlmStream(attemptCtx, request, factory, ManagedLlmStreamOptions{
			SessionID: identity.SessionID, Name: identity.Name, ModelName: identity.ModelName,
			Finalizer: func(context.Context) (any, error) { return acc.Finalize(), nil },
			OnStreamCreated: func(context.Context, any) error {
				writerToken.Store(int64(ClaimStreamWriter(hooks.fence)))
				if attemptCtx.Err() != nil {
					return &ccFinalError{err: c.attemptError(attemptCtx, kill, context.Cause(attemptCtx), s)}
				}
				return nil
			},
			OnChunk:      func(_ context.Context, chunk any) error { acc.Observe(chunk); return nil },
			ChunkAdapter: func(chunk any) any { return chunk },
			AcceptChunk: func(chunk any) bool {
				m, _ := chunk.(map[string]any)
				if ccMarkerOnlyFinish(m) {
					return true
				}
				if attemptCtx.Err() != nil || !writerStillCurrent(hooks.fence, int(writerToken.Load()), "Streaming", req.Model) {
					return false
				}
				relayBeat(beat, true)
				return true
			},
			CompletedResponsePredicate: func(value any) bool {
				body, ok := value.(*relayCompletedBody)
				return ok && body != nil
			},
			Metadata: identity.metadata("chat_completions"), DeferLogicalCompletion: true,
		})
	}
	s.relay = &ccManagedRelay{src: src, acc: acc}
	out, err := c.consumeStream(ctx, attemptCtx, cancel, kill, s.relay, beat, s, t)
	return out, s.deltasSent, err
}

// Managed Messages attempt of the consume loop, the Relay stream, the provider snapshot and the recorder
type anthManagedRelay struct {
	src  *managedRelaySource
	mu   sync.Mutex
	snap *anthAccumulator
	acc  *AnthropicStreamAccumulator
}

// Next post intercept event for the consume loop
func (r *anthManagedRelay) Next() (*anthEvent, error) {
	chunk, err := r.src.pull()
	if err != nil {
		return nil, err
	}
	payload := relayJSONableDict(chunk)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &ccFinalError{err: err}
	}
	return &anthEvent{Type: asStr(payload["type"]), Payload: payload, Raw: string(raw)}, nil
}

func (r *anthManagedRelay) release() { r.src.release() }

// Raw Messages events of one provider response, folded into the provider snapshot before Relay sees them
type anthRelayChunks struct {
	r    *anthManagedRelay
	src  anthEventSource
	body io.Closer
	c    *anthropicClient
	ctx  context.Context
	kill *ccKill
	s    *anthStreamState
}

func (r *anthRelayChunks) Next(context.Context) (any, error) {
	ev, err := r.src.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		var pse *ProviderStreamError
		if errors.As(err, &pse) {
			return nil, &ccFinalError{err: r.c.streamError(pse)}
		}
		return nil, &ccFinalError{err: r.c.attemptError(r.ctx, r.kill, err, r.s)}
	}
	if ev.Type != "error" && ev.Type != "message_stop" && (r.r.snap.started || ev.Type == "message_start") {
		r.r.mu.Lock()
		r.r.snap.observe(ev)
		r.r.mu.Unlock()
	}
	payload := ev.Payload
	if _, has := payload["type"]; !has {
		payload = relayCopyMap(payload)
		payload["type"] = ev.Type
	}
	return payload, nil
}

func (r *anthRelayChunks) Close() error { return r.body.Close() }

// Message a runaway cut attempt keeps, the streamed snapshot under any Relay rewrite
func (s *anthStreamState) runawayMessage() map[string]any {
	if s.relay == nil {
		message, _ := s.acc.finalize(false)
		return message
	}
	s.relay.mu.Lock()
	defer s.relay.mu.Unlock()
	snapshot, _ := s.relay.snap.finalize(false)
	return relayJSONableDict(s.relay.acc.Response(snapshot))
}

// Checked final message of a completed attempt, the Relay rewrite merged over the provider message when Relay changed the output
func (s *anthStreamState) finalMessage() (map[string]any, error) {
	if s.relay == nil {
		message, jsonErr := s.acc.finalize(true)
		if jsonErr != nil {
			s.c.markToolInputBuffered()
			return nil, jsonErr
		}
		return message, anthCheckMessage(message, true)
	}
	_, _, modified := s.relay.src.outcome()
	s.relay.mu.Lock()
	defer s.relay.mu.Unlock()
	base, jsonErr := s.relay.snap.finalize(true)
	if jsonErr != nil {
		s.c.markToolInputBuffered()
		return nil, jsonErr
	}
	if details := s.relay.acc.Finalize()["stop_details"]; details != nil && base["stop_details"] == nil {
		base["stop_details"] = details
	}
	if err := anthCheckMessage(base, false); err != nil {
		return nil, err
	}
	message := base
	if modified {
		message = relayJSONableDict(s.relay.acc.Response(base))
	}
	return message, anthCheckMessage(message, true)
}

// Response of the attempt the Relay stream completed without events, switching the session to non streaming
func (s *anthStreamState) adoptRelayFinal(latency time.Duration) (*Response, bool, error) {
	if s.relay == nil {
		return nil, false, nil
	}
	ended, final, _ := s.relay.src.outcome()
	if !ended || final == nil {
		return nil, false, nil
	}
	c := s.c
	catalogLog.Info(fmt.Sprintf("Streaming request returned a final message object instead of an event stream; switching %s/%s to non-streaming for this session.", orUnknown2(c.route.Provider), orUnknown2(c.route.Model)))
	c.mu.Lock()
	c.nonStreaming = true
	c.mu.Unlock()
	payload, status, headers := relayJSONableDict(final), s.status, s.headers
	if body, ok := final.(*relayCompletedBody); ok {
		payload, status, headers = body.payload, body.status, body.headers
	}
	resp, err := c.normalizeResponse(payload, status, headers, latency)
	if err != nil {
		return nil, true, err
	}
	return resp, true, c.replay(resp, s.fn)
}

// Messages attempt under an active Relay turn, the provider stream managed and recorded by Relay
func anthropicManagedRelayStream(ctx context.Context, c *anthropicClient, req *Request, p *anthPrepared, fn StreamFunc) (*Response, bool, error) {
	t := c.streamTimeouts(req, p.settings, p.estTokens)
	s := c.newStreamState(ctx, req, p, fn, t)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	kill := &ccKill{}
	hooks := streamCallFrom(ctx).hooks
	identity := hooks.relayIdentity(c.route, "anthropic")
	var writerToken atomic.Int64
	relay := &anthManagedRelay{snap: newAnthAccumulator(), acc: NewAnthropicStreamAccumulator()}
	factory := func(_ context.Context, request map[string]any) (any, error) {
		body := relayCopyMap(request)
		anthSanitizeKwargs(body, "")
		httpReq, err := c.transport.Request(attemptCtx, anthOpMessages, body, p.headers)
		if err != nil {
			return nil, err
		}
		s.wire = failedStreamRequestOf(httpReq)
		resp, err := c.httpClient(t.connect).Do(httpReq)
		if err != nil {
			return nil, &ccFinalError{err: c.attemptError(attemptCtx, kill, err, s)}
		}
		s.headers, s.status = resp.Header.Clone(), resp.StatusCode
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := readLimited(resp.Body, ccErrorBodyLimit)
			_ = resp.Body.Close()
			return nil, &ccFinalError{err: c.httpError(resp, data)}
		}
		StreamDiagCaptureResponse(s.diag, resp)
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return nil, &ccFinalError{err: c.attemptError(attemptCtx, kill, err, s)}
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				return nil, &ccFinalError{err: c.streamError(providerStreamErrorFromNonJSON(string(data), s.headers))}
			}
			if eo, ok := payload["error"]; ok && eo != nil {
				return nil, &ccFinalError{err: c.httpError(resp, data)}
			}
			return &relayCompletedBody{payload: payload, status: resp.StatusCode, headers: s.headers}, nil
		}
		return &anthRelayChunks{r: relay, src: c.transport.Events(resp), body: resp.Body, c: c, ctx: attemptCtx, kill: kill, s: s}, nil
	}
	src := &managedRelaySource{ctx: attemptCtx}
	src.open = func() (*ManagedLlmStream, error) {
		return NewManagedLlmStream(attemptCtx, relayCopyMap(p.body), factory, ManagedLlmStreamOptions{
			SessionID: identity.SessionID, Name: identity.Name, ModelName: identity.ModelName,
			Finalizer: func(context.Context) (any, error) {
				relay.mu.Lock()
				defer relay.mu.Unlock()
				return relay.acc.Finalize(), nil
			},
			OnStreamCreated: func(context.Context, any) error {
				writerToken.Store(int64(ClaimStreamWriter(hooks.fence)))
				if attemptCtx.Err() != nil {
					return &ccFinalError{err: c.attemptError(attemptCtx, kill, context.Cause(attemptCtx), s)}
				}
				return nil
			},
			OnChunk: func(_ context.Context, chunk any) error {
				relay.mu.Lock()
				defer relay.mu.Unlock()
				return relay.acc.Observe(chunk)
			},
			ChunkAdapter: func(chunk any) any { return chunk },
			AcceptChunk: func(any) bool {
				return writerStillCurrent(hooks.fence, int(writerToken.Load()), "Anthropic streaming", req.Model)
			},
			CompletedResponsePredicate: func(value any) bool {
				body, ok := value.(*relayCompletedBody)
				return ok && body != nil
			},
			Metadata: identity.metadata("anthropic_messages"), DeferLogicalCompletion: true,
		})
	}
	relay.src = src
	s.relay = relay
	out, err := c.consumeStream(ctx, attemptCtx, cancel, kill, relay, s, t)
	return out, s.deltasSent, err
}
