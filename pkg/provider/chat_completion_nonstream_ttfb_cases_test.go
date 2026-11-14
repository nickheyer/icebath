package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Hosted openai-codex fake agent in the tests/agent/test_codex_ttfb_watchdog.py fixture shape
func ttfbCodexAgent(t *testing.T) *fakeChatAgent {
	t.Helper()
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	agent.hint = func(model any) (string, error) { return codexSilentHangHint(true, fmt.Sprint(model)), nil }
	return agent
}

// Routes the fake agent's codex stream through the real RunCodexStream over a scripted responses client
func ttfbInstallEventStream(agent *fakeChatAgent, client *codexFakeClient) {
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		return RunCodexStream(ctx, agent, kw, client, nil)
	}
}

// Connection error of the shape the codex stream loop retries on a torn physical stream
func ttfbConnectionError(text string) error {
	return &AuxTransportError{Class: "ConnectError", Message: text}
}

// Stream tail that blocks until the watchdog retires the request token, then unwinds with err
func ttfbRetiredTail(agent *fakeChatAgent, err error) func() (map[string]any, error) {
	return func() (map[string]any, error) {
		deadline := time.Now().Add(30 * time.Second)
		for agent.ActiveCodexStreamRequestToken() != nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		return nil, err
	}
}

// Codex worker that never emits an event and unwinds once the watchdog cancels or retires it
func ttfbSilentHang(agent *fakeChatAgent) func(ctx context.Context, kw map[string]any) (any, error) {
	return func(ctx context.Context, kw map[string]any) (any, error) {
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) && ctx.Err() == nil && agent.ActiveCodexStreamRequestToken() != nil && !agent.InterruptRequested() {
			time.Sleep(20 * time.Millisecond)
		}
		return nil, errors.New("connection closed")
	}
}

func TestLocalEndpointTtfbDefaultUsesLocalStaleCeiling(t *testing.T) {
	local := agentFormCodexAgent(t, "custom", "http://127.0.0.1:11434/v1")
	hosted := agentFormCodexAgent(t, "custom", "https://api.example.com/v1")
	t.Setenv(LocalStreamStaleTimeoutEnv, "600")
	kw := map[string]any{"model": "qwen3-27b", "input": "hi"}
	if w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), local, kw); err != nil || w.ttfb != 600 {
		t.Fatalf("local %v %v", w.ttfb, err)
	}
	if w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), hosted, kw); err != nil || w.ttfb != 120 {
		t.Fatalf("hosted %v %v", w.ttfb, err)
	}
}

func TestLocalEndpointTtfbExplicitEnvStillWins(t *testing.T) {
	local := agentFormCodexAgent(t, "custom", "http://127.0.0.1:11434/v1")
	t.Setenv(LocalStreamStaleTimeoutEnv, "600")
	t.Setenv(CodexTTFBTimeoutEnv, "45")
	if w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), local, map[string]any{"model": "qwen3-27b", "input": "hi"}); err != nil || w.ttfb != 45 {
		t.Fatalf("explicit %v %v", w.ttfb, err)
	}
}

func TestTtfbIncludesSilentHangHintForGpt55(t *testing.T) {
	agent := ttfbCodexAgent(t)
	t.Setenv(CodexTTFBTimeoutEnv, "0.4")
	agent.codexStream = ttfbSilentHang(agent)
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	hint, _ := agent.CodexSilentHangHint("gpt-5.5")
	if hint == "" {
		t.Fatal("gpt-5.5 on the Codex backend must match the silent-hang heuristic")
	}
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), hint) {
		t.Fatalf("err %v", err)
	}
	if !anyContains(agent.reasons(&agent.aborts), "codex_ttfb_kill") {
		t.Fatalf("aborts %v", agent.reasons(&agent.aborts))
	}
	statuses := agent.snapshotStrings(&agent.statuses)
	if len(statuses) == 0 || !anyContains(statuses, hint) {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestTtfbInstallsAndRetiresTheCodexRequestToken(t *testing.T) {
	agent := ttfbCodexAgent(t)
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
		t.Fatal("interruptible_api_call must install a request token before the worker runs")
	}
	if err == nil || !strings.Contains(err.Error(), "TTFB") || strings.Contains(err.Error(), "retired worker") {
		t.Fatalf("err %v", err)
	}
	if !anyContains(agent.reasons(&agent.aborts), "codex_ttfb_kill") {
		t.Fatalf("aborts %v", agent.reasons(&agent.aborts))
	}
	if agent.ActiveCodexStreamRequestToken() != nil {
		t.Fatal("the kill must retire the token")
	}
}

func TestTtfbDoesNotKillWhenEventsFlow(t *testing.T) {
	agent := ttfbCodexAgent(t)
	t.Setenv(CodexTTFBTimeoutEnv, "0.4")
	sentinel := &struct{ ok bool }{true}
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		state := CodexWatchdogStateFrom(ctx)
		now := epochSeconds(time.Now())
		state.Mu.Lock()
		state.LastEventTs = &now
		state.Mu.Unlock()
		time.Sleep(900 * time.Millisecond)
		return sentinel, nil
	}
	resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	if err != nil || resp != sentinel {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if anyContains(agent.reasons(&agent.aborts), "codex_ttfb_kill") {
		t.Fatalf("aborts %v", agent.reasons(&agent.aborts))
	}
}

func TestIdlePhasePolicyIsNarrowAndPreservesOperatorOverrides(t *testing.T) {
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
}

func TestLifecycleEventDoesNotRestartFirstProgressDeadline(t *testing.T) {
	last := 280.0
	d := CodexWatchdogDeadline(CodexWatchdogClock{StaleTimeout: 900, TTFBEnabled: true, TTFBTimeout: 300, LastEventTS: &last, CallStart: 100,
		IdleEnabled: true, IdleTimeout: 120, IdleRequiresProgress: true, ProgressTimeout: 300, Elapsed: 250})
	if d == nil || d.Label != "first progress" || d.Remaining != 50 {
		t.Fatalf("deadline %+v", d)
	}
}

func TestLargeCodexLifecycleOnlyStreamHitsAttemptProgressBudget(t *testing.T) {
	agent := ttfbCodexAgent(t)
	shortenAgentWatchdogs(t, func(w *agentNonstreamWatchdogs) { w.idle, w.ttfb, w.progress = 2, 0.9, 0.9 })
	stream := newCodexFakeStream(codexStreamEv("response.created"))
	stream.before = func(int) { time.Sleep(700 * time.Millisecond) }
	stream.tail = ttfbRetiredTail(agent, ttfbConnectionError("retired lifecycle-only stream"))
	ttfbInstallEventStream(agent, codexStreamClient(func() any { return stream }))
	started := time.Now()
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.6-sol", "input": strings.Repeat("x", 40004)})
	if err == nil || !strings.Contains(err.Error(), "no substantive model progress") {
		t.Fatalf("err %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
		t.Fatalf("kill took %v", elapsed)
	}
	if !anyContains(agent.reasons(&agent.aborts), "codex_progress_kill") {
		t.Fatalf("aborts %v", agent.reasons(&agent.aborts))
	}
}

func TestEventStalePhaseIsScopedToPhysicalStreamAttempt(t *testing.T) {
	completed := func(id string) map[string]any {
		return codexStreamEv("response.completed", "response", map[string]any{"status": "completed", "id": id, "usage": nil})
	}
	gapStream := func(id, reasoning string) *codexFakeStream {
		s := newCodexFakeStream(codexStreamEv("response.created"), codexStreamEv("response.in_progress"),
			codexStreamEv("response.reasoning_text.delta", "delta", reasoning), codexStreamEv("response.output_text.delta", "delta", "done"), completed(id))
		s.before = func(i int) {
			if i == 2 {
				time.Sleep(3 * time.Second)
			}
		}
		return s
	}
	for _, mode := range []string{"initial_gap", "stall", "retry_gap", "retry_no_event"} {
		t.Run(mode, func(t *testing.T) {
			agent := ttfbCodexAgent(t)
			shortenAgentWatchdogs(t, func(w *agentNonstreamWatchdogs) { w.idle = 2 })
			t.Setenv(CodexTTFBTimeoutEnv, "2")
			t.Setenv(CodexTTFBStrictEnv, "1")
			t.Setenv(CodexHardTimeoutEnv, "5")
			var first, second func() any
			switch mode {
			case "initial_gap":
				first = func() any { return gapStream("resp-1", "working") }
			case "stall":
				first = func() any {
					s := newCodexFakeStream(codexStreamEv("response.created"), codexStreamEv("response.reasoning_text.delta", "delta", "first step"))
					s.tail = ttfbRetiredTail(agent, ttfbConnectionError("retired stalled stream"))
					return s
				}
			case "retry_gap":
				first = func() any {
					s := newCodexFakeStream(codexStreamEv("response.created"), codexStreamEv("response.reasoning_text.delta", "delta", "first step"))
					s.tail = func() (map[string]any, error) { return nil, ttfbConnectionError("retry physical stream") }
					return s
				}
				second = func() any { return gapStream("resp-2", "retry step") }
			case "retry_no_event":
				first = func() any {
					s := newCodexFakeStream(codexStreamEv("response.created"))
					s.tail = func() (map[string]any, error) { return nil, ttfbConnectionError("retry physical stream") }
					return s
				}
				second = func() any {
					s := newCodexFakeStream()
					s.tail = ttfbRetiredTail(agent, errors.New("retired zero-event retry"))
					return s
				}
			}
			streams := []func() any{first}
			if second != nil {
				streams = append(streams, second)
			}
			client := codexStreamClient(streams...)
			ttfbInstallEventStream(agent, client)
			resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.6-sol", "input": strings.Repeat("x", 40004)})
			wantAttempts := 1
			switch mode {
			case "initial_gap", "retry_gap":
				final, _ := resp.(map[string]any)
				if err != nil || final["output_text"] != "done" {
					t.Fatalf("resp %v err %v", resp, err)
				}
				if mode == "retry_gap" {
					wantAttempts = 2
				}
			default:
				want := "no SSE events"
				if mode == "retry_no_event" {
					want, wantAttempts = "no parsed stream event", 2
				}
				var te *TimeoutPhaseError
				if !errors.As(err, &te) || !strings.Contains(err.Error(), want) {
					t.Fatalf("err %v", err)
				}
			}
			aborts := agent.reasons(&agent.aborts)
			if anyContains(aborts, "codex_stream_idle_kill") != (mode == "stall") || anyContains(aborts, "codex_ttfb_kill") != (mode == "retry_no_event") {
				t.Fatalf("aborts %v", aborts)
			}
			if client.callCount() != wantAttempts {
				t.Fatalf("attempts %d want %d", client.callCount(), wantAttempts)
			}
		})
	}
}

func TestWaitNoticeOmitsReconnectWhenAllDeadlinesAreNonFinite(t *testing.T) {
	for _, stale := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		d := CodexWatchdogDeadline(CodexWatchdogClock{StaleTimeout: stale, TTFBEnabled: false, TTFBTimeout: math.NaN(), CallStart: 100,
			IdleEnabled: false, IdleTimeout: math.NaN(), IdleRequiresProgress: false, Elapsed: 30})
		if d != nil {
			t.Fatalf("stale %v gave %+v", stale, d)
		}
	}
}

func TestMoaHeartbeatSurvivesInfiniteStaleTimeout(t *testing.T) {
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

func TestWaitNoticeFormattingErrorDoesNotAbortRequest(t *testing.T) {
	agent, response := moaHeartbeatAgent(t)
	prev := codexWatchdogDeadlineFn
	codexWatchdogDeadlineFn = func(CodexWatchdogClock) *WatchdogDeadline { panic(&ValueError{Message: "bad display state"}) }
	t.Cleanup(func() { codexWatchdogDeadlineFn = prev })
	result, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "openai-xai-wide"})
	if err != nil || result != response {
		t.Fatalf("result %v err %v", result, err)
	}
}

func TestLargeCodexRequestHardCeilingReclaimsSilentStall(t *testing.T) {
	agent := ttfbCodexAgent(t)
	t.Setenv(CodexHardTimeoutEnv, "3")
	agent.codexStream = ttfbSilentHang(agent)
	started := time.Now()
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": strings.Repeat("x", 44000)})
	if elapsed := time.Since(started); elapsed >= 30*time.Second {
		t.Fatalf("hard ceiling took %v", elapsed)
	}
	if !anyContains(agent.reasons(&agent.aborts), "stale_call_kill") {
		t.Fatalf("stale kill expected, got %v", agent.reasons(&agent.aborts))
	}
	if err == nil || !strings.Contains(err.Error(), "timed out after") || !strings.Contains(err.Error(), "with no response") {
		t.Fatalf("err %v", err)
	}
}

func TestLargeRequestKeepsScaledTtfbInsteadOfRecapping(t *testing.T) {
	agent := ttfbCodexAgent(t)
	agent.reasoning = map[string]any{"enabled": false}
	w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": strings.Repeat("x", 440000)})
	if err != nil || w.estTokens <= 100000 || !w.ttfbEnabled || w.ttfb != 180 {
		t.Fatalf("%+v %v", w, err)
	}
}

func TestExplicitTtfbMaxSecondsStillCaps(t *testing.T) {
	agent := ttfbCodexAgent(t)
	agent.reasoning = map[string]any{"enabled": false}
	t.Setenv(CodexTTFBMaxEnv, "90")
	w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), agent, map[string]any{"model": "gpt-5.5", "input": strings.Repeat("x", 440000)})
	if err != nil || w.ttfb != 90 {
		t.Fatalf("%+v %v", w, err)
	}
}

func TestAgentNonstreamWatchdogPolicyHighEffortFloorsTtfbAndIdle(t *testing.T) {
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
}
