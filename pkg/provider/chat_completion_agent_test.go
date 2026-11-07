package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeChatClient struct {
	create func(ctx context.Context, kwargs map[string]any) (any, error)
	calls  atomic.Int32
}

func (c *fakeChatClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	c.calls.Add(1)
	return c.create(ctx, kwargs)
}

type fakeMoaFacade struct{ fakeChatClient }

func (*fakeMoaFacade) PrepareRequest(messages []map[string]any) (map[string]any, error) {
	return map[string]any{"messages": messages}, nil
}

type clientCall struct {
	client any
	reason string
}

type fakeChatAgent struct {
	mu                                       sync.Mutex
	provider, apiMode, baseURL, model, plat  string
	verbose, interrupted, headed             bool
	retry                                    RetrySession
	client                                   any
	credits                                  *CreditsCapture
	resolve                                  func(map[string]any) (any, error)
	create                                   func(reason string, kwargs map[string]any) (any, error)
	onAbort                                  func(client any, reason string)
	aborts, closes                           []clientCall
	touches                                  []string
	statuses                                 []string
	hook                                     *RequestAbortHook
	reasoning, wire                          map[string]any
	ephemeralOff, effortRej, disableRej, flr bool
	ollamaCtx                                int
	ollamaThinks                             bool
	pool                                     PoolProbe
	poolCleared                              bool
	browserErr                               error
	browserCleaned                           []string
	hint                                     func(model any) (string, error)
	codexStream                              func(ctx context.Context, kw map[string]any) (any, error)
	anthropicCreate                          func(reason string) (any, error)
	codexToken                               any
	notices, diagWaits                       []string
	aAborts, aCloses                         []clientCall
	sum                                      *summaryFake
	strm                                     *streamFake
}

func (a *fakeChatAgent) StreamProvider() string { return a.provider }
func (a *fakeChatAgent) StreamBaseURL() string  { return a.baseURL }
func (a *fakeChatAgent) BufferDiagnosticStatus(text string) {
	a.mu.Lock()
	a.statuses = append(a.statuses, text)
	a.mu.Unlock()
}
func (a *fakeChatAgent) TouchActivity(desc string) {
	a.mu.Lock()
	a.touches = append(a.touches, desc)
	a.mu.Unlock()
}
func (a *fakeChatAgent) touchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.touches)
}
func (a *fakeChatAgent) Provider() string                { return a.provider }
func (a *fakeChatAgent) CreditsCapture() *CreditsCapture { return a.credits }
func (a *fakeChatAgent) APIMode() string                 { return a.apiMode }
func (a *fakeChatAgent) BaseURL() string                 { return a.baseURL }
func (a *fakeChatAgent) Model() string                   { return a.model }
func (a *fakeChatAgent) Platform() string                { return a.plat }
func (a *fakeChatAgent) VerboseLogging() bool            { return a.verbose }
func (a *fakeChatAgent) RetrySession() *RetrySession     { return &a.retry }
func (a *fakeChatAgent) InterruptRequested() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interrupted
}
func (a *fakeChatAgent) Client() any                     { return a.client }
func (a *fakeChatAgent) CodexOnFirstDelta() func()       { return a.st().codexFirst }
func (a *fakeChatAgent) ReasoningConfig() map[string]any { return a.reasoning }
func (a *fakeChatAgent) EphemeralReasoningOff() bool     { return a.ephemeralOff }
func (a *fakeChatAgent) SetEphemeralReasoningOff(on bool) {
	a.ephemeralOff = on
}
func (a *fakeChatAgent) ReasoningEffortRejected() bool             { return a.effortRej }
func (a *fakeChatAgent) ReasoningDisableRejected() bool            { return a.disableRej }
func (a *fakeChatAgent) ReasoningFloorRequired() bool              { return a.flr }
func (a *fakeChatAgent) SetWireReasoningConfig(cfg map[string]any) { a.wire = cfg }
func (a *fakeChatAgent) OllamaNumCtx() int                         { return a.ollamaCtx }
func (a *fakeChatAgent) OllamaSupportsThinkingCached() bool        { return a.ollamaThinks }
func (a *fakeChatAgent) CredentialPool() PoolProbe                 { return a.pool }
func (a *fakeChatAgent) SetCredentialPool(pool PoolProbe)          { a.pool = pool }
func (a *fakeChatAgent) ClearCredentialPool()                      { a.pool, a.poolCleared = nil, true }
func (a *fakeChatAgent) CredentialPoolEnv() PoolEnv                { return PoolEnv{} }
func (a *fakeChatAgent) ToolEnv() *tools.Env                       { return nil }
func (a *fakeChatAgent) IsHeadedMode() (bool, error)               { return a.headed, nil }
func (a *fakeChatAgent) CleanupBrowser(taskID string) error {
	a.browserCleaned = append(a.browserCleaned, taskID)
	return a.browserErr
}
func (a *fakeChatAgent) ComputeNonStreamStaleTimeout(kw map[string]any) (any, error) {
	return a.resolve(kw)
}
func (a *fakeChatAgent) CreateRequestOpenAIClient(reason string, kw map[string]any) (any, error) {
	return a.create(reason, kw)
}
func (a *fakeChatAgent) AbortRequestOpenAIClient(client any, reason string) error {
	a.mu.Lock()
	a.aborts = append(a.aborts, clientCall{client, reason})
	a.mu.Unlock()
	if a.onAbort != nil {
		a.onAbort(client, reason)
	}
	return nil
}
func (a *fakeChatAgent) CloseRequestOpenAIClient(client any, reason string) error {
	a.mu.Lock()
	a.closes = append(a.closes, clientCall{client, reason})
	a.mu.Unlock()
	return nil
}
func (a *fakeChatAgent) ActiveRequestAbort() *RequestAbortHook {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hook
}
func (a *fakeChatAgent) SetActiveRequestAbort(hook *RequestAbortHook) {
	a.mu.Lock()
	a.hook = hook
	a.mu.Unlock()
}
func (a *fakeChatAgent) RunCodexStream(ctx context.Context, kw map[string]any, client any, onFirstDelta func()) (any, error) {
	if a.codexStream != nil {
		return a.codexStream(ctx, kw)
	}
	return client.(chatCompletionCreator).CreateChatCompletion(ctx, kw)
}
func (a *fakeChatAgent) AnthropicMessagesCreate(ctx context.Context, kw map[string]any, client any) (any, error) {
	return client.(chatCompletionCreator).CreateChatCompletion(ctx, kw)
}
func (a *fakeChatAgent) ChatCompletionCapturingHeaders(ctx context.Context, client any, kw map[string]any) (any, error) {
	a.s().sdkSeen = append(a.s().sdkSeen, kw)
	return client.(chatCompletionCreator).CreateChatCompletion(ctx, kw)
}
func (a *fakeChatAgent) CodexSilentHangHint(model any) (string, error) {
	if a.hint == nil {
		return "", nil
	}
	return a.hint(model)
}

func cronChatAgent(stale any) *fakeChatAgent {
	return &fakeChatAgent{plat: "cron", apiMode: "chat_completions", provider: "openrouter", model: "m",
		resolve: func(map[string]any) (any, error) { return stale, nil }}
}

// Wires a client whose request blocks until the agent aborts it
func stallingChatClient(a *fakeChatAgent) *fakeChatClient {
	release := make(chan struct{})
	var once sync.Once
	c := &fakeChatClient{create: func(ctx context.Context, kw map[string]any) (any, error) {
		<-release
		return nil, errors.New("ConnectionError: socket shut down")
	}}
	a.onAbort = func(any, string) { once.Do(func() { close(release) }) }
	a.create = func(string, map[string]any) (any, error) { return c, nil }
	return c
}

func TestInlineCodexKeepsTheLargeContextStaleFloorAndLocalKeepsItsBudget(t *testing.T) {
	t.Setenv("ICEBATH_CODEX_HARD_TIMEOUT_SECONDS", "1500")
	kw := map[string]any{"model": "gpt-5.5", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("x ", 60000)}}}
	hosted := &fakeChatAgent{provider: "openai-codex", apiMode: "codex_responses", baseURL: "https://chatgpt.com/backend-api/codex", plat: "cron",
		resolve: func(map[string]any) (any, error) { return 90.0, nil }}
	if !ShouldUseDirectAPICall(t.Context(), hosted.provider, hosted.plat, ModeCodexResponses) {
		t.Fatal("cron codex must run inline")
	}
	if got, err := ResolveDirectStaleTimeout(t.Context(), hosted, kw); err != nil || got < 600 {
		t.Fatalf("hosted %v %v", got, err)
	}
	local := &fakeChatAgent{provider: "custom", apiMode: "codex_responses", baseURL: "http://127.0.0.1:8080/v1", plat: "cron",
		resolve: func(map[string]any) (any, error) { return 3000.0, nil }}
	if got, err := ResolveDirectStaleTimeout(t.Context(), local, kw); err != nil || got != 3000 {
		t.Fatalf("local %v %v", got, err)
	}
	fakeAgent := &fakeChatAgent{apiMode: "chat_completions", resolve: func(map[string]any) (any, error) { return true, nil }}
	if got, _ := ResolveDirectStaleTimeout(t.Context(), fakeAgent, kw); got <= 1e300 {
		t.Fatalf("non numeric budget must disarm, got %v", got)
	}
}

func TestRetryAfterAnInlineWatchdogKillGetsAFreshClientAndSucceeds(t *testing.T) {
	a := cronChatAgent(0.2)
	stallingChatClient(a)
	_, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || a.retry.StaleStreak() != 1 {
		t.Fatalf("err %v streak %d", err, a.retry.StaleStreak())
	}
	if len(a.closes) != 1 || a.closes[0].reason != "request_error_cleanup" || len(a.aborts) != 1 || a.aborts[0].reason != "stale_call_kill" {
		t.Fatalf("closes %+v aborts %+v", a.closes, a.aborts)
	}
	healthy := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) { return map[string]any{"id": "ok"}, nil }}
	a.create = func(string, map[string]any) (any, error) { return healthy, nil }
	resp, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	if err != nil || resp.(map[string]any)["id"] != "ok" || a.retry.StaleStreak() != 0 {
		t.Fatalf("resp %v err %v streak %d", resp, err, a.retry.StaleStreak())
	}
	if a.closes[1].reason != "request_complete" || a.ActiveRequestAbort() != nil {
		t.Fatalf("closes %+v", a.closes)
	}
}

func TestCronTurnIsBoundedThroughTheInterruptibleRouting(t *testing.T) {
	a := cronChatAgent(0.3)
	var sent map[string]any
	c := stallingChatClient(a)
	inner := c.create
	c.create = func(ctx context.Context, kw map[string]any) (any, error) { sent = kw; return inner(ctx, kw) }
	_, err := InterruptibleAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || c.calls.Load() != 1 || len(a.closes) != 1 || sent["timeout"] != 0.3 {
		t.Fatalf("err %v calls %d closes %+v timeout %v", err, c.calls.Load(), a.closes, sent["timeout"])
	}
	var status bool
	for _, s := range a.statuses {
		status = status || strings.Contains(s, "No response from provider for 0s (non-streaming, model: m). Aborting call.")
	}
	if !status {
		t.Fatalf("statuses %v", a.statuses)
	}
}

func TestTimerFiringBeforeClientRegistrationFailsTheDispatch(t *testing.T) {
	a := cronChatAgent(0.05)
	late := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) { return map[string]any{"id": "late"}, nil }}
	a.create = func(string, map[string]any) (any, error) { time.Sleep(400 * time.Millisecond); return late, nil }
	_, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || late.calls.Load() != 0 {
		t.Fatalf("err %v calls %d", err, late.calls.Load())
	}
	if len(a.aborts) != 1 || a.aborts[0].client != late || a.aborts[0].reason != "stale_call_kill" {
		t.Fatalf("aborts %+v", a.aborts)
	}
}

func TestDirectStaleResolverErrorPropagates(t *testing.T) {
	a := cronChatAgent(0.2)
	a.resolve = func(map[string]any) (any, error) { return nil, errors.New("resolver regression") }
	a.create = func(string, map[string]any) (any, error) {
		t.Fatal("no client before the budget resolves")
		return nil, nil
	}
	if _, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m"}); err == nil || err.Error() != "resolver regression" {
		t.Fatalf("err %v", err)
	}
}

func TestDirectAPICallHeartbeatStopsOnError(t *testing.T) {
	prev := directAPIActivityHeartbeat
	directAPIActivityHeartbeat = 50 * time.Millisecond
	t.Cleanup(func() { directAPIActivityHeartbeat = prev })
	a := cronChatAgent(nil)
	a.plat = "subagent"
	down := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		time.Sleep(120 * time.Millisecond)
		return nil, errors.New("provider down")
	}}
	a.create = func(string, map[string]any) (any, error) { return down, nil }
	if _, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m"}); err == nil || err.Error() != "provider down" {
		t.Fatalf("err %v", err)
	}
	after := a.touchCount()
	if after < 2 {
		t.Fatalf("heartbeat never touched, %d", after)
	}
	time.Sleep(200 * time.Millisecond)
	if a.touchCount() != after {
		t.Fatalf("heartbeat touched after the call failed")
	}
}

func TestDirectAPICallInterruptAbortsTheActiveClientAndRaises(t *testing.T) {
	a := cronChatAgent(nil)
	ready := make(chan struct{})
	release := make(chan struct{})
	c := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		<-release
		return nil, errors.New("socket closed")
	}}
	a.create = func(string, map[string]any) (any, error) { close(ready); return c, nil }
	done := make(chan error, 1)
	go func() {
		_, err := DirectAPICall(context.Background(), a, map[string]any{"model": "m"})
		done <- err
	}()
	<-ready
	deadline := time.Now().Add(2 * time.Second)
	for a.ActiveRequestAbort() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	hook := a.ActiveRequestAbort()
	if hook == nil {
		t.Fatal("no abort hook published")
	}
	a.mu.Lock()
	a.interrupted = true
	a.mu.Unlock()
	hook.Abort("interrupt_abort")
	if len(a.aborts) != 1 || a.aborts[0].client != c || a.aborts[0].reason != "interrupt_abort" {
		t.Fatalf("aborts %+v", a.aborts)
	}
	close(release)
	err := <-done
	var ie *InterruptedError
	if !errors.As(err, &ie) || err.Error() != "Agent interrupted during API call" || a.retry.StaleStreak() != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestDispatchKeepsTheMoaPreparedRequestOnlyForTheFacade(t *testing.T) {
	var got map[string]any
	create := func(_ context.Context, kw map[string]any) (any, error) { got = kw; return "ok", nil }
	facade := &fakeMoaFacade{fakeChatClient{create: create}}
	a := &fakeChatAgent{provider: "moa", apiMode: "chat_completions", client: facade}
	noClient := func(string, string) (any, error) { t.Fatal("moa builds no request client"); return nil, nil }
	if _, err := DispatchNonstreamingAPIRequest(t.Context(), a, map[string]any{"_moa_prepared_request": 1}, noClient); err != nil || got["_moa_prepared_request"] != 1 {
		t.Fatalf("facade %v %v", got, err)
	}
	a.client = &fakeChatClient{create: create}
	if _, err := DispatchNonstreamingAPIRequest(t.Context(), a, map[string]any{"_moa_prepared_request": 1}, noClient); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["_moa_prepared_request"]; ok {
		t.Fatalf("native client kept the prepared request %v", got)
	}
	var reasons []string
	a = &fakeChatAgent{provider: "anthropic", apiMode: "anthropic_messages"}
	mk := func(reason, kind string) (any, error) {
		reasons = append(reasons, reason+"/"+kind)
		return &fakeChatClient{create: create}, nil
	}
	_, _ = DispatchNonstreamingAPIRequest(t.Context(), a, map[string]any{}, mk)
	a.apiMode = "codex_responses"
	_, _ = DispatchNonstreamingAPIRequest(t.Context(), a, map[string]any{}, mk)
	a.apiMode, a.provider = "chat_completions", "openrouter"
	_, _ = DispatchNonstreamingAPIRequest(t.Context(), a, map[string]any{}, mk)
	if strings.Join(reasons, ",") != "anthropic_messages_request/anthropic_messages,codex_stream_request/openai,chat_completion_request/openai" {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestReasoningConfigForWireAppliesTheRejectionRules(t *testing.T) {
	a := &fakeChatAgent{apiMode: "anthropic_messages", reasoning: map[string]any{"enabled": true, "effort": "max"}, effortRej: true}
	if ReasoningConfigForWire(t.Context(), a) != nil || a.wire != nil {
		t.Fatal("rejected effort must omit the reasoning fields")
	}
	a = &fakeChatAgent{apiMode: "anthropic_messages", reasoning: map[string]any{"enabled": false}, disableRej: true, flr: true}
	if got := ReasoningConfigForWire(t.Context(), a); got["effort"] != ReasoningFloorEffort || got["enabled"] != true || a.wire["effort"] != ReasoningFloorEffort {
		t.Fatalf("floor %v", got)
	}
	a.flr = false
	if ReasoningConfigForWire(t.Context(), a) != nil || a.wire != nil {
		t.Fatal("a rejected disable without a floor is dropped")
	}
	a = &fakeChatAgent{apiMode: "anthropic_messages", reasoning: map[string]any{"enabled": true, "effort": "high"}, disableRej: true, ephemeralOff: true}
	if got := ReasoningConfigForWire(t.Context(), a); got["effort"] != "high" || a.ephemeralOff {
		t.Fatalf("rejected disables resend the user config %v", got)
	}
	a = &fakeChatAgent{apiMode: "anthropic_messages", reasoning: map[string]any{"enabled": true, "effort": "high"}, ephemeralOff: true}
	if got := ReasoningConfigForWire(t.Context(), a); got["effort"] != "none" || got["enabled"] != false || a.ephemeralOff || a.reasoning["effort"] != "high" {
		t.Fatalf("one shot off %v", got)
	}
	if got := ReasoningConfigForWire(t.Context(), a); got["effort"] != "high" {
		t.Fatalf("the off override lasts one call %v", got)
	}
	a = &fakeChatAgent{apiMode: "anthropic_messages"}
	if ReasoningConfigForWire(t.Context(), a) != nil {
		t.Fatal("an unset effort off chat completions sends no config")
	}
}

type probePool struct {
	PoolProbe
	provider string
}

func (p probePool) Provider() string { return p.provider }

func TestRebindFallbackCredentialPoolClearsOnlyAnotherProvidersPool(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	logs := captureLogs(t)
	prev := nonStreamLog
	nonStreamLog = slog.Default()
	t.Cleanup(func() { nonStreamLog = prev })
	same := probePool{provider: " OpenRouter "}
	a := &fakeChatAgent{pool: same}
	RebindFallbackCredentialPool(a, "openrouter", "m")
	if a.poolCleared || a.pool != same {
		t.Fatal("same provider pool must be kept")
	}
	a = &fakeChatAgent{pool: probePool{provider: "anthropic"}}
	RebindFallbackCredentialPool(a, "openrouter", "m")
	if !a.poolCleared || logCount(logs, "Fallback to openrouter/m: clearing primary credential pool (pool_provider=anthropic)") != 1 {
		t.Fatalf("cleared %v", a.poolCleared)
	}
}

func TestCleanupTaskResourcesSkipsHeadedBrowsersAndLogsFailures(t *testing.T) {
	logs := captureLogs(t)
	prev := nonStreamLog
	nonStreamLog = slog.Default()
	t.Cleanup(func() { nonStreamLog = prev })
	a := &fakeChatAgent{headed: true, verbose: true}
	CleanupTaskResources(a, "t1")
	if len(a.browserCleaned) != 0 {
		t.Fatal("headed browser must be left to the reaper")
	}
	a = &fakeChatAgent{verbose: true, browserErr: errors.New("boom")}
	CleanupTaskResources(a, "t2")
	if len(a.browserCleaned) != 1 || logCount(logs, "Failed to cleanup browser for task t2: boom") != 1 {
		t.Fatalf("cleaned %v", a.browserCleaned)
	}
	a = &fakeChatAgent{browserErr: errors.New("quiet")}
	CleanupTaskResources(a, "t3")
	if logCount(logs, "Failed to cleanup browser for task t3") != 0 {
		t.Fatal("failures log only under verbose logging")
	}
}

type dumpModel struct{}

func (dumpModel) ModelDump(warnings bool) any { return map[string]any{"warnings": warnings} }

func TestDumpIfModelAndCodexHangHint(t *testing.T) {
	if got := DumpIfModel(dumpModel{}).(map[string]any); got["warnings"] != false {
		t.Fatalf("dump %v", got)
	}
	if DumpIfModel(7) != 7 {
		t.Fatal("plain value unchanged")
	}
	a := &fakeChatAgent{hint: func(model any) (string, error) { return "hint " + model.(string), nil }}
	if codexSilentHangHintFor(a, map[string]any{"model": "gpt"}) != "hint gpt" {
		t.Fatal("hint")
	}
	a.hint = func(any) (string, error) { return "x", errors.New("boom") }
	if codexSilentHangHintFor(a, map[string]any{}) != "" {
		t.Fatal("a failing hint answers none")
	}
}
