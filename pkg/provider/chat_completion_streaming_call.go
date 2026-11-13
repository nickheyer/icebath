package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var streamingCallLog = slog.Default()

// Streaming request on the agent wire firing the delta callbacks, the non streaming response shape on return
func InterruptibleStreamingAPICall(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any, onFirstDelta func()) (any, error) {
	if agent.InterruptRequested() {
		return nil, &InterruptedError{Message: "Agent interrupted before streaming API call"}
	}
	switch agent.APIMode() {
	case string(ModeCodexResponses):
		return StreamCodexPassthrough(ctx, agent, apiKwargs, onFirstDelta)
	case string(modeBedrockConverse):
		return BedrockStreamCall(ctx, agent, apiKwargs, onFirstDelta)
	}
	if err := checkStaleGiveup(agent.RetrySession()); err != nil {
		return nil, err
	}
	return newStreamingCall(ctx, agent, apiKwargs, onFirstDelta).run()
}

// One streaming request shared by the request worker and the heartbeat, stale and interrupt monitor
type streamingCall struct {
	ctx          context.Context
	agent        ChatCompletionAgent
	apiKwargs    map[string]any
	onFirstDelta func()
	inline       bool
	clients      *requestClientRegistry
	worker       *registryCaller
	callDone     chan struct{}
	callDoneOnce sync.Once
	workerDone   chan struct{}

	requestCancelled     atomic.Bool
	firstDeltaFired      atomic.Bool
	deltasWereSent       atomic.Bool
	providerToolInFlight atomic.Bool
	monitorInterrupted   atomic.Bool
	lastChunk            atomic.Int64

	mu               sync.Mutex
	response         any
	err              error
	partialToolNames []string
	diag             *StreamDiag
	progress         map[string]any
	progressAt       time.Time
	staleTimeout     float64
	current          int
	cancelled        map[int]bool
	discardedChunks  int
	discardedBytes   int
	staleCounted     map[int]bool
	managedStream    *ManagedLlmStream
	writerToken      int
	attemptClient    any
	attemptResponse  *http.Response
	attemptConn      func() net.Conn
	lifetimeKill     *streamingLifetimeKill
	mon              streamingMonitor
	requestRoute     [4]string
	abandonErr       error
	releasedAttempt  int
	compatRetries    int
}

func newStreamingCall(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any, onFirstDelta func()) *streamingCall {
	c := &streamingCall{ctx: withStaleStreak(ctx, agent.RetrySession()), agent: agent, apiKwargs: apiKwargs, onFirstDelta: onFirstDelta,
		clients: newRequestClientRegistry(agent), worker: &registryCaller{}, cancelled: map[int]bool{}, staleCounted: map[int]bool{}}
	c.lastChunk.Store(time.Now().UnixNano())
	c.requestRoute = c.liveRoute()
	return c
}

func (c *streamingCall) liveRoute() [4]string {
	return [4]string{c.agent.Model(), c.agent.Provider(), c.agent.BaseURL(), c.agent.APIMode()}
}

// True once a model switch re-pointed the agent mid request so the turn loop rebuilds it for the new route
func (c *streamingCall) routeSwitchedUnderRequest() bool {
	live := c.liveRoute()
	if live == c.requestRoute {
		return false
	}
	or := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	streamingCallLog.Warn(fmt.Sprintf("Stream retry skipped: model/provider switched mid-request (%s via %s -> %s via %s); handing back to the turn loop to rebuild the request for the current route.",
		c.requestRoute[0], or(c.requestRoute[1], c.requestRoute[2]), live[0], or(live[1], live[2])))
	return true
}

// Runs a display callback, a panic swallowed so a hook never breaks the stream
func quietCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

func (c *streamingCall) touchChunkClock() { c.lastChunk.Store(time.Now().UnixNano()) }

func (c *streamingCall) lastChunkTime() time.Time { return time.Unix(0, c.lastChunk.Load()) }

func (c *streamingCall) setManagedStream(s *ManagedLlmStream) *ManagedLlmStream {
	c.mu.Lock()
	c.managedStream = s
	c.mu.Unlock()
	return s
}

// Closes and forgets the managed provider stream, a failed close logged at debug
func (c *streamingCall) closeManagedStream() {
	c.mu.Lock()
	s := c.managedStream
	c.managedStream = nil
	c.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.Close(); err != nil {
		streamingCallLog.Debug("Managed provider stream cleanup failed", "exc_info", err)
	}
}

// Next attempt id with the attempt local progress, tool flag and partial tool names reset
func (c *streamingCall) startStreamAttempt() int {
	c.mu.Lock()
	c.current++
	id := c.current
	c.progress = nil
	c.partialToolNames = nil
	c.mu.Unlock()
	c.providerToolInFlight.Store(false)
	return id
}

func (c *streamingCall) cancelCurrentStreamAttempt(reason string) {
	c.mu.Lock()
	current := c.current
	if current != 0 {
		c.cancelled[current] = true
	}
	c.mu.Unlock()
	if current != 0 {
		streamingCallLog.Debug(fmt.Sprintf("Marked stream attempt %d cancelled: %s", current, reason))
	}
}

func (c *streamingCall) streamAttemptIsActive(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return id == c.current && !c.cancelled[id]
}

func (c *streamingCall) streamAttemptWasCancelled(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled[id]
}

// Counts a chunk of a superseded attempt, warning on the first and debug logging the rest
func (c *streamingCall) discardStaleStreamChunk(id int, chunk any) {
	size := len(pyrepr.Repr(chunk))
	c.mu.Lock()
	c.discardedChunks++
	c.discardedBytes += size
	chunks, bytes := c.discardedChunks, c.discardedBytes
	c.mu.Unlock()
	if chunks == 1 {
		streamingCallLog.Warn(fmt.Sprintf("Discarding chunk from superseded stream attempt %d (discarded_chunks=%d discarded_bytes=%d)", id, chunks, bytes))
		return
	}
	streamingCallLog.Debug(fmt.Sprintf("Discarded stale stream chunk from attempt %d (discarded_chunks=%d discarded_bytes=%d)", id, chunks, bytes))
}

func (c *streamingCall) fireFirstDelta() {
	if c.onFirstDelta != nil && c.firstDeltaFired.CompareAndSwap(false, true) {
		quietCall(c.onFirstDelta)
	}
}

func (c *streamingCall) emitText(text string) {
	c.fireFirstDelta()
	c.agent.FireStreamDelta(text)
	c.deltasWereSent.Store(true)
}

// True when scrubbed visible assistant text reached a stream consumer this attempt
func (c *streamingCall) visibleTextDelivered() bool {
	return strings.TrimSpace(c.agent.CurrentStreamedAssistantText()) != ""
}

func (c *streamingCall) emitReasoning(text string) {
	c.fireFirstDelta()
	c.agent.FireReasoningDelta(text)
}

func (c *streamingCall) emitToolStarted(name string) {
	c.fireFirstDelta()
	c.agent.FireToolGenStarted(name)
}

// Tool call turns still route content to the delta callback so its reasoning tags reach the display
func (c *streamingCall) routeSuppressedText(text string) {
	quietCall(func() { c.agent.RouteSuppressedStreamText(text) })
}

func (c *streamingCall) newDiag() *StreamDiag {
	diag := StreamDiagInit()
	c.mu.Lock()
	c.diag = diag
	c.mu.Unlock()
	return diag
}

// Stamps liveness for a real chunk and folds it into the attempt diagnostics
func (c *streamingCall) countChunk(diag *StreamDiag, chunk any) {
	now := time.Now()
	c.lastChunk.Store(now.UnixNano())
	c.agent.TouchActivity("receiving stream response")
	size := 0
	if m, ok := chunk.(map[string]any); ok {
		size = estimateChunkBytes(m)
	}
	c.mu.Lock()
	countDiagChunk(diag, now, size)
	c.mu.Unlock()
}

// Counts a real chunk, true for a llama server prompt progress chunk that is liveness only
func (c *streamingCall) takeChunk(diag *StreamDiag, chunk map[string]any, llamacpp bool) bool {
	var progress map[string]any
	if llamacpp {
		progress = promptProgress(chunk)
	}
	if progress == nil {
		c.countChunk(diag, chunk)
		return false
	}
	now := time.Now()
	c.lastChunk.Store(now.UnixNano())
	c.mu.Lock()
	c.progress, c.progressAt = progress, now
	c.mu.Unlock()
	c.agent.TouchActivity("model reading the prompt")
	return true
}

// Model, response id and serving provider as the chunks report them
type chunkIdentity struct {
	model, responseID, upstream string
}

func (c *streamingCall) observeChunkIdentity(diag *StreamDiag, chunk map[string]any, id *chunkIdentity) {
	if m := stringOf(chunk["model"]); m != "" && pyrepr.Truthy(chunk["model"]) {
		id.model = m
	}
	if s, ok := chunk["id"].(string); ok && s != "" && id.responseID == "" {
		id.responseID = s
	}
	if p, ok := chunk["provider"].(string); ok && p != "" && id.upstream == "" {
		id.upstream = p
		c.mu.Lock()
		diag.ServingProvider = pystr.Slice(pystr.Strip(p), 0, 64)
		c.mu.Unlock()
	}
}

func (c *streamingCall) markFinishSeen(diag *StreamDiag, finishReason string) {
	if finishReason == "" {
		return
	}
	c.mu.Lock()
	diag.FinishReasonSeen = true
	c.mu.Unlock()
}

// Write, read and connect socket timeouts, the provider request timeout winning over the env defaults
func (c *streamingCall) streamTimeouts() (float64, float64, float64) {
	if cfg, ok := GetProviderRequestTimeout(c.ctx, c.agent.Provider(), c.agent.Model()); ok {
		return cfg, cfg, math.Min(cfg, 60)
	}
	base := envFloat("ICEBATH_API_TIMEOUT", 1800)
	read := envFloat("ICEBATH_STREAM_READ_TIMEOUT", 120)
	c.mu.Lock()
	stale := c.staleTimeout
	c.mu.Unlock()
	baseURL := c.agent.BaseURL()
	switch {
	case read == 120 && baseURL != "" && IsLocalEndpoint(baseURL):
		read = base
		streamingCallLog.Debug(fmt.Sprintf("Local provider detected (%s) — stream read timeout raised to %.0fs", baseURL, read))
	case read == 120 && stale != 0 && !math.IsInf(stale, 1) && stale > read:
		read = stale
		streamingCallLog.Debug(fmt.Sprintf("Cloud reasoning stream — read timeout raised to %.0fs to match stale-stream detector", read))
	}
	return base, read, 30
}

// Usage and finish of a choiceless chunk, an in stream provider error raised as a ProviderStreamError
func streamingChoicelessChunk(chunk map[string]any, finishReason string) (map[string]any, string, error) {
	usage, _ := chunk["usage"].(map[string]any)
	if len(usage) == 0 {
		usage = nil
	}
	errType, errMsg := chunk["error_type"], chunk["error_message"]
	if pyrepr.Truthy(errType) || pyrepr.Truthy(errMsg) {
		status := statusCodeFromPayload(map[string]any{"code": errType, "message": errMsg})
		if status == 0 {
			status = statusCodeFromValue(errType)
		}
		code := any("provider_in_stream_error")
		if pyrepr.Truthy(errType) {
			code = errType
		}
		msg := pyrepr.Str(chunk)
		if pyrepr.Truthy(errMsg) {
			msg = pyrepr.Str(errMsg)
		}
		return nil, finishReason, &ProviderStreamError{StatusCode: status, Body: providerErrorBody(map[string]any{"code": code, "message": msg}, status),
			RawText: fmt.Sprintf("%s: %s", pyrepr.Str(errType), pyrepr.Str(errMsg))}
	}
	switch last := chunk["lastOne"].(type) {
	case bool:
		if last && finishReason == "" {
			finishReason = "stop"
		}
	case float64:
		if last == 1 && finishReason == "" {
			finishReason = "stop"
		}
	case int:
		if last == 1 && finishReason == "" {
			finishReason = "stop"
		}
	case string:
		if last == "true" && finishReason == "" {
			finishReason = "stop"
		}
	}
	return usage, finishReason, nil
}

// Opens the chat completions stream on a fresh request local client with the usage extension when allowed
func (c *streamingCall) openChatStream(streamKwargs map[string]any) (any, error) {
	if !IsNativeGeminiBaseURL(c.agent.BaseURL()) && !c.agent.RetrySession().StreamOptionsUnsupported {
		streamKwargs["stream_options"] = map[string]any{"include_usage": true}
	}
	client, err := c.agent.CreateRequestOpenAIClient("chat_completion_stream_request", streamKwargs)
	if err != nil {
		return nil, err
	}
	client = c.clients.setClient(client, "openai", c.worker)
	c.mu.Lock()
	c.attemptClient = client
	c.mu.Unlock()
	c.touchChunkClock()
	c.agent.TouchActivity("waiting for provider response (streaming)")
	streamKwargs = BypassChatSDKRequestTransform(streamKwargs, client)
	creator, ok := client.(chatCompletionCreator)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'chat'", client)
	}
	return creator.CreateChatCompletion(c.ctx, streamKwargs)
}

// HTTP response behind a raw SDK stream, nil for a stream that exposes none
func rawStreamResponse(raw any) *http.Response {
	if s, ok := raw.(*auxSDKStream); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.resp
	}
	return nil
}

// Captures the attempt response headers, claims the stream writer and re-aborts a raced cancel
func (c *streamingCall) chatStreamCreated(raw any) {
	resp := rawStreamResponse(raw)
	c.mu.Lock()
	c.attemptResponse, c.attemptConn = resp, nil
	if s, ok := raw.(*auxSDKStream); ok {
		c.attemptConn = func() net.Conn { return socketFromResponse(s) }
	}
	diag := c.diag
	c.mu.Unlock()
	if resp != nil {
		if credits := c.agent.CreditsCapture(); credits != nil {
			credits.CaptureRateLimits(resp.Header)
			credits.CaptureCredits(resp.Header, false)
			credits.CaptureNousModelSwitch(resp.Header)
			credits.CheckOpenRouterCacheStatus(resp.Header)
		}
		c.mu.Lock()
		StreamDiagCaptureResponse(diag, resp)
		c.mu.Unlock()
	}
	token := ClaimStreamWriter(c.agent)
	c.mu.Lock()
	c.writerToken = token
	c.mu.Unlock()
	c.reabortIfCancelled(resp)
}

// Shuts the socket of an attempt whose connect raced the interrupt or a stale kill
func (c *streamingCall) reabortIfCancelled(resp *http.Response) {
	c.mu.Lock()
	cancelled := c.requestCancelled.Load() || c.cancelled[c.current]
	client := c.attemptClient
	c.mu.Unlock()
	if !cancelled {
		return
	}
	streamingCallShutdownAttemptSocket(c, resp)
	if client == nil {
		return
	}
	if c.agent.APIMode() == string(ModeAnthropicMessages) {
		_ = c.agent.AbortRequestAnthropicClient(client, "cancelled_attempt_late_connect")
		return
	}
	_ = c.agent.AbortRequestOpenAIClient(client, "cancelled_attempt_late_connect")
}

// Writer fence and attempt check for a chunk, a marker only finish always passing
func (c *streamingCall) acceptChatChunk(id int, chunk any) bool {
	if m, ok := chunk.(map[string]any); ok {
		choices, _ := m["choices"].([]any)
		var choice, delta map[string]any
		if len(choices) > 0 {
			choice, _ = choices[0].(map[string]any)
		}
		if choice != nil {
			delta, _ = choice["delta"].(map[string]any)
		}
		if pyrepr.Truthy(delta["tool_calls"]) {
			c.providerToolInFlight.Store(true)
		}
		if choice != nil && pyrepr.Truthy(choice["finish_reason"]) && !pyrepr.Truthy(delta["content"]) && !pyrepr.Truthy(delta["tool_calls"]) &&
			!pyrepr.Truthy(delta["reasoning_content"]) && !pyrepr.Truthy(delta["reasoning"]) {
			return true
		}
	}
	if !c.streamAttemptIsActive(id) || !c.writerStillCurrent("Streaming") {
		return false
	}
	c.touchChunkClock()
	return true
}

// Single writer fence, false with a warning once a newer stream claimed the writer slot
func (c *streamingCall) writerStillCurrent(label string) bool {
	c.mu.Lock()
	token := c.writerToken
	c.mu.Unlock()
	if token == 0 || StreamWriterIsCurrent(c.agent, token) {
		return true
	}
	model := "unknown"
	if m, ok := c.apiKwargs["model"]; ok {
		model = pyrepr.Str(m)
	}
	streamingCallLog.Warn(fmt.Sprintf("%s attempt superseded by a newer stream; stopping consumption to preserve the single-writer invariant (model=%s).", label, model))
	return false
}

// Chunk source over the SDK stream for the Relay iterator, ending with io.EOF
type sdkChunkSource struct{ s *auxSDKStream }

func (r *sdkChunkSource) Next(ctx context.Context) (any, error) { return r.s.Next(ctx) }

func (r *sdkChunkSource) Close() error { return r.s.Close() }

// Streams one chat completions attempt, firing deltas and returning the non streaming shaped response
func (c *streamingCall) callChatCompletions(id int) (any, error) {
	base, read, connCap := c.streamTimeouts()
	var contentParts, reasoningParts, detailDisplayParts, refusalParts, pendingText []string
	var reasoningDetails []map[string]any
	tools := newCCToolCallAccumulator()
	carriers := newStreamCarriers(c.ctx)
	var finishReason string
	var usage map[string]any
	ident := &chunkIdentity{}
	diag := c.newDiag()
	c.mu.Lock()
	c.writerToken, c.attemptClient, c.attemptResponse, c.attemptConn = 0, nil, nil, nil
	c.mu.Unlock()
	relayResponse := newRelayChatAccumulator()
	contentWatch, reasoningWatch, detailWatch := NewRunawayStreamWatch(), NewRunawayStreamWatch(), NewRunawayStreamWatch()
	runaway := ""
	llamacpp := streamsLlamaCpp(Route{Provider: c.agent.Provider(), BaseURL: c.agent.BaseURL(), Model: c.agent.Model()})
	factory := func(_ context.Context, next map[string]any) (any, error) {
		if llamacpp {
			next = withPromptProgress(next)
		}
		kwargs := make(map[string]any, len(next)+2)
		for k, v := range next {
			kwargs[k] = v
		}
		kwargs["stream"], kwargs["timeout"] = true, sdkStreamTimeout(connCap, read, base, connCap)
		raw, err := c.openChatStream(kwargs)
		if err != nil {
			return nil, err
		}
		if s, ok := raw.(*auxSDKStream); ok {
			return &sdkChunkSource{s: s}, nil
		}
		return raw, nil
	}
	flushPending := func() {
		pending := pendingText
		pendingText = nil
		for _, text := range pending {
			if tools.Len() > 0 {
				c.routeSuppressedText(text)
			} else {
				c.emitText(text)
			}
		}
	}
	identity := agentStreamRelayIdentity(c.agent, "provider")
	stream, err := NewManagedLlmStream(c.ctx, c.apiKwargs, factory, ManagedLlmStreamOptions{
		SessionID: identity.SessionID, Name: identity.Name, ModelName: identity.ModelName,
		Finalizer: func(context.Context) (any, error) { return relayResponse.Finalize(), nil },
		OnStreamCreated: func(_ context.Context, raw any) error {
			if src, ok := raw.(*sdkChunkSource); ok {
				raw = src.s
			}
			c.chatStreamCreated(raw)
			return nil
		},
		OnChunk:      func(_ context.Context, chunk any) error { relayResponse.Observe(chunk); return nil },
		ChunkAdapter: func(chunk any) any { return chunk },
		AcceptChunk:  func(chunk any) bool { return c.acceptChatChunk(id, chunk) },
		CompletedResponsePredicate: func(value any) bool {
			m, ok := value.(map[string]any)
			_, has := m["choices"]
			return ok && has
		},
		Metadata: identity.metadata("chat_completions"), DeferLogicalCompletion: true,
	})
	if err != nil {
		return nil, err
	}
	c.setManagedStream(stream)
	if c.agent.Provider() == "moa" {
		c.clients.setStreamHandle(stream, c.worker)
	}
	closeHalfRead := func(reason string) {
		if err := stream.Close(); err != nil {
			c.mu.Lock()
			client := c.attemptClient
			c.mu.Unlock()
			if client != nil {
				_ = c.agent.AbortRequestOpenAIClient(client, reason)
			}
		}
	}
	chunks := stream.Iter()
	for {
		raw, err := chunks.Next(c.ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var te *AuxTransportError
			if errors.As(err, &te) && te.Class == "JSONDecodeError" {
				c.mu.Lock()
				resp := c.attemptResponse
				c.mu.Unlock()
				return nil, providerStreamErrorFromJSONDecodeError(err, resp)
			}
			return nil, err
		}
		chunk, _ := raw.(map[string]any)
		isProgress := c.takeChunk(diag, chunk, llamacpp)
		if c.agent.InterruptRequested() {
			closeHalfRead("interrupt_stream_close_failed")
			break
		}
		if !c.streamAttemptIsActive(id) {
			c.discardStaleStreamChunk(id, raw)
			continue
		}
		if isProgress {
			continue
		}
		c.observeChunkIdentity(diag, chunk, ident)
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			u, fr, err := streamingChoicelessChunk(chunk, finishReason)
			if err != nil {
				return nil, err
			}
			if u != nil {
				usage = u
			}
			finishReason = fr
			c.markFinishSeen(diag, finishReason)
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if fr, _ := NormalizeFinishReason(choice["finish_reason"]).(string); fr != "" {
			finishReason = fr
		}
		c.markFinishSeen(diag, finishReason)
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		var reasoningText *string
		for _, key := range []string{"reasoning_content", "reasoning"} {
			if s, ok := delta[key].(string); ok && s != "" {
				reasoningText = &s
				break
			}
		}
		reasoningText = carriers.feed(delta, reasoningText)
		plainReasoning := ""
		if reasoningText != nil && *reasoningText != "" {
			prev := ""
			if n := len(reasoningParts); n > 0 {
				prev = reasoningParts[n-1]
			}
			plainReasoning = separateGluedReasoningBlocks(prev, *reasoningText)
			reasoningParts = append(reasoningParts, plainReasoning)
		}
		var detailTextParts []string
		if rd, ok := delta["reasoning_details"].([]any); ok {
			for _, d := range rd {
				detailTextParts = append(detailTextParts, streamedReasoningDetailText(d))
				reasoningDetails = appendStreamedReasoningDetail(reasoningDetails, d)
			}
		}
		detailText := strings.Join(detailTextParts, "")
		if detailText != "" {
			prev := ""
			if n := len(detailDisplayParts); n > 0 {
				prev = detailDisplayParts[n-1]
			}
			detailText = separateGluedReasoningBlocks(prev, detailText)
			detailDisplayParts = append(detailDisplayParts, detailText)
		}
		display := detailText
		if display == "" {
			display = plainReasoning
		}
		if display != "" {
			c.emitReasoning(display)
			if reasoningWatch.Feed(plainReasoning) || detailWatch.Feed(detailText) {
				runaway = "reasoning"
				break
			}
		}
		if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
			refusalParts = append(refusalParts, refusal)
		}
		if deltaContent := flattenMessageText(delta["content"], ""); deltaContent != "" {
			contentParts = append(contentParts, deltaContent)
			if contentWatch.Feed(deltaContent) {
				runaway = "content"
				break
			}
			switch {
			case tools.Len() > 0:
				c.routeSuppressedText(deltaContent)
			case len(pendingText) > 0 || providerStreamTextMayBeSSE(deltaContent) || (!c.deltasWereSent.Load() && routerTimeoutShimMayFollow(strings.Join(contentParts, ""))):
				pendingText = append(pendingText, deltaContent)
				pending := strings.Join(pendingText, "")
				if !(providerStreamTextMayBeSSE(pending) || routerTimeoutShimMayFollow(pending)) {
					flushPending()
				}
				continue
			default:
				c.emitText(deltaContent)
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			flushPending()
			for _, rawTC := range tcs {
				tc, _ := rawTC.(map[string]any)
				if tc == nil {
					continue
				}
				if name, _ := tools.Feed(tc); name != "" {
					c.emitToolStarted(name)
					c.mu.Lock()
					c.partialToolNames = append(c.partialToolNames, name)
					c.mu.Unlock()
				}
			}
		}
	}
	if runaway != "" {
		closeHalfRead("runaway_stream_close_failed")
		c.logRunawayCut(runaway)
	}
	tools.Materialize()
	c.closeManagedStream()
	if c.streamAttemptWasCancelled(id) {
		return nil, &AuxTransportError{Class: "RemoteProtocolError", Message: fmt.Sprintf("stream attempt %d was superseded", id)}
	}
	if stream.FinalResponse != nil {
		return c.adoptFinalResponse(stream.FinalResponse), nil
	}
	if runaway != "" {
		finishReason = "length"
	}
	c.mu.Lock()
	resp := c.attemptResponse
	c.mu.Unlock()
	response, err := c.finishChatStream(resp, contentParts, reasoningParts, tools, finishReason, ident, usage, flushPending, reasoningDetails, refusalParts)
	if err != nil {
		return nil, err
	}
	carriers.apply(response)
	if runaway != "" {
		response[ccRawRunaway] = true
	}
	return response, nil
}

func (c *streamingCall) logRunawayCut(channel string) {
	model := "unknown"
	if m, ok := c.apiKwargs["model"]; ok {
		model = pyrepr.Str(m)
	}
	prov := c.agent.Provider()
	if prov == "" {
		prov = "unknown"
	}
	streamingCallLog.Warn(fmt.Sprintf("%sStream cut: the %s turned into a runaway repetition loop (model=%s, provider=%s).", c.agent.LogPrefix(), channel, model, prov))
}

// Latches the session to non streaming and replays a completed response the adapter returned
func (c *streamingCall) adoptFinalResponse(final any) any {
	streamingCallLog.Info(fmt.Sprintf("Streaming request returned a final response object instead of an iterator; switching %s/%s to non-streaming for this session.", orUnknown2(c.agent.Provider()), orUnknown2(c.agent.Model())))
	c.agent.RetrySession().DisableStreaming = true
	return c.replayFinalResponse(final)
}

// Replays a completed chat completions response's reasoning and content as deltas
func (c *streamingCall) replayFinalResponse(final any) any {
	m, _ := DumpIfModel(final).(map[string]any)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return final
	}
	choice, _ := choices[0].(map[string]any)
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return final
	}
	reasoning := ""
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if s, ok := message[key].(string); ok && s != "" {
			reasoning = s
			break
		}
	}
	if reasoning != "" {
		c.emitReasoning(reasoning)
	}
	if content, ok := message["content"].(string); ok && content != "" {
		c.fireFirstDelta()
		c.agent.FireStreamDelta(content)
	}
	return final
}

// Length finished response for a cut stream in the api mode wire shape with the drop markers set
func partialStreamCutResponse(content, reasoning any, model string, usage any, dropped []string, overflowTerminal, cleanEOF bool, apiMode string) map[string]any {
	var droppedValue any
	if len(dropped) > 0 {
		droppedValue = dropped
	}
	var cut map[string]any
	if apiMode == string(ModeAnthropicMessages) {
		text, _ := content.(string)
		cut = map[string]any{"id": PartialStreamStubID, "type": "message", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "max_tokens", "stop_sequence": nil, "usage": usage}
	} else {
		msg := map[string]any{"role": "assistant", "content": content, "tool_calls": nil, "reasoning_content": reasoning}
		cut = map[string]any{"id": PartialStreamStubID, "model": model, "usage": usage,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "length"}}}
	}
	cut[ccRawDroppedToolNames], cut[ccRawOverflowTerminal], cut[ccRawCleanEOF] = droppedValue, overflowTerminal, cleanEOF
	return cut
}

// Assembles the non streaming shaped response, a drop without a finish reason becoming a cut response
func (c *streamingCall) finishChatStream(resp *http.Response, contentParts, reasoningParts []string, tools *ccToolCallAccumulator, finishReason string, ident *chunkIdentity, usage map[string]any, flushPending func(), reasoningDetails []map[string]any, refusalParts []string) (map[string]any, error) {
	fullContent := strings.Join(contentParts, "")
	fullReasoning := strings.Join(reasoningParts, "")
	if fullReasoning == "" && fullContent != "" {
		fullReasoning = inlineReasoning(fullContent)
	}
	toolCalls, truncated := assembleStreamedToolCalls(tools, finishReason)
	if finishReason == "" && len(contentParts) == 0 && len(reasoningParts) == 0 && len(refusalParts) == 0 && tools.Len() == 0 {
		return nil, &EmptyStreamError{Message: "Provider returned an empty stream with no finish_reason (possible upstream error or malformed SSE response)."}
	}
	var usageValue any
	if usage != nil {
		usageValue = usage
	}
	if truncated && finishReason == "" {
		var dropped []string
		for _, idx := range tools.Slots() {
			name := tools.acc[idx].Name
			if name == "" {
				name = "?"
			}
			dropped = append(dropped, name)
		}
		streamingCallLog.Warn(fmt.Sprintf("Clean EOF, no finish_reason: server ended the stream (no transport exception) while a tool call's arguments were still incomplete (tools=%s). The server or a proxy closed the stream cleanly; not an output-length truncation.", pyrepr.Repr(dropped)))
		return partialStreamCutResponse(noneIfEmpty(fullContent), noneIfEmpty(fullReasoning), ident.model, usageValue, dropped, false, true, ""), nil
	}
	if finishReason == "" && (len(contentParts) > 0 || len(reasoningParts) > 0) && tools.Len() == 0 && usage == nil {
		streamingCallLog.Warn("Clean EOF, no finish_reason: server ended the stream (no transport exception) after delivering text with no tool calls. The server or a proxy closed the stream cleanly.")
		return partialStreamCutResponse(noneIfEmpty(fullContent), noneIfEmpty(fullReasoning), ident.model, nil, nil, false, true, ""), nil
	}
	effective := finishReason
	if truncated {
		effective = "length"
	} else if effective == "" {
		effective = "stop"
	}
	var headers http.Header
	if resp != nil {
		headers = resp.Header
	}
	if pse := providerStreamErrorFromText(fullContent, effective, headers); pse != nil {
		return nil, pse
	}
	var toolList any
	if len(toolCalls) > 0 {
		list := make([]any, 0, len(toolCalls))
		for _, tc := range toolCalls {
			list = append(list, tc)
		}
		toolList = list
	}
	message := map[string]any{"role": "assistant", "content": noneIfEmpty(fullContent), "tool_calls": toolList,
		"reasoning_content": noneIfEmpty(fullReasoning), "refusal": noneIfEmpty(strings.Join(refusalParts, ""))}
	if len(reasoningDetails) > 0 {
		list := make([]any, 0, len(reasoningDetails))
		for _, d := range reasoningDetails {
			list = append(list, d)
		}
		message["reasoning_details"] = list
	}
	responseID := ident.responseID
	if responseID == "" {
		responseID = "stream-" + uuid.NewString()
	}
	var modelValue, providerValue any
	if ident.model != "" {
		modelValue = ident.model
	}
	if ident.upstream != "" {
		providerValue = ident.upstream
	}
	response := map[string]any{"id": responseID, "model": modelValue, "usage": usageValue, "provider": providerValue,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": effective}}}
	if !chatResponseIsRouterTimeoutShim(response) {
		flushPending()
	}
	return response, nil
}

// Resets delivery when deltas fired but nothing visible reached a consumer, true while a tool call is in flight
func (c *streamingCall) forgetInvisibleDeltas(err error) bool {
	c.mu.Lock()
	toolInFlight := len(c.partialToolNames) > 0
	c.mu.Unlock()
	toolInFlight = toolInFlight || c.providerToolInFlight.Load()
	if c.deltasWereSent.Load() && !toolInFlight && !c.visibleTextDelivered() {
		slog.Warn(fmt.Sprintf("Stream died after deltas but before any visible text was delivered (0 chars, no tool call in flight); treating as an undelivered stream failure: %v", err))
		quietCall(c.agent.ResetStreamDeliveryTracking)
		c.deltasWereSent.Store(false)
		c.firstDeltaFired.Store(false)
	}
	return toolInFlight
}

// Per provider stale_timeout_seconds config, else the stream stale env base
func streamingConfiguredStaleBase(ctx context.Context, agent ChatCompletionAgent) float64 {
	if cfg, ok := GetProviderStaleTimeout(ctx, agent.Provider(), agent.Model()); ok {
		return cfg
	}
	base, _ := streamEnvStaleBase()
	return base
}

// Picks the stale timeout, finite long patience for local endpoints and context scaled for cloud ones
func (c *streamingCall) resolveStaleTimeout() {
	baseURL := c.agent.BaseURL()
	timeout := 0.0
	if streamingConfiguredStaleBase(c.ctx, c.agent) == 180 && baseURL != "" && IsLocalEndpoint(baseURL) {
		timeout = localStreamStaleTimeoutDefault(c.ctx)
		streamingCallLog.Debug(fmt.Sprintf("Local provider detected (%s) — stale stream timeout set to %.0fs", baseURL, timeout))
	} else {
		timeout = cloudStaleTimeoutForAgent(c.ctx, c.agent, c.agent.Provider(), c.agent.Model(), c.apiKwargs)
	}
	c.mu.Lock()
	c.staleTimeout = timeout
	c.mu.Unlock()
}

func (c *streamingCall) setResult(response any, err error) {
	c.mu.Lock()
	c.response, c.err = response, err
	c.mu.Unlock()
}

func (c *streamingCall) markCallDone() { c.callDoneOnce.Do(func() { close(c.callDone) }) }

// Attempt loop with the transient retry budget plus the one stream_options compatibility retry
func (c *streamingCall) call() {
	maxRetries := envInt("ICEBATH_STREAM_RETRIES", 2)
	c.mu.Lock()
	c.compatRetries = 0
	c.mu.Unlock()
	attempt := -1
	defer func() {
		c.closeManagedStream()
		c.mu.Lock()
		ok := c.response != nil
		c.mu.Unlock()
		reason := "stream_error_cleanup"
		if ok {
			reason = "stream_request_complete"
		}
		_ = c.clients.closeOnce(reason, c.worker)
	}()
	for {
		c.mu.Lock()
		limit := maxRetries + c.compatRetries
		c.mu.Unlock()
		if attempt >= limit {
			return
		}
		attempt++
		id := c.startStreamAttempt()
		if c.agent.InterruptRequested() {
			c.cancelCurrentStreamAttempt("interrupt_before_stream_retry")
			c.setResult(nil, &InterruptedError{Message: "Agent interrupted before stream retry"})
			return
		}
		response, err := WithStreamEmitters(c.agent, func() (any, error) { return c.callWire(id) })
		if err == nil {
			c.mu.Lock()
			c.response = response
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		c.releasedAttempt = id
		c.mu.Unlock()
		c.closeManagedStream()
		if !streamingCallHandleError(c, err, attempt, maxRetries) {
			return
		}
		if c.routeSwitchedUnderRequest() {
			c.setResult(nil, err)
			return
		}
	}
}

func (c *streamingCall) callWire(id int) (any, error) {
	if c.agent.APIMode() != string(ModeAnthropicMessages) {
		return c.callChatCompletions(id)
	}
	client, err := c.agent.CreateRequestAnthropicClient("anthropic_stream_request")
	if err != nil {
		return nil, err
	}
	return streamingCallAnthropicWire(c, c.clients.setClient(client, "anthropic_messages", c.worker))
}

func (c *streamingCall) runCall() {
	defer close(c.workerDone)
	defer c.markCallDone()
	c.call()
}

// Length cut response after tokens reached the platform, terminal on a context overflow
func (c *streamingCall) partialDeliveryResponse(err error) any {
	partial := strings.TrimSpace(c.agent.CurrentStreamedAssistantText())
	c.mu.Lock()
	names := append([]string(nil), c.partialToolNames...)
	c.mu.Unlock()
	if len(names) > 0 {
		nameStr := strings.Join(names[:min(3, len(names))], ", ")
		if len(names) > 3 {
			nameStr += fmt.Sprintf(", +%d more", len(names)-3)
		}
		warn := fmt.Sprintf("\n\n⚠ Stream stalled mid tool-call (%s); the action was not executed. Ask me to retry if you want to continue.", nameStr)
		partial += warn
		if c.agent.WarningPresentationEnabled() {
			quietCall(func() { c.agent.FireStreamDelta(warn) })
		}
		streamingCallLog.Warn(fmt.Sprintf("Partial stream dropped tool call(s) %s after %d chars of text; surfaced warning to user: %v", pyrepr.Repr(names), len([]rune(partial)), err))
	}
	var reason FailoverReason
	func() {
		defer func() { _ = recover() }()
		if cls := ClassifyError(ClassifyInput{Err: err, Provider: c.agent.Provider(), Model: c.agent.Model()}); cls != nil {
			reason = cls.Reason
		}
	}()
	resetStaleStreak(c.ctx)
	model := c.agent.Model()
	if model == "" {
		model = "unknown"
	}
	if reason == ReasonContextOverflow {
		streamingCallLog.Warn(fmt.Sprintf("Partial stream ended on a context-overflow error after %d chars; NOT seeding a continuation cut (transcript is already over budget): %v", len([]rune(partial)), err))
		return partialStreamCutResponse(nil, nil, model, nil, names, true, false, c.agent.APIMode())
	}
	if len(names) == 0 {
		streamingCallLog.Warn(fmt.Sprintf("Partial stream delivered before error; returning length-truncated cut with %d chars of recovered content so the loop can continue from where the stream died: %v", len([]rune(partial)), err))
	}
	cut := partialStreamCutResponse(noneIfEmpty(partial), nil, model, nil, names, false, false, c.agent.APIMode())
	if reason == ReasonContentPolicyBlocked {
		cut[ccRawContentFilter] = true
	}
	return cut
}

// Runs the request inline or on a worker under the monitor, then translates the outcome
func (c *streamingCall) run() (any, error) {
	c.resolveStaleTimeout()
	c.callDone, c.workerDone = make(chan struct{}), make(chan struct{})
	if directAPICallFor(c.ctx, c.agent.Provider(), c.agent.Platform(), APIMode(c.agent.APIMode())) {
		c.inline = true
		monitorDone := make(chan struct{})
		go func() {
			defer close(monitorDone)
			streamingCallMonitorLoop(c)
		}()
		c.runCall()
		select {
		case <-monitorDone:
		case <-time.After(2 * time.Second):
		}
	} else {
		go c.runCall()
		streamingCallMonitorLoop(c)
	}
	if c.monitorInterrupted.Load() {
		return nil, &InterruptedError{Message: "Agent interrupted during streaming API call"}
	}
	if c.agent.InterruptRequested() {
		return nil, &InterruptedError{Message: "Agent interrupted during streaming API call (post-worker)"}
	}
	c.mu.Lock()
	err, response, abandon, diag := c.err, c.response, c.abandonErr, c.diag
	c.mu.Unlock()
	if abandon != nil {
		joinWorkerForRelayTeardown(c.ctx, c.workerDone, "Abandoned streaming")
		err, response = abandon, nil
		c.forgetInvisibleDeltas(err)
	}
	if err != nil {
		if c.deltasWereSent.Load() {
			return c.partialDeliveryResponse(err), nil
		}
		return nil, err
	}
	if response != nil {
		resetStaleStreak(c.ctx)
	}
	c.mu.Lock()
	if diag != nil && diag.FirstChunkAt != nil {
		c.agent.SetLastAPIFirstChunkAt(epochSeconds(*diag.FirstChunkAt))
	}
	c.mu.Unlock()
	return response, nil
}
