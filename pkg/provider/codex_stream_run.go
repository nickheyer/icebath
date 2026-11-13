package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const defaultCodexStreamDrainTimeout = 2.0

// Seconds the post terminal drain may block, agent.stream_drain_timeout clamped at zero, two by default
func codexStreamDrainTimeout(ctx context.Context) float64 {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return defaultCodexStreamDrainTimeout
	}
	agentCfg, ok := map[string]any(tree)["agent"].(map[string]any)
	if !ok {
		return defaultCodexStreamDrainTimeout
	}
	var value float64
	switch v := agentCfg["stream_drain_timeout"].(type) {
	case bool:
		return defaultCodexStreamDrainTimeout
	case float64:
		value = v
	case float32:
		value = float64(v)
	default:
		n, isInt := pyrepr.Int(v)
		if !isInt {
			return defaultCodexStreamDrainTimeout
		}
		if value, _ = new(big.Float).SetInt(n).Float64(); math.IsInf(value, 0) {
			return defaultCodexStreamDrainTimeout
		}
	}
	if !(value > 0) {
		return 0
	}
	return value
}

// Drain budget, raw stream socket lookup and socket shutdown seams of the post terminal drain
var (
	codexStreamDrainTimeoutFn = codexStreamDrainTimeout
	codexStreamSocketFn       = codexStreamSocket
	codexShutdownSocketFn     = auxShutdownConn
)

// Connection behind a raw SDK stream, nil when the stream exposes none or the lookup panics
func codexStreamSocket(raw any) (conn net.Conn) {
	defer func() {
		if recover() != nil {
			conn = nil
		}
	}()
	return socketFromResponse(raw)
}

// Callbacks of one Codex event stream consumption
type codexConsumeOptions struct {
	onTextDelta         func(text string)
	onReasoningDelta    func(text string)
	onCommentaryMessage func(text string)
	onFirstDelta        func()
	onEvent             func(event any) error
	interruptCheck      func() (bool, error)
}

// Whether a hook error is watchdog or cancellation control flow that must end the stream
func codexStreamControlFlow(err error) bool {
	var timeout *TimeoutPhaseError
	return auxStreamControlFlow(err) || errors.As(err, &timeout)
}

// Text callback guarded so a panicking display hook is logged and never ends the turn
func codexGuardedText(fn func(string), label string) func(string) error {
	if fn == nil {
		return nil
	}
	return func(text string) error {
		codexCallGuarded(func() { fn(text) }, true, "Codex stream "+label+" raised")
		return nil
	}
}

// Runs the event hook, a panic or a non control flow error is logged and swallowed
func codexRunEventHook(onEvent func(event any) error, event any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Codex stream on_event hook raised", "panic", r)
			err = nil
		}
	}()
	if hookErr := onEvent(event); hookErr != nil {
		if codexStreamControlFlow(hookErr) {
			return hookErr
		}
		slog.Debug("Codex stream on_event hook raised", "error", hookErr)
	}
	return nil
}

// Folds a Codex SSE event stream into a Responses object through the assembler and the callbacks
func consumeCodexEventStream(ctx context.Context, next func(ctx context.Context) (any, error), model string, o codexConsumeOptions) (map[string]any, error) {
	assembler := newCodexAssembler(model)
	assembler.textFn = codexGuardedText(o.onTextDelta, "on_text_delta")
	assembler.reasoningFn = codexGuardedText(o.onReasoningDelta, "on_reasoning_delta")
	assembler.commentaryFn = codexGuardedText(o.onCommentaryMessage, "on_commentary_message")
	if o.onFirstDelta != nil {
		assembler.firstDeltaFn = func() error {
			codexCallGuarded(o.onFirstDelta, true, "Codex stream on_first_delta raised")
			return nil
		}
	}
	for {
		event, err := next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if o.onEvent != nil {
			if err := codexRunEventHook(o.onEvent, event); err != nil {
				return nil, err
			}
		}
		if o.interruptCheck != nil {
			stop, err := o.interruptCheck()
			if err != nil {
				return nil, err
			}
			if stop {
				break
			}
		}
		eventMap, ok := event.(map[string]any)
		if !ok {
			eventMap = relayJSONableDict(event)
		}
		done, err := assembler.feed(eventMap)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	return assembler.result()
}

// Event source over a captured event list
func codexSliceEvents(events []any) func(ctx context.Context) (any, error) {
	i := 0
	return func(context.Context) (any, error) {
		if i >= len(events) {
			return nil, io.EOF
		}
		i++
		return events[i-1], nil
	}
}

// Implemented by agents that report the ChatGPT OAuth Codex backend
type codexBackendReporter interface{ IsCodexBackend() bool }

// Implemented by agents whose display can switch commentary off
type codexCommentaryToggle interface{ ShowCommentary() bool }

// Implemented by agents that report an installed interim assistant callback
type codexInterimCallbackReporter interface{ HasInterimAssistantCallback() bool }

// Implemented by agents that deliver a completed live Codex commentary message
type codexCommentaryFirer interface{ FireStreamedCodexCommentary(text string) }

// Implemented by agents that build the shared primary OpenAI client on demand
type codexPrimaryClientEnsurer interface {
	EnsurePrimaryOpenAIClient(ctx context.Context, reason string) (any, error)
}

// Implemented by agents that expose when the current call accepted its first chunk
type codexFirstChunkReader interface{ LastAPIFirstChunkAt() *float64 }

// Implemented by agents that keep the streamed Codex text parts of the current request
type codexStreamedTextKeeper interface {
	ResetCodexStreamedTextParts()
	AppendCodexStreamedTextPart(text string)
	CodexStreamedTextParts() []string
}

// Implemented by SDK clients that open a Responses API request
type codexResponsesCreator interface {
	CreateResponse(ctx context.Context, kwargs map[string]any) (any, error)
}

// Copy of the request without prompt_cache_retention, top level or in extra_body, for the ChatGPT OAuth Codex backend
func sanitizeConsumerCodexRequest(agent any, request map[string]any) map[string]any {
	sanitized := make(map[string]any, len(request))
	for k, v := range request {
		sanitized[k] = v
	}
	backend, ok := agent.(codexBackendReporter)
	if !ok || !backend.IsCodexBackend() {
		return sanitized
	}
	var droppedFrom []string
	if _, has := sanitized["prompt_cache_retention"]; has {
		droppedFrom = append(droppedFrom, "top-level")
	}
	delete(sanitized, "prompt_cache_retention")
	if extraBody, ok := sanitized["extra_body"].(map[string]any); ok {
		if _, has := extraBody["prompt_cache_retention"]; has {
			rest := make(map[string]any, len(extraBody))
			for k, v := range extraBody {
				if k != "prompt_cache_retention" {
					rest[k] = v
				}
			}
			sanitized["extra_body"] = rest
			if len(rest) == 0 {
				delete(sanitized, "extra_body")
			}
			droppedFrom = append(droppedFrom, "extra_body")
		}
	}
	if len(droppedFrom) > 0 {
		model, has := sanitized["model"]
		if !has {
			model = "unknown"
			if m, ok := agent.(interface{ Model() string }); ok {
				model = m.Model()
			}
		}
		slog.Warn(fmt.Sprintf("Dropped unsupported prompt_cache_retention at consumer Codex wire boundary (model=%s, via %s).",
			pyrepr.Str(model), strings.Join(droppedFrom, ", ")))
	}
	return sanitized
}

func SanitizeConsumerCodexRequest(agent any, request map[string]any) map[string]any {
	return sanitizeConsumerCodexRequest(agent, request)
}

// Relay managed stream surface a Codex attempt consumes, closes and reads the finalizer response from
type codexManagedStream interface {
	Next(ctx context.Context) (any, error)
	Close() error
	finalResponse() any
}

// Relay stream constructor seam of a Codex attempt
var codexRelayStreamFn = func(ctx context.Context, request map[string]any, factory func(ctx context.Context, request map[string]any) (any, error), o ManagedLlmStreamOptions) (codexManagedStream, error) {
	s, err := NewManagedLlmStream(ctx, request, factory, o)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Python TimeoutError raised when a Codex request is retired or killed
func codexTimeoutError(message string) error {
	return &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: errors.New(message)}
}

// Whether an error is one of the httpx transport classes or a builtin ConnectionError the stream retries
func codexIsStreamTransportError(err error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case *AuxTransportError:
		switch e.Class {
		case "RemoteProtocolError", "ReadTimeout", "ReadError", "ConnectError":
			return true
		}
		return false
	case *AuxSDKError, *TimeoutPhaseError, *InterruptedError:
		return false
	}
	return codexIsConnectionError(err)
}

// Whether an error carries a connection refused, reset, aborted or broken pipe errno
func codexIsConnectionError(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.EPIPE, syscall.ESHUTDOWN} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// Whether an error is the SDK APIConnectionError or its APITimeoutError subclass
func codexIsAPIConnectionError(err error) bool {
	e, ok := err.(*AuxSDKError)
	return ok && (e.Class == "APIConnectionError" || e.Class == "APITimeoutError")
}

// Whether an SDK connection error was caused by an httpx transport failure
func codexCauseIsHTTPXTransport(err error) bool {
	e, ok := err.(*AuxSDKError)
	if !ok || e.Err == nil {
		return false
	}
	cause := e.Err
	for {
		wrapped, ok := cause.(*StreamRequestError)
		if !ok || wrapped.Err == nil {
			break
		}
		cause = wrapped.Err
	}
	switch c := cause.(type) {
	case *AuxTransportError:
		return c.Class != "JSONDecodeError"
	case *keepaliveTimeoutError:
		return true
	}
	var ne net.Error
	return errors.As(cause, &ne) || codexIsConnectionError(cause) || errors.Is(cause, io.ErrUnexpectedEOF)
}

// Whether an error is a Python RuntimeError, the missing terminal response among them
func codexIsRuntimeError(err error) bool {
	if errors.Is(err, errCodexNoTerminal) {
		return true
	}
	switch e := err.(type) {
	case *AuxRuntimeError:
		return true
	case *tools.RelayError:
		return e.Class == "RuntimeError"
	}
	return false
}

// Python identity of two request tokens, false for values that cannot be compared
func codexSameToken(a, b any) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// Delta sink claim of the current physical attempt and the raw stream it opened
type codexWriterClaim struct {
	mu              sync.Mutex
	opened          bool
	token           int
	rawStream       any
	supersedeLogged bool
}

func (w *codexWriterClaim) reset() {
	w.mu.Lock()
	w.opened, w.token, w.rawStream, w.supersedeLogged = false, 0, nil, false
	w.mu.Unlock()
}

// Runs one streaming Responses API request over raw create events with one physical retry
func RunCodexStream(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any, client any, onFirstDelta func()) (any, error) {
	activeClient := client
	if !pyrepr.Truthy(activeClient) {
		ensurer, ok := agent.(codexPrimaryClientEnsurer)
		if !ok {
			return nil, &auxAttributeError{typeName: pyrepr.TypeName(agent), attr: "_ensure_primary_openai_client"}
		}
		var err error
		if activeClient, err = ensurer.EnsurePrimaryOpenAIClient(ctx, "codex_stream_direct"); err != nil {
			return nil, err
		}
	}
	const maxStreamRetries = 1
	model := apiKwargs["model"]
	modelText := ""
	if model != nil {
		modelText = pyrepr.Str(model)
	}
	var localParts []string
	keeper, keeps := agent.(codexStreamedTextKeeper)
	if keeps {
		keeper.ResetCodexStreamedTextParts()
	}
	watchdog := CodexWatchdogStateFrom(ctx)
	var requestToken any
	if watchdog != nil {
		requestToken = watchdog.Token
	} else {
		requestToken = agent.ActiveCodexStreamRequestToken()
	}
	writer := &codexWriterClaim{}
	attempt := 0
	clientBaseURL := func() string {
		if b, ok := activeClient.(interface{ BaseURL() string }); ok {
			return b.BaseURL()
		}
		return ""
	}

	requestIsCurrent := func() bool {
		return requestToken == nil || codexSameToken(agent.ActiveCodexStreamRequestToken(), requestToken)
	}
	writerIsCurrent := func() bool {
		writer.mu.Lock()
		opened, token := writer.opened, writer.token
		writer.mu.Unlock()
		if !opened || StreamWriterIsCurrent(agent, token) {
			return true
		}
		writer.mu.Lock()
		logged := writer.supersedeLogged
		writer.supersedeLogged = true
		writer.mu.Unlock()
		if !logged {
			kwModel, has := apiKwargs["model"]
			if !has {
				kwModel = "unknown"
			}
			slog.Warn(fmt.Sprintf("Codex streaming attempt superseded by a newer stream; suppressing its live deltas while "+
				"consuming to completion so the final response is not truncated (model=%s).", pyrepr.Str(kwModel)))
		}
		return false
	}
	live := func(fn func(string)) func(string) {
		return func(text string) {
			if requestIsCurrent() && writerIsCurrent() {
				fn(text)
			}
		}
	}
	onTextDelta := func(text string) {
		if !requestIsCurrent() {
			return
		}
		if keeps {
			keeper.AppendCodexStreamedTextPart(text)
		} else {
			localParts = append(localParts, text)
		}
		if writerIsCurrent() {
			agent.FireStreamDelta(text)
		}
	}
	onEvent := func(event any) error {
		if !requestIsCurrent() {
			return nil
		}
		now := epochSeconds(agentNonStreamClock())
		if reader, ok := agent.(codexFirstChunkReader); !ok || reader.LastAPIFirstChunkAt() == nil {
			agent.SetLastAPIFirstChunkAt(now)
		}
		eventMap, isMap := event.(map[string]any)
		if !isMap {
			eventMap = relayJSONableDict(event)
		}
		hasProgress := codexEventHasContent(eventMap)
		firstEvent, firstProgress := false, false
		if watchdog != nil {
			watchdog.Mu.Lock()
			firstEvent = watchdog.LastEventTs == nil
			stamp := now
			watchdog.LastEventTs = &stamp
			if hasProgress {
				firstProgress = watchdog.LastProgressTs == nil
				progress := now
				watchdog.LastProgressTs = &progress
				if watchdog.PhaseAware {
					watchdog.RetryStartedTs = nil
				}
			}
			watchdog.Mu.Unlock()
		}
		if firstEvent {
			slog.Info(fmt.Sprintf("Codex stream first parsed event at %.3f (attempt=%d/%d, model=%s)", now, attempt+1, maxStreamRetries+1, pyrepr.Str(model)))
		}
		if firstProgress {
			slog.Info(fmt.Sprintf("Codex stream first substantive progress at %.3f (attempt=%d/%d, model=%s)", now, attempt+1, maxStreamRetries+1, pyrepr.Str(model)))
		}
		agent.TouchActivity("receiving stream response")
		return nil
	}
	interruptOrSuperseded := func() (bool, error) {
		if !requestIsCurrent() {
			return false, codexTimeoutError("Codex Responses stream request retired before terminal response")
		}
		return agent.InterruptRequested(), nil
	}
	openCodexStream := func(ctx context.Context, nextAPIKwargs map[string]any) (any, error) {
		if IsActualRoute(agent.Provider(), clientBaseURL()) {
			return nil, &ValueError{Message: "Actual requests require Chat Completions; refusing to call /responses."}
		}
		streamKwargs := sanitizeConsumerCodexRequest(agent, nextAPIKwargs)
		streamKwargs["stream"] = true
		creator, ok := activeClient.(codexResponsesCreator)
		if !ok {
			return nil, &auxAttributeError{typeName: pyrepr.TypeName(activeClient), attr: "responses"}
		}
		return creator.CreateResponse(ctx, BypassSDKRequestTransform(streamKwargs, nil, false))
	}
	logFailure := func(exc error) {
		bodyBytes, chain := codexRequestFailureDetails(exc)
		size := "unknown"
		if bodyBytes != nil {
			size = fmt.Sprint(*bodyBytes)
		}
		writer.mu.Lock()
		opened := writer.opened
		writer.mu.Unlock()
		slog.Warn(fmt.Sprintf("Codex Responses request failed: serialized_request_body_bytes=%s stream_opened=%t exception_chain=%s model=%s attempt=%d/%d",
			size, opened, chain, agent.Model(), attempt+1, maxStreamRetries+1))
		if !opened {
			baseURL := clientBaseURL()
			if baseURL == "" {
				baseURL = agent.BaseURL()
			}
			BufferConnectExhaustedNotice(agent, exc, attempt+1, baseURL)
		}
	}
	streamCreated := func(_ context.Context, raw any) error {
		token := ClaimStreamWriter(agent)
		writer.mu.Lock()
		writer.opened, writer.token, writer.rawStream = true, token, raw
		writer.mu.Unlock()
		slog.Debug(fmt.Sprintf("Codex stream opened (attempt=%d/%d, model=%s)", attempt+1, maxStreamRetries+1, pyrepr.Str(model)))
		return nil
	}
	drainForFinalizer := func(ctx context.Context, eventStream codexManagedStream) {
		budget := codexStreamDrainTimeoutFn(ctx)
		if budget <= 0 {
			return
		}
		writer.mu.Lock()
		raw := writer.rawStream
		writer.mu.Unlock()
		sock := codexStreamSocketFn(raw)
		if sock == nil {
			slog.Debug("Codex post-terminal drain skipped: no interruptible stream socket found. " + agent.ClientLogContext())
			return
		}
		var timedOut atomic.Bool
		var timer *time.Timer
		fired := make(chan struct{})
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug("Codex Responses stream finalization failed after a terminal response", "panic", r)
				}
				if timer != nil && !timer.Stop() {
					<-fired
				}
			}()
			if !math.IsInf(budget, 1) {
				timer = time.AfterFunc(secondsDuration(budget), func() {
					defer close(fired)
					timedOut.Store(true)
					codexShutdownSocketFn(sock)
				})
			}
			for {
				_, err := eventStream.Next(ctx)
				if errors.Is(err, io.EOF) {
					return
				}
				if err == nil {
					continue
				}
				if codexIsStreamTransportError(err) || codexIsAPIConnectionError(err) {
					if !timedOut.Load() {
						if !codexIsStreamTransportError(err) {
							logFailure(err)
						}
						slog.Warn(fmt.Sprintf("Codex Responses stream transport finalization failed after a terminal response was already "+
							"received; returning the completed response instead of retrying. %s error=%s", agent.ClientLogContext(), err))
					}
					return
				}
				slog.Debug("Codex Responses stream finalization failed after a terminal response", "error", err)
				return
			}
		}()
		if timedOut.Load() {
			slog.Warn(fmt.Sprintf("Codex Responses stream remained open %.1fs after a terminal response "+
				"(agent.stream_drain_timeout); shut down its socket and completed cleanup on the owner thread. %s", budget, agent.ClientLogContext()))
		}
	}
	closeEventStream := func(eventStream any) (err error) {
		closer, ok := eventStream.(interface{ Close() error })
		if !ok || closer == nil {
			return nil
		}
		failed := false
		func() {
			defer func() {
				if recover() != nil {
					failed = true
				}
			}()
			failed = closer.Close() != nil
		}()
		if failed && client != nil {
			return agent.AbortRequestOpenAIClient(activeClient, "codex_stream_close_failed")
		}
		return nil
	}

	showCommentary := true
	if t, ok := agent.(codexCommentaryToggle); ok {
		showCommentary = t.ShowCommentary()
	}
	wantsCommentary := false
	if r, ok := agent.(codexInterimCallbackReporter); ok {
		wantsCommentary = r.HasInterimAssistantCallback() && showCommentary
	}
	var onCommentaryMessage func(string)
	if wantsCommentary {
		firer, ok := agent.(codexCommentaryFirer)
		if !ok {
			return nil, &auxAttributeError{typeName: pyrepr.TypeName(agent), attr: "_fire_streamed_codex_commentary"}
		}
		onCommentaryMessage = live(firer.FireStreamedCodexCommentary)
	}
	callRole := "primary"
	if agent.IsSubagent() {
		callRole = "delegated"
	} else if agent.FallbackIndex() > 0 {
		callRole = "fallback"
	}
	var onFirst func()
	if onFirstDelta != nil {
		onFirst = func() {
			if requestIsCurrent() && writerIsCurrent() {
				onFirstDelta()
			}
		}
	}
	name := agent.Provider()
	if name == "" {
		name = "codex"
	}
	modelName := ""
	if pyrepr.Truthy(model) {
		modelName = modelText
	}

	runAttempt := func() (result any, retry bool, err error) {
		var eventsMu sync.Mutex
		var intercepted []any
		writer.reset()
		var eventStream codexManagedStream
		defer func() {
			var closeErr error
			if eventStream == nil {
				writer.mu.Lock()
				raw := writer.rawStream
				writer.mu.Unlock()
				closeErr = closeEventStream(raw)
			} else {
				closeErr = closeEventStream(eventStream)
			}
			if closeErr != nil {
				result, retry, err = nil, false, closeErr
			}
		}()
		var requestID any
		if id := agent.CurrentAPIRequestID(); id != "" {
			requestID = id
		}
		request := make(map[string]any, len(apiKwargs))
		for k, v := range apiKwargs {
			request[k] = v
		}
		var final map[string]any
		eventStream, err = codexRelayStreamFn(ctx, request, openCodexStream, ManagedLlmStreamOptions{
			SessionID: agent.SessionID(), Name: name, ModelName: modelName,
			Finalizer: func(ctx context.Context) (any, error) {
				eventsMu.Lock()
				captured := append([]any(nil), intercepted...)
				eventsMu.Unlock()
				return consumeCodexEventStream(ctx, codexSliceEvents(captured), modelText, codexConsumeOptions{})
			},
			OnStreamCreated: streamCreated,
			OnChunk: func(_ context.Context, chunk any) error {
				eventsMu.Lock()
				intercepted = append(intercepted, chunk)
				eventsMu.Unlock()
				return nil
			},
			ChunkAdapter: func(chunk any) any { return chunk },
			CompletedResponsePredicate: func(value any) bool {
				m, ok := value.(map[string]any)
				if !ok {
					return false
				}
				_, hasOutput := m["output"]
				return hasOutput
			},
			Metadata: map[string]any{"api_mode": "codex_responses", "call_role": callRole, "retry_count": attempt,
				"api_request_id": requestID},
			DeferLogicalCompletion: true,
		})
		if err == nil {
			final, err = consumeCodexEventStream(ctx, eventStream.Next, modelText, codexConsumeOptions{
				onTextDelta: onTextDelta, onReasoningDelta: live(agent.FireReasoningDelta), onCommentaryMessage: onCommentaryMessage,
				onFirstDelta: onFirst, onEvent: onEvent, interruptCheck: interruptOrSuperseded,
			})
		}
		if err != nil {
			switch {
			case codexIsStreamTransportError(err):
				if attempt >= maxStreamRetries {
					logFailure(err)
					return nil, false, err
				}
				format := "Codex Responses stream transport failed mid-iteration (attempt %d/%d); retrying. %s error=%s"
				if eventStream == nil {
					format = "Codex Responses stream connect failed (attempt %d/%d); retrying. %s error=%s"
				}
				slog.Debug(fmt.Sprintf(format, attempt+1, maxStreamRetries+1, agent.ClientLogContext(), err))
				eventsMu.Lock()
				zeroEvents := len(intercepted) == 0
				eventsMu.Unlock()
				if zeroEvents {
					apiKwargs = pruneZeroEventRetryPayload(ctx, apiKwargs, attempt+1, maxStreamRetries+1)
				}
				return nil, true, nil
			case codexIsRuntimeError(err):
				if eventStream != nil {
					if final := eventStream.finalResponse(); final != nil {
						return final, false, nil
					}
				}
				return nil, false, err
			case codexIsAPIConnectionError(err):
				writer.mu.Lock()
				opened := writer.opened
				writer.mu.Unlock()
				if attempt < maxStreamRetries && !opened && codexCauseIsHTTPXTransport(err) {
					slog.Debug(fmt.Sprintf("Codex Responses pre-stream connect failed (attempt %d/%d); retrying. %s error=%s",
						attempt+1, maxStreamRetries+1, agent.ClientLogContext(), err))
					return nil, true, nil
				}
				logFailure(err)
				return nil, false, err
			}
			return nil, false, err
		}
		if !agent.InterruptRequested() {
			drainForFinalizer(ctx, eventStream)
		}
		if status := stringOf(final["status"]); status == "incomplete" || status == "failed" {
			streamed := 0
			parts := localParts
			if keeps {
				parts = keeper.CodexStreamedTextParts()
			}
			for _, p := range parts {
				streamed += len([]rune(p))
			}
			slog.Warn(fmt.Sprintf("Codex Responses stream terminal status=%s (incomplete_details=%s, error=%s, streamed_chars=%d). %s",
				status, pyrepr.Str(final["incomplete_details"]), pyrepr.Str(final["error"]), streamed, agent.ClientLogContext()))
		}
		return final, false, nil
	}

	for attempt = 0; attempt <= maxStreamRetries; attempt++ {
		if !requestIsCurrent() {
			return nil, codexTimeoutError("Codex Responses stream request retired before retry")
		}
		if agent.InterruptRequested() {
			return nil, &InterruptedError{Message: "Agent interrupted before Codex stream retry"}
		}
		if attempt > 0 && watchdog != nil && watchdog.PhaseAware {
			watchdog.Mu.Lock()
			started := epochSeconds(agentNonStreamClock())
			watchdog.RetryStartedTs, watchdog.LastEventTs, watchdog.LastProgressTs = &started, nil, nil
			slog.Info(fmt.Sprintf("Codex physical stream retry at %.3f (attempt=%d/%d, model=%s)", started, attempt+1, maxStreamRetries+1, pyrepr.Str(model)))
			watchdog.Mu.Unlock()
		}
		result, retry, err := runAttempt()
		if retry {
			continue
		}
		return result, err
	}
	return nil, nil
}
