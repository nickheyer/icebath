package provider

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// CodexWatchdogState is the watchdog clock one codex request shares with the codex stream its worker runs
type CodexWatchdogState struct {
	Token          any
	Mu             sync.Mutex
	LastEventTs    *float64
	LastProgressTs *float64
	RetryStartedTs *float64
	PhaseAware     bool
}

type codexWatchdogStateKey struct{}

// Context whose codex stream reports its event, progress and reconnect markers to the state
func WithCodexWatchdogState(ctx context.Context, s *CodexWatchdogState) context.Context {
	return context.WithValue(ctx, codexWatchdogStateKey{}, s)
}

// Watchdog state of the codex request the context runs for, nil outside one
func CodexWatchdogStateFrom(ctx context.Context) *CodexWatchdogState {
	s, _ := ctx.Value(codexWatchdogStateKey{}).(*CodexWatchdogState)
	return s
}

type codexRequestToken struct{ _ byte }

// Identity of the goroutine role calling a request client registry
type registryCaller struct{ _ byte }

// Per request client or stream handle the worker owns and stranger goroutines may only abort
type requestClientRegistry struct {
	agent  ChatCompletionAgent
	mu     sync.Mutex
	client any
	kind   string
	owner  *registryCaller
}

func newRequestClientRegistry(agent ChatCompletionAgent) *requestClientRegistry {
	return &requestClientRegistry{agent: agent, kind: "openai"}
}

// Registers the client of a kind as owned by the caller and answers it
func (r *requestClientRegistry) setClient(client any, kind string, caller *registryCaller) any {
	r.mu.Lock()
	r.client, r.kind, r.owner = client, kind, caller
	r.mu.Unlock()
	return client
}

// StreamResponseHolder is a stream handle exposing the transport response that closes its connection
type StreamResponseHolder interface {
	StreamResponse() any
}

// Close function of a stream handle or of its response, nil when neither closes
func streamCloseCallable(stream any) func() error {
	owners := []any{stream, nil}
	if h, ok := stream.(StreamResponseHolder); ok {
		owners[1] = h.StreamResponse()
	}
	for _, owner := range owners {
		switch c := owner.(type) {
		case interface{ Close() error }:
			return c.Close
		case interface{ Close() }:
			return func() error { c.Close(); return nil }
		}
	}
	return nil
}

// Registers a closable stream handle so an interrupt closes the stream itself, else answers it untouched
func (r *requestClientRegistry) setStreamHandle(stream any, caller *registryCaller) any {
	if streamCloseCallable(stream) == nil {
		return stream
	}
	return r.setClient(stream, "stream", caller)
}

// Closes a stream handle and logs the outcome
func (r *requestClientRegistry) closeStreamHandle(stream any, reason string) {
	closeFn := streamCloseCallable(stream)
	if closeFn == nil {
		return
	}
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%v", p)
			}
		}()
		return closeFn()
	}()
	if err != nil {
		nonStreamLog.Debug(fmt.Sprintf("Streaming response handle close failed (%s): %s", reason, err))
		return
	}
	nonStreamLog.Info(fmt.Sprintf("Streaming response handle closed (%s)", reason))
}

// Aborts the sockets from a stranger goroutine, else pops and fully closes the client by its kind
func (r *requestClientRegistry) closeOnce(reason string, caller *registryCaller) error {
	r.mu.Lock()
	client, kind, owner := r.client, r.kind, r.owner
	if kind != "stream" && client != nil && owner != nil && owner != caller {
		defer r.mu.Unlock()
		if kind == "anthropic_messages" {
			return r.agent.AbortRequestAnthropicClient(client, reason)
		}
		return r.agent.AbortRequestOpenAIClient(client, reason)
	}
	r.client, r.owner = nil, nil
	r.mu.Unlock()
	if client == nil {
		return nil
	}
	switch kind {
	case "stream":
		r.closeStreamHandle(client, reason)
		return nil
	case "anthropic_messages":
		return r.agent.CloseRequestAnthropicClient(client, reason)
	}
	return r.agent.CloseRequestOpenAIClient(client, reason)
}

// Poll thresholds of one agent non streaming request, the codex watchdogs armed only for codex_responses
type agentNonstreamWatchdogs struct {
	codexWatchdogs
	codex bool
}

// Seconds of a stale budget an agent resolver answered, Python numeric semantics for bools
func staleSecondsOf(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("TypeError: '>' not supported between instances of 'float' and '%s'", pyrepr.TypeName(raw))
}

// Silence floor when the agent wire reasoning config is enabled at high or a stronger ladder level
func agentHighEffortSilenceFloor(cfg map[string]any) float64 {
	if cfg == nil {
		return 0
	}
	if enabled, ok := cfg["enabled"].(bool); ok && !enabled {
		return 0
	}
	effort := ""
	if e := cfg["effort"]; pyrepr.Truthy(e) {
		effort = strings.ToLower(strings.TrimSpace(pyrepr.Str(e)))
	}
	if idx := ladderIndex(effort); idx < 0 || idx < ladderIndex("high") {
		return 0
	}
	return HighEffortSilenceFloorSeconds
}

// Local provider stale ceiling from agent.local_stream_stale_timeout, overridden by the env
func localStreamStaleTimeoutDefault(ctx context.Context) float64 {
	return envFloat(LocalStreamStaleTimeoutEnv, loadWireSettings(ctx).localStreamStale)
}

var resolveAgentNonstreamWatchdogs = resolveAgentNonstreamWatchdogsOf

// Stale timeout of the agent plus the codex TTFB, idle and first progress watchdogs of the request
func resolveAgentNonstreamWatchdogsOf(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any) (agentNonstreamWatchdogs, error) {
	raw, err := agent.ComputeNonStreamStaleTimeout(apiKwargs)
	if err != nil {
		return agentNonstreamWatchdogs{}, err
	}
	stale, err := staleSecondsOf(raw)
	if err != nil {
		return agentNonstreamWatchdogs{}, err
	}
	codex := agent.APIMode() == string(ModeCodexResponses)
	codexBackend := ResponsesRouteIsCodexBackend(agent.Provider(), agent.BaseURL())
	estTokens := EstimateRequestContextTokens(ctx, apiKwargs)
	effortFloor := 0.0
	if codex {
		effortFloor = agentHighEffortSilenceFloor(agent.ReasoningConfig())
	}
	codexFloor := 0.0
	baseURL := agent.BaseURL()
	local := baseURL != "" && IsLocalEndpoint(baseURL)
	if codex && !local {
		codexFloor = CodexStaleTimeoutFloor(estTokens)
		stale = boundCodexStaleTimeout(stale, estTokens)
	}
	idleDefault := 12.0
	switch {
	case estTokens > 100000:
		idleDefault = 180
	case estTokens > 50000:
		idleDefault = 120
	case estTokens > 10000:
		idleDefault = 60
	}
	idleDefault = math.Max(effortFloor, idleDefault)
	ttfbEnabled := codex
	ttfbExplicit := envFloat(CodexTTFBTimeoutEnv, -1) != -1
	ttfb := envFloat(CodexTTFBTimeoutEnv, 120)
	switch {
	case ttfb <= 0:
		ttfbEnabled = false
	case codex && !local:
		disableAbove := envFloat(CodexTTFBDisableAboveEnv, 10000)
		if !envTruthy(CodexTTFBStrictEnv) && disableAbove > 0 && float64(estTokens) >= disableAbove && ttfb < idleDefault {
			nonStreamLog.Info(fmt.Sprintf("Scaling codex-responses no-event TTFB watchdog from %.0fs to %.0fs for large request (context=~%s tokens >= %.0f). Set %s=1 to keep the smaller cutoff.", ttfb, idleDefault, commaInt(estTokens), disableAbove, CodexTTFBStrictEnv))
			ttfb = idleDefault
		}
		if ttfbCap := envFloat(CodexTTFBMaxEnv, 0); ttfbCap > 0 && ttfb > ttfbCap {
			nonStreamLog.Info(fmt.Sprintf("Capping codex-responses no-event TTFB timeout from %.0fs to %.0fs (context=~%s tokens) per %s.", ttfb, ttfbCap, commaInt(estTokens), CodexTTFBMaxEnv))
			ttfb = ttfbCap
		}
	case !ttfbExplicit && local:
		if localCeiling := localStreamStaleTimeoutDefault(ctx); localCeiling > ttfb {
			nonStreamLog.Info(fmt.Sprintf("Local provider detected (%s) — no-event TTFB watchdog raised from %.0fs to %.0fs (agent.local_stream_stale_timeout); set %s for an explicit cutoff.", baseURL, ttfb, localCeiling, CodexTTFBTimeoutEnv))
			ttfb = localCeiling
		}
	}
	if ttfbEnabled && !ttfbExplicit {
		ttfb = math.Max(ttfb, effortFloor)
	}
	idleExplicit := envFloat(CodexEventStaleTimeoutEnv, -1) != -1
	idle := envFloat(CodexEventStaleTimeoutEnv, idleDefault)
	progressGated := codex && codexBackend && codexFloor > 0 && !idleExplicit
	w := agentNonstreamWatchdogs{codex: codex, codexWatchdogs: codexWatchdogs{stale: stale, estTokens: estTokens, ttfbEnabled: ttfbEnabled, ttfb: ttfb,
		idleEnabled: codex && idle > 0, idle: idle, idleRequiresProgress: progressGated}}
	if progressGated {
		w.progress = CodexFirstProgressTimeoutSeconds
	}
	return w, nil
}

// NonStreamWatchdogs is the resolved idle, TTFB and stale fuses of one agent request
type NonStreamWatchdogs struct {
	IdleEnabled, TTFBEnabled               bool
	IdleTimeout, TTFBTimeout, StaleTimeout float64
}

// Resolved idle, TTFB and stale fuses of an agent request for callers outside the package
func ResolveNonstreamWatchdogs(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any) (NonStreamWatchdogs, error) {
	w, err := resolveAgentNonstreamWatchdogs(ctx, agent, apiKwargs)
	return NonStreamWatchdogs{IdleEnabled: w.idleEnabled, TTFBEnabled: w.ttfbEnabled, IdleTimeout: w.idle, TTFBTimeout: w.ttfb, StaleTimeout: w.stale}, err
}

// Wall clock and poll wait of the agent request poll loop
var (
	agentNonStreamClock    = time.Now
	agentNonStreamPollWait = time.After
)

type nonStreamAgentResult struct {
	resp any
	err  error
}

// One agent non streaming request on a worker goroutine the caller polls for watchdog kills and interrupts
type nonStreamAgentRequest struct {
	ctx          context.Context
	staleCtx     context.Context
	agent        ChatCompletionAgent
	apiKwargs    map[string]any
	clients      *requestClientRegistry
	worker       *registryCaller
	poller       *registryCaller
	mu           sync.Mutex
	cancelled    bool
	codexRetired bool
	codexToken   any
	wd           agentNonstreamWatchdogs
	codexState   *CodexWatchdogState
	callStart    float64
	waiter       *nonStreamWaiter
	surface      func(StreamEvent)
	done         chan nonStreamAgentResult
	exited       chan struct{}
}

// Runs the agent's blocking call on a polled worker with the codex watchdogs, the stale kill and interrupts
func agentNonStreamRequest(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any) (any, error) {
	r := &nonStreamAgentRequest{ctx: ctx, staleCtx: withStaleStreak(ctx, agent.RetrySession()), agent: agent, apiKwargs: apiKwargs,
		clients: newRequestClientRegistry(agent), worker: &registryCaller{}, poller: &registryCaller{}, surface: agentRequestSurface(agent),
		done: make(chan nonStreamAgentResult, 1), exited: make(chan struct{})}
	if agent.APIMode() == string(ModeCodexResponses) {
		r.codexToken = &codexRequestToken{}
	}
	wd, err := resolveAgentNonstreamWatchdogs(ctx, agent, apiKwargs)
	if err != nil {
		return nil, err
	}
	r.wd = wd
	if r.codexToken != nil {
		r.codexState = &CodexWatchdogState{Token: r.codexToken, PhaseAware: wd.idleRequiresProgress}
	}
	r.callStart = epochSeconds(agentNonStreamClock())
	model := "the provider"
	if v, ok := apiKwargs["model"]; ok {
		model = pyrepr.Str(v)
	}
	r.waiter = newNonStreamWaiter(model, r.callStart, wd.codexWatchdogs, r.surface)
	return r.run()
}

func (r *nonStreamAgentRequest) retired() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codexRetired
}

func (r *nonStreamAgentRequest) wasCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

// Publishes this request's codex token on the agent unless a kill retired it before the worker started
func (r *nonStreamAgentRequest) installCodexRequestToken() {
	if r.codexToken != nil && !r.retired() {
		r.agent.SetActiveCodexStreamRequestToken(r.codexToken)
	}
}

// Retires the codex token so a worker still draining frames knows it no longer owns the turn
func (r *nonStreamAgentRequest) retireCodexRequestToken() {
	if r.codexToken == nil {
		return
	}
	r.mu.Lock()
	r.codexRetired = true
	r.mu.Unlock()
	if r.agent.ActiveCodexStreamRequestToken() == r.codexToken {
		r.agent.SetActiveCodexStreamRequestToken(nil)
	}
}

// Builds the per request client of the kind and registers it with the abort machinery
func (r *nonStreamAgentRequest) makeClient(reason, kind string) (any, error) {
	var client any
	var err error
	if kind == "anthropic_messages" {
		client, err = r.agent.CreateRequestAnthropicClient(reason)
	} else {
		client, err = r.agent.CreateRequestOpenAIClient(reason, r.apiKwargs)
	}
	if err != nil {
		return nil, err
	}
	return r.clients.setClient(client, kind, r.worker), nil
}

// Worker body that swallows the error its own forced close caused and closes the request client once
func (r *nonStreamAgentRequest) call(ctx context.Context) {
	var resp any
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				resp, err = nil, fmt.Errorf("%v", p)
			}
		}()
		r.installCodexRequestToken()
		resp, err = DispatchNonstreamingAPIRequest(ctx, r.agent, r.apiKwargs, r.makeClient)
	}()
	if err != nil {
		resp = nil
		switch {
		case r.retired():
			nonStreamLog.Info(fmt.Sprintf("Codex worker caught %s after request retirement — discarding the stale partial instead of surfacing it as a completed response. %s", goErrorTypeName(err), r.agent.ClientLogContext()))
			err = nil
		case r.wasCancelled():
			nonStreamLog.Debug(fmt.Sprintf("Non-streaming worker caught %s after request cancellation — exiting without surfacing a network error.", goErrorTypeName(err)))
			err = nil
		}
	}
	r.retireCodexRequestToken()
	reason := "request_error_cleanup"
	if resp != nil {
		reason = "request_complete"
	}
	if cerr := r.clients.closeOnce(reason, r.worker); cerr != nil {
		nonStreamLog.Error("Exception in non-streaming worker thread", "exc_info", cerr)
	}
	r.done <- nonStreamAgentResult{resp, err}
	close(r.exited)
}

// Kills the request client from the poll loop and retires the codex token, errors suppressed
func (r *nonStreamAgentRequest) abortRequest(reason string) {
	func() {
		defer func() { _ = recover() }()
		_ = r.clients.closeOnce(reason, r.poller)
	}()
	r.retireCodexRequestToken()
}

// Gives a killed worker two seconds to land its result, else the kill timeout
func (r *nonStreamAgentRequest) awaitWorkerAfterKill(finished bool, result nonStreamAgentResult, message string) nonStreamAgentResult {
	if !finished {
		select {
		case result = <-r.done:
		case <-time.After(2 * time.Second):
		}
	}
	if result.err == nil && result.resp == nil {
		result.err = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("%s", message)}
	}
	return result
}

func (r *nonStreamAgentRequest) model() string {
	if v, ok := r.apiKwargs["model"]; ok {
		return pyrepr.Str(v)
	}
	return "unknown"
}

// Watchdog markers of the current attempt under the state lock, all unset for a non codex request
func (r *nonStreamAgentRequest) snapshot() nonStreamSnapshot {
	s := r.codexState
	if s == nil {
		return nonStreamSnapshot{}
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return nonStreamSnapshot{lastEvent: s.LastEventTs, lastProgress: s.LastProgressTs, retryStarted: s.RetryStartedTs}
}

func withSilentHint(message, sep, hint string) string {
	if hint == "" {
		return message
	}
	return message + sep + hint
}

// Kills a codex attempt that sent no parsed event within the first event cutoff so the retry reconnects
func (r *nonStreamAgentRequest) ttfbKill(elapsed float64, finished bool, result nonStreamAgentResult) nonStreamAgentResult {
	hint := codexSilentHangHintFor(r.agent, r.apiKwargs)
	nonStreamLog.Warn(fmt.Sprintf("Codex stream produced no parsed stream event within TTFB cutoff (%.0fs > %.0fs, model=%s). Backend accepted the connection but sent no stream events. Killing connection so the retry loop can reconnect.", elapsed, r.wd.ttfb, r.model()))
	r.agent.BufferDiagnosticStatus(withSilentHint(fmt.Sprintf("⚠️ No first stream event from provider in %ds (codex stream, model: %s). Reconnecting.", int(elapsed), r.model()), " ", hint))
	r.abortRequest("codex_ttfb_kill")
	r.agent.EmitDiagnosticWait(fmt.Sprintf("⚠ no response from provider in %ds — reconnecting...", int(elapsed)))
	r.agent.TouchActivity(fmt.Sprintf("codex stream killed after %ds with no first stream event", int(elapsed)))
	return r.awaitWorkerAfterKill(finished, result, withSilentHint(fmt.Sprintf("Codex stream produced no parsed stream event within %ds (TTFB threshold: %ds)", int(elapsed), int(r.wd.ttfb)), ". ", hint))
}

// Kills a codex attempt whose stream opened but never made model progress
func (r *nonStreamAgentRequest) progressKill(elapsed float64, finished bool, result nonStreamAgentResult) nonStreamAgentResult {
	nonStreamLog.Warn(fmt.Sprintf("Codex stream produced lifecycle events but no substantive model progress for %.0fs (threshold %.0fs, model=%s, context=~%s tokens). Reconnecting.", elapsed, r.wd.progress, r.model(), commaInt(r.wd.estTokens)))
	r.agent.BufferDiagnosticStatus(fmt.Sprintf("⚠️ Codex stream opened but made no model progress for %ds (model: %s). Reconnecting.", int(elapsed), r.model()))
	r.abortRequest("codex_progress_kill")
	r.agent.TouchActivity(fmt.Sprintf("codex stream killed after %ds without model progress", int(elapsed)))
	return r.awaitWorkerAfterKill(finished, result, fmt.Sprintf("Codex stream produced no substantive model progress for %ds (progress threshold: %ds)", int(elapsed), int(r.wd.progress)))
}

// Kills a codex attempt whose events stopped after the phase specific idle arm point
func (r *nonStreamAgentRequest) idleKill(idle float64, finished bool, result nonStreamAgentResult) nonStreamAgentResult {
	armPoint := "the first parsed event"
	if r.wd.idleRequiresProgress {
		armPoint = "model progress began"
	}
	nonStreamLog.Warn(fmt.Sprintf("Codex stream produced no SSE events for %.0fs after %s (threshold %.0fs, model=%s, context=~%s tokens). Killing connection so the retry loop can reconnect.", idle, armPoint, r.wd.idle, r.model(), commaInt(r.wd.estTokens)))
	r.agent.BufferDiagnosticStatus(fmt.Sprintf("⚠️ Codex stream sent no events for %ds after %s (model: %s). Reconnecting.", int(idle), armPoint, r.model()))
	r.abortRequest("codex_stream_idle_kill")
	r.agent.TouchActivity(fmt.Sprintf("codex stream killed after %ds with no SSE events", int(idle)))
	return r.awaitWorkerAfterKill(finished, result, fmt.Sprintf("Codex stream produced no SSE events for %ds after %s (threshold: %ds)", int(idle), armPoint, int(r.wd.idle)))
}

// Kills a request with no response within the stale timeout and counts it toward the stale breaker
func (r *nonStreamAgentRequest) staleKill(elapsed float64, finished bool, result nonStreamAgentResult) nonStreamAgentResult {
	hint := codexSilentHangHintFor(r.agent, r.apiKwargs)
	reportStaleNonstreamKill(r.surface, r.model(), EstimateRequestContextTokens(r.ctx, r.apiKwargs), elapsed, r.wd.stale, false, hint)
	r.abortRequest("stale_call_kill")
	bumpStaleStreak(r.staleCtx)
	touchStaleKillActivity(r.surface, elapsed)
	return r.awaitWorkerAfterKill(finished, result, withSilentHint(fmt.Sprintf("Non-streaming API call timed out after %ds with no response (threshold: %ds)", int(elapsed), int(r.wd.stale)), ". ", hint))
}

// Marks the request cancelled, aborts its client and answers the interrupt once Relay can tear down
func (r *nonStreamAgentRequest) interrupt(elapsed float64) error {
	s := r.snapshot()
	recordInterruptedProviderWait(r.staleCtx, elapsed, r.wd.codex && s.lastEvent != nil)
	r.mu.Lock()
	r.cancelled = true
	r.mu.Unlock()
	nonStreamLog.Debug("Force-closing httpx client due to interrupt (not a network error).")
	r.abortRequest("interrupt_abort")
	joinWorkerForRelayTeardown(r.ctx, r.exited, "Non-streaming")
	return &InterruptedError{Message: "Agent interrupted during API call"}
}

// Polls the worker every 300ms for wait notices, the four kills and the agent interrupt
func (r *nonStreamAgentRequest) run() (any, error) {
	wd := r.wd
	if wd.codex {
		s := r.codexState
		s.Mu.Lock()
		s.LastEventTs, s.LastProgressTs, s.RetryStartedTs = nil, nil, nil
		s.Mu.Unlock()
	}
	r.agent.TouchActivity("waiting for non-streaming API response")
	workerCtx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	if r.codexState != nil {
		workerCtx = WithCodexWatchdogState(workerCtx, r.codexState)
	}
	go r.call(workerCtx)
	var result nonStreamAgentResult
	finished := false
poll:
	for !finished {
		select {
		case result = <-r.done:
			finished = true
		case <-agentNonStreamPollWait(nonStreamPollInterval):
		}
		now := epochSeconds(agentNonStreamClock())
		elapsed := now - r.callStart
		r.waiter.poll(now, r.snapshot())
		s := r.snapshot()
		attemptElapsed := now - r.waiter.attemptStarted(s)
		if wd.ttfbEnabled && s.lastEvent == nil && attemptElapsed > wd.ttfb {
			result = r.ttfbKill(attemptElapsed, finished, result)
			break poll
		}
		if r.waiter.preProgress(s) && attemptElapsed > wd.progress {
			result = r.progressKill(attemptElapsed, finished, result)
			break poll
		}
		if s.retryStarted == nil && wd.idleEnabled && s.lastEvent != nil && (!wd.idleRequiresProgress || s.lastProgress != nil) {
			if idle := now - *s.lastEvent; idle > wd.idle {
				result = r.idleKill(idle, finished, result)
				break poll
			}
		}
		if elapsed > wd.stale {
			result = r.staleKill(elapsed, finished, result)
			break poll
		}
		if r.agent.InterruptRequested() {
			return nil, r.interrupt(elapsed)
		}
	}
	if result.err != nil {
		return nil, result.err
	}
	if result.resp != nil {
		r.agent.RetrySession().ResetStaleStreak()
	}
	return result.resp, nil
}
