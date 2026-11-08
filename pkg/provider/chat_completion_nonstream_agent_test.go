package provider

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConnErr struct{}

func (*fakeConnErr) Error() string { return "Connection error." }

func (a *fakeChatAgent) CreateRequestAnthropicClient(reason string) (any, error) {
	return a.anthropicCreate(reason)
}
func (a *fakeChatAgent) AbortRequestAnthropicClient(client any, reason string) error {
	a.mu.Lock()
	a.aAborts = append(a.aAborts, clientCall{client, reason})
	a.mu.Unlock()
	if a.onAbort != nil {
		a.onAbort(client, reason)
	}
	return nil
}
func (a *fakeChatAgent) CloseRequestAnthropicClient(client any, reason string) error {
	a.mu.Lock()
	a.aCloses = append(a.aCloses, clientCall{client, reason})
	a.mu.Unlock()
	return nil
}
func (a *fakeChatAgent) ActiveCodexStreamRequestToken() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.codexToken
}
func (a *fakeChatAgent) SetActiveCodexStreamRequestToken(token any) {
	a.mu.Lock()
	a.codexToken = token
	a.mu.Unlock()
}
func (a *fakeChatAgent) EmitWaitNotice(text string) {
	a.mu.Lock()
	a.notices = append(a.notices, text)
	a.mu.Unlock()
}
func (a *fakeChatAgent) EmitDiagnosticWait(text string) {
	a.mu.Lock()
	a.diagWaits = append(a.diagWaits, text)
	a.mu.Unlock()
}
func (a *fakeChatAgent) ClientLogContext() string { return "client=fake" }

func (a *fakeChatAgent) reasons(calls *[]clientCall) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, c := range *calls {
		out = append(out, c.reason)
	}
	return out
}

func (a *fakeChatAgent) snapshotStrings(list *[]string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), *list...)
}

func (a *fakeChatAgent) setInterrupted(on bool) {
	a.mu.Lock()
	a.interrupted = on
	a.mu.Unlock()
}

// Codex agent on the hosted codex backend with a 60s stale budget and a fresh home
func agentFormCodexAgent(t *testing.T, provider, baseURL string) *fakeChatAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unsetEnvForTest(t, CodexTTFBTimeoutEnv, CodexTTFBMaxEnv, CodexTTFBDisableAboveEnv, CodexTTFBStrictEnv, CodexEventStaleTimeoutEnv, LocalStreamStaleTimeoutEnv)
	client := &fakeChatClient{}
	return &fakeChatAgent{provider: provider, apiMode: "codex_responses", baseURL: baseURL, model: "gpt-5.5", plat: "cli",
		resolve: func(map[string]any) (any, error) { return 60.0, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil }}
}

func anyContains(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func TestAgentNonStreamTTFBKillCarriesTheSilentHangHint(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	t.Setenv(CodexTTFBTimeoutEnv, "0.4")
	agent.hint = func(model any) (string, error) { return "Silent hang hint for " + model.(string), nil }
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		<-ctx.Done()
		return nil, errors.New("connection closed")
	}
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "Silent hang hint for gpt-5.5") || !strings.Contains(err.Error(), "TTFB threshold") {
		t.Fatalf("err %v", err)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "codex_ttfb_kill" {
		t.Fatalf("aborts %v", aborts)
	}
	if !anyContains(agent.snapshotStrings(&agent.statuses), "Silent hang hint for gpt-5.5") {
		t.Fatalf("statuses %v", agent.statuses)
	}
	if !anyContains(agent.snapshotStrings(&agent.diagWaits), "no response from provider in 0s") {
		t.Fatalf("diag waits %v", agent.diagWaits)
	}
}

func TestAgentNonStreamInstallsAndRetiresTheCodexRequestToken(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	t.Setenv(CodexTTFBTimeoutEnv, "1")
	var seen atomic.Value
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		token := agent.ActiveCodexStreamRequestToken()
		seen.Store(token != nil && token == CodexWatchdogStateFrom(ctx).Token)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if agent.ActiveCodexStreamRequestToken() == nil {
				return nil, errors.New("retired worker stream ended without terminal")
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil, errors.New("test timed out waiting for retirement")
	}
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	if ok, _ := seen.Load().(bool); !ok {
		t.Fatal("the request token must be installed before the worker runs")
	}
	if err == nil || !strings.Contains(err.Error(), "TTFB") || strings.Contains(err.Error(), "retired worker") {
		t.Fatalf("err %v", err)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "codex_ttfb_kill" {
		t.Fatalf("aborts %v", aborts)
	}
	if agent.ActiveCodexStreamRequestToken() != nil {
		t.Fatal("the kill must retire the token")
	}
}

func TestAgentNonStreamDoesNotKillWhileEventsFlow(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	t.Setenv(CodexTTFBTimeoutEnv, "0.4")
	sentinel := &struct{ ok bool }{true}
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		state := CodexWatchdogStateFrom(ctx)
		for i := 0; i < 8; i++ {
			now := epochSeconds(time.Now())
			state.Mu.Lock()
			state.LastEventTs, state.LastProgressTs = &now, &now
			state.Mu.Unlock()
			time.Sleep(100 * time.Millisecond)
		}
		return sentinel, nil
	}
	agent.retry.staleStreak.Store(2)
	resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	if err != nil || resp != sentinel {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 0 {
		t.Fatalf("aborts %v", aborts)
	}
	if closes := agent.reasons(&agent.closes); len(closes) != 1 || closes[0] != "request_complete" {
		t.Fatalf("closes %v", closes)
	}
	if agent.retry.StaleStreak() != 0 || agent.ActiveCodexStreamRequestToken() != nil {
		t.Fatalf("streak %d token %v", agent.retry.StaleStreak(), agent.ActiveCodexStreamRequestToken())
	}
}

// Wraps the resolver so the implicit policy keeps its branches while the test scales time
func shortenAgentWatchdogs(t *testing.T, edit func(*agentNonstreamWatchdogs)) {
	prev := resolveAgentNonstreamWatchdogs
	resolveAgentNonstreamWatchdogs = func(ctx context.Context, agent ChatCompletionAgent, kw map[string]any) (agentNonstreamWatchdogs, error) {
		w, err := prev(ctx, agent, kw)
		edit(&w)
		return w, err
	}
	t.Cleanup(func() { resolveAgentNonstreamWatchdogs = prev })
}

func TestAgentNonStreamLifecycleOnlyStreamHitsTheProgressBudget(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	agent.resolve = func(map[string]any) (any, error) { return 900.0, nil }
	shortenAgentWatchdogs(t, func(w *agentNonstreamWatchdogs) { w.idle, w.ttfb, w.progress = 2, 0.9, 0.9 })
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		state := CodexWatchdogStateFrom(ctx)
		for ctx.Err() == nil {
			now := epochSeconds(time.Now())
			state.Mu.Lock()
			state.LastEventTs = &now
			state.Mu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
		return nil, ctx.Err()
	}
	started := time.Now()
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.6-sol", "input": strings.Repeat("x", 40004)})
	if err == nil || !strings.Contains(err.Error(), "no substantive model progress") || time.Since(started) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(started))
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "codex_progress_kill" {
		t.Fatalf("aborts %v", aborts)
	}
}

func TestAgentNonStreamIdleKillScopesToThePhase(t *testing.T) {
	for _, mode := range []string{"initial_gap", "stall"} {
		t.Run(mode, func(t *testing.T) {
			agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
			agent.resolve = func(map[string]any) (any, error) { return 900.0, nil }
			t.Setenv(CodexTTFBTimeoutEnv, "2")
			t.Setenv(CodexTTFBStrictEnv, "1")
			shortenAgentWatchdogs(t, func(w *agentNonstreamWatchdogs) { w.idle = 0.8 })
			agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
				state := CodexWatchdogStateFrom(ctx)
				mark := func(progress bool) {
					now := epochSeconds(time.Now())
					state.Mu.Lock()
					state.LastEventTs = &now
					if progress {
						state.LastProgressTs = &now
					}
					state.Mu.Unlock()
				}
				mark(false)
				if mode == "initial_gap" {
					time.Sleep(1500 * time.Millisecond)
					mark(true)
					return "done", nil
				}
				mark(true)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.6-sol", "input": strings.Repeat("x", 40004)})
			aborts := agent.reasons(&agent.aborts)
			if mode == "initial_gap" {
				if err != nil || resp != "done" || len(aborts) != 0 {
					t.Fatalf("resp %v err %v aborts %v", resp, err, aborts)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "after model progress began") || len(aborts) != 1 || aborts[0] != "codex_stream_idle_kill" {
				t.Fatalf("err %v aborts %v", err, aborts)
			}
		})
	}
}

func TestAgentNonStreamHardCeilingReclaimsASilentStall(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	agent.resolve = func(map[string]any) (any, error) { return 0.6, nil }
	shortenAgentWatchdogs(t, func(w *agentNonstreamWatchdogs) { w.stale, w.ttfbEnabled = 0.6, false })
	agent.hint = func(any) (string, error) { return "", nil }
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": strings.Repeat("x", 40004)})
	if err == nil || !strings.Contains(err.Error(), "timed out after") || !strings.Contains(err.Error(), "with no response") || time.Since(started) > 3*time.Second {
		t.Fatalf("err %v", err)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "stale_call_kill" {
		t.Fatalf("aborts %v", aborts)
	}
	if agent.retry.StaleStreak() != 1 || !anyContains(agent.snapshotStrings(&agent.statuses), "Aborting call.") {
		t.Fatalf("streak %d statuses %v", agent.retry.StaleStreak(), agent.statuses)
	}
}

func TestAgentNonStreamStaleCloseIsSingleOwner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	aborted := make(chan struct{})
	var once sync.Once
	client := &fakeChatClient{create: func(ctx context.Context, kw map[string]any) (any, error) {
		select {
		case <-aborted:
		case <-time.After(5 * time.Second):
			t.Error("stale watchdog never aborted the request")
		}
		return nil, &fakeConnErr{}
	}}
	agent := &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m",
		resolve: func(map[string]any) (any, error) { return 0.5, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil },
		onAbort: func(any, string) { once.Do(func() { close(aborted) }) }}
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "m", "messages": []any{}})
	var ce *fakeConnErr
	if !errors.As(err, &ce) {
		t.Fatalf("the worker's own error must surface, got %v", err)
	}
	if closes := agent.reasons(&agent.closes); len(closes) != 1 || closes[0] != "request_error_cleanup" {
		t.Fatalf("closes %v", closes)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "stale_call_kill" {
		t.Fatalf("aborts %v", aborts)
	}
}

func TestAgentNonStreamBreakerShortCircuitsAndSuccessResets(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "3")
	client := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) { return "resp", nil }}
	agent := &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m",
		resolve: func(map[string]any) (any, error) { return 60.0, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil }}
	agent.retry.staleStreak.Store(3)
	if _, err := InterruptibleAPICall(t.Context(), agent, map[string]any{}); err == nil || !strings.Contains(err.Error(), "unresponsive") {
		t.Fatalf("err %v", err)
	}
	if client.calls.Load() != 0 || agent.retry.StaleStreak() != 3 {
		t.Fatalf("calls %d streak %d", client.calls.Load(), agent.retry.StaleStreak())
	}
	agent.retry.staleStreak.Store(2)
	if resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "m", "messages": []any{}}); err != nil || resp != "resp" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if agent.retry.StaleStreak() != 0 {
		t.Fatalf("streak %d", agent.retry.StaleStreak())
	}
}

func TestAgentNonStreamInterruptAbortsAndSwallowsTheWorkerError(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	client := &fakeChatClient{create: func(ctx context.Context, kw map[string]any) (any, error) {
		close(started)
		<-release
		return nil, &fakeConnErr{}
	}}
	var agent *fakeChatAgent
	agent = &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m",
		resolve: func(map[string]any) (any, error) { return 60.0, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil },
		onAbort: func(any, string) { close(release) }}
	go func() {
		<-started
		agent.setInterrupted(true)
	}()
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "m", "messages": []any{}})
	var ie *InterruptedError
	if !errors.As(err, &ie) || err.Error() != "Agent interrupted during API call" {
		t.Fatalf("err %v", err)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "interrupt_abort" {
		t.Fatalf("aborts %v", aborts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(agent.reasons(&agent.closes)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closes := agent.reasons(&agent.closes); len(closes) != 1 || closes[0] != "request_error_cleanup" {
		t.Fatalf("closes %v", closes)
	}
}

// Steps the poll clock 300ms per poll and releases the worker on the two hundredth poll
func moaHeartbeatAgent(t *testing.T) (*fakeChatAgent, any) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	response := &struct{ ok bool }{true}
	release := make(chan struct{})
	client := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		<-release
		return response, nil
	}}
	agent := &fakeChatAgent{provider: "moa", apiMode: "chat_completions", plat: "desktop", client: client,
		resolve: func(map[string]any) (any, error) { return math.Inf(1), nil }}
	base := time.Unix(1000, 0)
	var polls atomic.Int64
	prevClock, prevWait := agentNonStreamClock, agentNonStreamPollWait
	agentNonStreamClock = func() time.Time { return base.Add(time.Duration(polls.Load()) * nonStreamPollInterval) }
	agentNonStreamPollWait = func(time.Duration) <-chan time.Time {
		n := polls.Add(1)
		if n > 200 {
			return nil
		}
		if n == 200 {
			close(release)
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	t.Cleanup(func() { agentNonStreamClock, agentNonStreamPollWait = prevClock, prevWait })
	return agent, response
}

func TestAgentNonStreamMoaHeartbeatSurvivesAnInfiniteStaleTimeout(t *testing.T) {
	agent, response := moaHeartbeatAgent(t)
	result, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "openai-xai-wide"})
	if err != nil || result != response {
		t.Fatalf("result %v err %v", result, err)
	}
	notices := agent.snapshotStrings(&agent.notices)
	if len(notices) != 1 || !strings.Contains(notices[0], "waiting on openai-xai-wide") || strings.Contains(notices[0], "auto-reconnect") {
		t.Fatalf("notices %q", notices)
	}
}

type panickingNoticeAgent struct{ *fakeChatAgent }

func (panickingNoticeAgent) EmitWaitNotice(string) { panic("format failure") }

func TestAgentNonStreamWaitNoticeFailureDoesNotAbortTheRequest(t *testing.T) {
	agent, response := moaHeartbeatAgent(t)
	result, err := InterruptibleAPICall(t.Context(), panickingNoticeAgent{agent}, map[string]any{"model": "openai-xai-wide"})
	if err != nil || result != response {
		t.Fatalf("result %v err %v", result, err)
	}
}

func TestAgentNonstreamWatchdogPolicy(t *testing.T) {
	cases := []struct {
		provider, baseURL string
		chars             int
		idleEnv           *string
		idleOn, progress  bool
	}{
		{"openai-codex", "https://chatgpt.com/backend-api/codex", 40004, nil, true, true},
		{"openai-codex", "https://chatgpt.com/backend-api/codex", 40004, strPtr("2"), true, false},
		{"openai-codex", "https://chatgpt.com/backend-api/codex", 40000, nil, true, false},
		{"xai-oauth", "https://api.x.ai/v1", 40004, nil, true, false},
		{"openai-codex", "https://chatgpt.com/backend-api/codex", 40004, strPtr(""), true, true},
		{"openai-codex", "https://chatgpt.com/backend-api/codex", 40004, strPtr("invalid"), true, true},
		{"openai-codex", "https://chatgpt.com/backend-api/codex", 40004, strPtr("0"), false, false},
	}
	for i, c := range cases {
		agent := agentFormCodexAgent(t, c.provider, c.baseURL)
		if c.idleEnv != nil {
			t.Setenv(CodexEventStaleTimeoutEnv, *c.idleEnv)
		}
		w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, map[string]any{"model": "gpt-5.6-sol", "input": strings.Repeat("x", c.chars)})
		if err != nil || w.estTokens != c.chars/4 || w.idleEnabled != c.idleOn || w.idleRequiresProgress != c.progress || (w.progress > 0) != c.progress {
			t.Fatalf("case %d %+v %v", i, w, err)
		}
	}
	t.Run("local ttfb uses the local stale ceiling", func(t *testing.T) {
		local := agentFormCodexAgent(t, "custom", "http://127.0.0.1:11434/v1")
		hosted := agentFormCodexAgent(t, "custom", "https://api.example.com/v1")
		t.Setenv(LocalStreamStaleTimeoutEnv, "600")
		kw := map[string]any{"model": "qwen3-27b", "input": "hi"}
		if w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), local, kw); w.ttfb != 600 {
			t.Fatalf("local %v", w.ttfb)
		}
		if w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), hosted, kw); w.ttfb != 120 {
			t.Fatalf("hosted %v", w.ttfb)
		}
		t.Setenv(CodexTTFBTimeoutEnv, "45")
		if w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), local, kw); w.ttfb != 45 {
			t.Fatalf("explicit %v", w.ttfb)
		}
	})
	t.Run("large request keeps the scaled ttfb unless capped", func(t *testing.T) {
		agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
		agent.reasoning = map[string]any{"enabled": false}
		kw := map[string]any{"model": "gpt-5.5", "input": strings.Repeat("x", 440000)}
		w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, kw)
		if w.estTokens <= 100000 || !w.ttfbEnabled || w.ttfb != 180 {
			t.Fatalf("%+v", w)
		}
		t.Setenv(CodexTTFBMaxEnv, "90")
		if w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, kw); w.ttfb != 90 {
			t.Fatalf("cap %v", w.ttfb)
		}
	})
	t.Run("high effort floors ttfb and idle", func(t *testing.T) {
		agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
		agent.reasoning = map[string]any{"enabled": true, "effort": " XHigh "}
		w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
		if w.ttfb != HighEffortSilenceFloorSeconds || w.idle != HighEffortSilenceFloorSeconds {
			t.Fatalf("%+v", w)
		}
		agent.apiMode = "chat_completions"
		if w, _ := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"}); w.codex || w.ttfbEnabled || w.idleEnabled {
			t.Fatalf("non codex %+v", w)
		}
	})
}

type fakeStreamHandle struct{ closed atomic.Int32 }

func (h *fakeStreamHandle) Close() error { h.closed.Add(1); return nil }

type fakeStreamWithResponse struct{ resp *fakeStreamHandle }

func (s fakeStreamWithResponse) StreamResponse() any { return s.resp }

func TestRequestClientRegistryRoutesByKindAndOwner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	anthropicClient := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) { return "msg", nil }}
	agent := &fakeChatAgent{provider: "anthropic", apiMode: "anthropic_messages", baseURL: "https://api.anthropic.com", model: "claude",
		resolve:         func(map[string]any) (any, error) { return 60.0, nil },
		anthropicCreate: func(reason string) (any, error) { return anthropicClient, nil }}
	if resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "claude"}); err != nil || resp != "msg" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if closes := agent.reasons(&agent.aCloses); len(closes) != 1 || closes[0] != "request_complete" || len(agent.closes) != 0 {
		t.Fatalf("anthropic closes %v openai closes %v", closes, agent.closes)
	}
	reg := newRequestClientRegistry(agent)
	worker, stranger := &registryCaller{}, &registryCaller{}
	reg.setClient(anthropicClient, "anthropic_messages", worker)
	if err := reg.closeOnce("interrupt_abort", stranger); err != nil {
		t.Fatal(err)
	}
	if aborts := agent.reasons(&agent.aAborts); len(aborts) != 1 || aborts[0] != "interrupt_abort" || len(agent.aCloses) != 1 {
		t.Fatalf("stranger must only abort, aborts %v closes %v", aborts, agent.aCloses)
	}
	if err := reg.closeOnce("request_error_cleanup", worker); err != nil || len(agent.aCloses) != 2 {
		t.Fatalf("owner close %v %v", err, agent.aCloses)
	}
	if err := reg.closeOnce("request_error_cleanup", worker); err != nil || len(agent.aCloses) != 2 {
		t.Fatal("a popped client closes once")
	}
	plain := "not closable"
	if got := reg.setStreamHandle(plain, worker); got != plain || reg.client != nil {
		t.Fatal("an unclosable stream stays unregistered")
	}
	inner := &fakeStreamHandle{}
	reg.setStreamHandle(fakeStreamWithResponse{inner}, worker)
	if err := reg.closeOnce("interrupt_abort", stranger); err != nil || inner.closed.Load() != 1 || reg.client != nil {
		t.Fatalf("a stream handle closes from any goroutine, closed %d", inner.closed.Load())
	}
}

func (a *fakeChatAgent) SessionID() string { return a.model + "-session" }

func TestMergeNousPortalMessagesExtraBodyOnlyForNous(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	kwargs := map[string]any{"model": "claude-opus-4-8", "messages": []any{}}
	other := &fakeChatAgent{provider: "anthropic", model: "s"}
	if got := MergeNousPortalMessagesExtraBody(t.Context(), other, kwargs, nil); got["extra_body"] != nil || len(got) != 2 {
		t.Fatalf("non nous %v", got)
	}
	nous := &fakeChatAgent{provider: "nous", model: "sess-abc123"}
	kwargs = map[string]any{"extra_body": map[string]any{"keep": 1}}
	got := MergeNousPortalMessagesExtraBody(t.Context(), nous, kwargs, []map[string]any{{"role": "user", "content": "hi"}})
	extra := got["extra_body"].(map[string]any)
	if extra["keep"] != 1 || extra["session_id"] == nil || extra["provider_preferences"] != nil {
		t.Fatalf("extra %v", extra)
	}
	tags, _ := extra["tags"].([]any)
	if !slicesContainsAny(tags, "product=icebath-agent") || !slicesContainsAny(tags, "conversation=sess-abc123-session") {
		t.Fatalf("tags %v", tags)
	}
	bad := map[string]any{"extra_body": "x"}
	if got := MergeNousPortalMessagesExtraBody(t.Context(), nous, bad, nil); got["extra_body"] != "x" {
		t.Fatalf("a non dict extra_body stays %v", got)
	}
}

func slicesContainsAny(list []any, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestResolveNonstreamWatchdogsExportsTheResolvedFuses(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	got, err := ResolveNonstreamWatchdogs(context.Background(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := resolveAgentNonstreamWatchdogsOf(context.Background(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	want := NonStreamWatchdogs{IdleEnabled: inner.idleEnabled, TTFBEnabled: inner.ttfbEnabled, IdleTimeout: inner.idle, TTFBTimeout: inner.ttfb, StaleTimeout: inner.stale}
	if got != want || !got.TTFBEnabled || !got.IdleEnabled {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
