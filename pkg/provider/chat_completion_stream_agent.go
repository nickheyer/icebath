package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// StreamEmitters brackets a stream with the agent display hooks for its start and its end
type StreamEmitters interface {
	EmitStreamStart()
	EmitStreamEnd(finalText string, finished bool, errText *string)
}

// Assistant text of a finished response, the first choice content or the joined text parts, empty otherwise
func streamFinalText(response any) string {
	switch resp := response.(type) {
	case *Response:
		if resp == nil || resp.Message == nil {
			return ""
		}
		return resp.Message.Text()
	case *bedrockConverseResult:
		if resp == nil {
			return ""
		}
		text, _ := resp.Content.(string)
		return text
	}
	m, ok := DumpIfModel(response).(map[string]any)
	if !ok {
		return ""
	}
	if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]any); ok {
			if msg, ok := first["message"].(map[string]any); ok {
				if content, ok := msg["content"].(string); ok {
					return content
				}
			}
		}
	}
	switch content := m["content"].(type) {
	case string:
		return content
	case []any:
		out := ""
		for _, part := range content {
			if p, ok := part.(map[string]any); ok {
				if t, ok := p["text"].(string); ok {
					out += t
				}
			}
		}
		return out
	}
	return ""
}

// Runs one stream between the start and end hooks, the end carrying the final text or the error string
func WithStreamEmitters(emitters StreamEmitters, run func() (any, error)) (any, error) {
	if emitters != nil {
		emitters.EmitStreamStart()
	}
	response, err := run()
	if err != nil {
		if emitters != nil {
			text := err.Error()
			emitters.EmitStreamEnd("", false, &text)
		}
		return nil, err
	}
	if emitters != nil {
		emitters.EmitStreamEnd(streamFinalText(response), true, nil)
	}
	return response, nil
}

// Streams a codex turn through the blocking call with the first delta callback parked on the agent
func StreamCodexPassthrough(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any, onFirstDelta func()) (any, error) {
	agent.SetCodexOnFirstDelta(onFirstDelta)
	defer agent.SetCodexOnFirstDelta(nil)
	return WithStreamEmitters(agent, func() (any, error) { return agent.InterruptibleAPICall(ctx, apiKwargs) })
}

// Relay final response of the intercepted Bedrock events, nil for a stream that never reached messageStop
func finalizeBedrockRelayEvents(events []any) (any, error) {
	dicts := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m, ok := ev.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("bedrock relay event %s is not an event dict", pyrepr.TypeName(ev))
		}
		dicts = append(dicts, m)
	}
	result, _, err := streamConverseWithCallbacks(bedrockEventList(dicts), bedrockStreamCallbacks{})
	var empty *EmptyStreamError
	if errors.As(err, &empty) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Shuts the socket of an attempt whose connect raced the interrupt or a stale kill, true when it was cancelled
func reabortIfCancelled(attemptCtx context.Context, resp *http.Response) bool {
	if attemptCtx.Err() == nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// Session, provider name and model the Relay stream of an attempt is recorded under
type streamRelayIdentity struct {
	SessionID, Name, ModelName string
	CallRole                   string
	APIRequestID               any
}

// Relay metadata of a stream attempt on the given api mode
func (id streamRelayIdentity) metadata(apiMode string) map[string]any {
	return map[string]any{"api_mode": apiMode, "api_request_id": id.APIRequestID, "call_role": id.CallRole}
}

// Call role of a request, delegated for a subagent and fallback once a fallback route is active
func streamCallRole(subagent, fallbackActive bool) string {
	switch {
	case subagent:
		return "delegated"
	case fallbackActive:
		return "fallback"
	}
	return "primary"
}

// Agent surfaces a typed stream attempt reads, the Relay identity, the writer fence and the delta gates
type streamHooks struct {
	identity           streamRelayIdentity
	fence              any
	hasStreamConsumers func() bool
	wantsReasoning     func() bool
}

// True unless the caller reports no stream consumer for text deltas
func (h streamHooks) textWanted() bool {
	return h.hasStreamConsumers == nil || h.hasStreamConsumers()
}

// True when a reasoning or delta callback or a plugin reasoning observer wants reasoning deltas
func (h streamHooks) reasoningWanted(ctx context.Context) bool {
	return h.wantsReasoning == nil || h.wantsReasoning() || pluginReasoningObserver(ctx)
}

// Relay identity of the attempt on a route, the provider name defaulting to the wire name
func (h streamHooks) relayIdentity(route Route, nameDefault string) streamRelayIdentity {
	id := h.identity
	id.Name, id.ModelName = route.Provider, route.Model
	if id.Name == "" {
		id.Name = nameDefault
	}
	return id
}

// Chunk source over the decoded Bedrock event stream for the Relay iterator
type bedrockRelayEvents struct{ next bedrockEventSource }

func (e *bedrockRelayEvents) Next(context.Context) (any, error) { return e.next() }

// One Bedrock ConverseStream call, its gated callbacks, its surfaces and its Relay identity
type bedrockStreamRun struct {
	region     string
	kwargs     map[string]any
	stale      float64
	cb         bedrockStreamCallbacks
	firstDelta func()
	// interrupted reports a pending stop, interruptErr builds the error for the poll or the post worker check
	interrupted  func() bool
	interruptErr func(postWorker bool) error
	emit         func(StreamEvent) error
	onDenied     func()
	fence        any
	identity     streamRelayIdentity
}

// Outcome of a Bedrock stream, final set when the IAM fallback or Relay recorded a completed response
type bedrockStreamOutcome struct {
	streamed    *bedrockConverseResult
	final       *bedrockConverseResult
	raw         map[string]any
	denied      bool
	interrupted bool
	headers     http.Header
	status      int
}

// Runs the stream on a worker through Relay while the poll loop applies the interrupt and the event gap watchdog
func runBedrockStream(ctx context.Context, r bedrockStreamRun) (*bedrockStreamOutcome, error) {
	started := time.Now()
	var lastEvent atomic.Int64
	lastEvent.Store(started.UnixNano())
	var responseStarted, firstFired atomic.Bool
	fireFirst := func() {
		responseStarted.Store(true)
		if r.firstDelta != nil && firstFired.CompareAndSwap(false, true) {
			func() {
				defer func() { _ = recover() }()
				r.firstDelta()
			}()
		}
	}
	cb := r.cb
	if text := r.cb.OnTextDelta; text != nil {
		cb.OnTextDelta = func(s string) error { fireFirst(); return text(s) }
	}
	if reasoning := r.cb.OnReasoningDelta; reasoning != nil {
		cb.OnReasoningDelta = func(s string) error { fireFirst(); return reasoning(s) }
	}
	if tool := r.cb.OnToolStart; tool != nil {
		cb.OnToolStart = func(i int, id, name string) error { fireFirst(); return tool(i, id, name) }
	}
	cb.OnInterruptCheck = r.interrupted
	cb.OnEvent = func() { lastEvent.Store(time.Now().UnixNano()) }
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &bedrockStreamOutcome{}
	var outMu sync.Mutex
	type workerResult struct{ err error }
	done := make(chan workerResult, 1)
	go func() {
		done <- workerResult{runBedrockStreamWorker(attemptCtx, r, cb, out, &outMu)}
	}()
	poll := time.NewTicker(bedrockStreamPoll)
	defer poll.Stop()
	var res workerResult
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-ctx.Done():
		case <-poll.C:
		}
		if r.interrupted() {
			recordInterruptedProviderWait(ctx, time.Since(started).Seconds(), responseStarted.Load())
			cancel()
			joinWorkerForRelayTeardown(ctx, done, "Bedrock streaming")
			return nil, r.interruptErr(false)
		}
		if elapsed := time.Since(time.Unix(0, lastEvent.Load())).Seconds(); elapsed > r.stale {
			cancel()
			return nil, bedrockStreamStaleKill(ctx, r.region, bedrockStreamModel(r.kwargs), elapsed, r.stale, r.emit)
		}
	}
	if r.interrupted() {
		recordInterruptedProviderWait(ctx, time.Since(started).Seconds(), responseStarted.Load())
		return nil, r.interruptErr(true)
	}
	if res.err != nil {
		return nil, res.err
	}
	outMu.Lock()
	defer outMu.Unlock()
	if session, _ := ctx.Value(staleStreakKey{}).(*RetrySession); session != nil && (out.final != nil || out.streamed != nil) && !out.interrupted {
		session.ResetStaleStreak()
	}
	return out, nil
}

// Opens the Relay stream over ConverseStream, the IAM fallback answered by converse inside the same attempt
func runBedrockStreamWorker(ctx context.Context, r bedrockStreamRun, cb bedrockStreamCallbacks, out *bedrockStreamOutcome, outMu *sync.Mutex) error {
	var eventsMu sync.Mutex
	var intercepted []any
	writerToken := 0
	factory := func(ctx context.Context, request map[string]any) (any, error) {
		resp, err := bedrockConverseCall(ctx, r.region, request, true, true)
		var denied *bedrockStreamDenied
		if errors.As(err, &denied) {
			r.onDenied()
			bedrockLog.Info(fmt.Sprintf("bedrock: converse_stream denied by IAM (%s) — using non-streaming converse() for this session.", pyTypeNameOfError(denied.err)))
			result, raw, err := bedrockConverseComplete(ctx, r.region, request)
			if err != nil {
				return nil, err
			}
			outMu.Lock()
			out.denied, out.raw = true, raw
			outMu.Unlock()
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		outMu.Lock()
		out.headers, out.status = resp.Header.Clone(), resp.StatusCode
		outMu.Unlock()
		go func() {
			<-ctx.Done()
			_ = resp.Body.Close()
		}()
		return &bedrockRelayEvents{next: bedrockConverseEvents(resp.Body, resp.Header.Clone())}, nil
	}
	request := make(map[string]any, len(r.kwargs))
	for k, v := range r.kwargs {
		request[k] = v
	}
	stream, err := NewManagedLlmStream(ctx, request, factory, ManagedLlmStreamOptions{
		SessionID: r.identity.SessionID, Name: r.identity.Name, ModelName: r.identity.ModelName,
		Finalizer: func(context.Context) (any, error) {
			eventsMu.Lock()
			events := append([]any(nil), intercepted...)
			eventsMu.Unlock()
			return finalizeBedrockRelayEvents(events)
		},
		OnStreamCreated: func(context.Context, any) error { writerToken = ClaimStreamWriter(r.fence); return nil },
		OnChunk: func(_ context.Context, chunk any) error {
			eventsMu.Lock()
			intercepted = append(intercepted, chunk)
			eventsMu.Unlock()
			return nil
		},
		ChunkAdapter: func(chunk any) any { return chunk },
		AcceptChunk:  func(any) bool { return writerToken == 0 || StreamWriterIsCurrent(r.fence, writerToken) },
		CompletedResponsePredicate: func(value any) bool {
			result, ok := value.(*bedrockConverseResult)
			return ok && result != nil
		},
		Metadata: r.identity.metadata("custom"), DeferLogicalCompletion: true,
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	next := func() (map[string]any, error) {
		chunk, err := stream.Next(ctx)
		if err != nil {
			return nil, err
		}
		event, ok := chunk.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("bedrock stream chunk %s is not an event dict", pyrepr.TypeName(chunk))
		}
		return event, nil
	}
	streamed, interrupted, err := streamConverseWithCallbacks(next, cb)
	final, _ := stream.FinalResponse.(*bedrockConverseResult)
	var empty *EmptyStreamError
	if errors.As(err, &empty) && final != nil {
		streamed, err = nil, nil
	}
	if err != nil {
		return err
	}
	outMu.Lock()
	out.streamed, out.final, out.interrupted = streamed, final, interrupted
	outMu.Unlock()
	return nil
}

// Agent form Bedrock stream, the deltas delivered through the agent fire hooks between the stream emitters
func BedrockStreamCall(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any, onFirstDelta func()) (any, error) {
	if err := checkStaleGiveup(agent.RetrySession()); err != nil {
		return nil, err
	}
	ctx = withStaleStreak(ctx, agent.RetrySession())
	region := "us-east-1"
	if v, ok := apiKwargs["__bedrock_region__"]; ok {
		region, _ = v.(string)
	}
	kwargs := make(map[string]any, len(apiKwargs))
	for k, v := range apiKwargs {
		if k != "__bedrock_region__" && k != "__bedrock_converse__" {
			kwargs[k] = v
		}
	}
	surface := agentRequestSurface(agent)
	cb := bedrockStreamCallbacks{
		OnToolStart: func(_ int, _, name string) error { agent.FireToolGenStarted(name); return nil },
	}
	if agent.HasStreamConsumers() {
		cb.OnTextDelta = func(text string) error { agent.FireStreamDelta(text); return nil }
	}
	if agent.WantsReasoningDeltas() || pluginReasoningObserver(ctx) {
		cb.OnReasoningDelta = func(text string) error { agent.FireReasoningDelta(text); return nil }
	}
	r := bedrockStreamRun{
		region: region, kwargs: kwargs, stale: cloudStaleTimeoutFor(ctx, agent.Provider(), bedrockStreamModel(kwargs), kwargs),
		cb: cb, firstDelta: onFirstDelta, fence: agent,
		interrupted: func() bool { return agent.InterruptRequested() || ctx.Err() != nil },
		interruptErr: func(postWorker bool) error {
			if postWorker {
				return &InterruptedError{Message: "Agent interrupted during Bedrock API call (post-worker)"}
			}
			return &InterruptedError{Message: "Agent interrupted during Bedrock API call"}
		},
		emit: func(ev StreamEvent) error { surface(ev); return nil },
		onDenied: func() {
			agent.DisableStreaming()
			agent.SafePrint(bedrockIAMDeniedNotice, true)
		},
		identity: agentStreamRelayIdentity(agent, "bedrock"),
	}
	return WithStreamEmitters(agent, func() (any, error) {
		out, err := runBedrockStream(ctx, r)
		if err != nil {
			return nil, err
		}
		if out.final != nil {
			return out.final, nil
		}
		if out.streamed == nil {
			return nil, nil
		}
		return out.streamed, nil
	})
}

const bedrockIAMDeniedNotice = "\n⚠  AWS IAM denied bedrock:InvokeModelWithResponseStream — falling back to non-streaming InvokeModel.\n   Grant that action to restore streaming output.\n"

// Plugin reasoning observer check, false with a debug line when the hook registry fails
func pluginReasoningObserver(ctx context.Context) (has bool) {
	defer func() {
		if r := recover(); r != nil {
			bedrockLog.Debug("plugin reasoning stream observer check failed", "panic", r)
			has = false
		}
	}()
	return plugins.HasReasoningStreamObserverHooks(ctx)
}

// Relay identity of an agent request, the provider name defaulting to the wire name
func agentStreamRelayIdentity(agent ChatCompletionAgent, nameDefault string) streamRelayIdentity {
	name := agent.Provider()
	if name == "" {
		name = nameDefault
	}
	var requestID any
	if id := agent.CurrentAPIRequestID(); id != "" {
		requestID = id
	}
	return streamRelayIdentity{SessionID: agent.SessionID(), Name: name, ModelName: agent.Model(),
		CallRole: streamCallRole(agent.IsSubagent(), agent.FallbackIndex() > 0), APIRequestID: requestID}
}
