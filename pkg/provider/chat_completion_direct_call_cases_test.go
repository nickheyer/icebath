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

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
)

// Client answering a fixed response map while recording the kwargs it was sent
func recordingChatClient(resp map[string]any) (*fakeChatClient, *atomic.Value) {
	var sent atomic.Value
	c := &fakeChatClient{create: func(_ context.Context, kw map[string]any) (any, error) {
		sent.Store(kw)
		return resp, nil
	}}
	return c, &sent
}

// Kwargs the recording client saw, nil before any request
func sentKwargs(sent *atomic.Value) map[string]any {
	kw, _ := sent.Load().(map[string]any)
	return kw
}

// Client whose request blocks on release after signalling started, then answers id
func blockingChatClient(id string) (*fakeChatClient, chan struct{}, chan struct{}) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			return nil, errors.New("release never arrived")
		}
		return map[string]any{"id": id}, nil
	}}
	return c, started, release
}

func TestShouldUseDirectApiCallOnlyForCronOpenaiWire(t *testing.T) {
	ctx := t.Context()
	if !ShouldUseDirectAPICall(ctx, "openrouter", "cron", ModeChatCompletions) {
		t.Fatal("cron must run inline")
	}
	for _, platform := range []string{"cli", "telegram", ""} {
		if ShouldUseDirectAPICall(ctx, "openrouter", platform, ModeChatCompletions) {
			t.Fatalf("platform %q must not run inline", platform)
		}
	}
	if !ShouldUseDirectAPICall(ctx, "openrouter", "cron", ModeCodexResponses) {
		t.Fatal("cron codex must run inline")
	}
	for _, mode := range []APIMode{ModeAnthropicMessages, modeBedrockConverse} {
		if ShouldUseDirectAPICall(ctx, "openrouter", "cron", mode) {
			t.Fatalf("mode %s must not run inline", mode)
		}
	}
	if ShouldUseDirectAPICall(ctx, "moa", "cron", ModeChatCompletions) {
		t.Fatal("moa must not run inline")
	}
}

func TestStalledInlineCallIsAbortedAndRaisesRetryableTimeout(t *testing.T) {
	a := cronChatAgent(0.2)
	stallingChatClient(a)
	_, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "no response") {
		t.Fatalf("err %v", err)
	}
	if aborts := a.reasons(&a.aborts); len(aborts) != 1 || aborts[0] != "stale_call_kill" {
		t.Fatalf("aborts %v", aborts)
	}
}

func TestInlineCronOpenaiCodexKeepsLargeContextStaleFloor(t *testing.T) {
	unsetEnvForTest(t, APICallStaleTimeoutEnv, StreamStaleTimeoutEnv, CodexHardTimeoutEnv)
	hosted := &fakeChatAgent{provider: "openai-codex", apiMode: "codex_responses", baseURL: "https://chatgpt.com/backend-api/codex", plat: "cron", model: "gpt-5.5",
		resolve: func(map[string]any) (any, error) { return 90.0, nil }}
	kw := map[string]any{"model": "gpt-5.5", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("x ", 60000)}}}
	if !ShouldUseDirectAPICall(t.Context(), hosted.provider, hosted.plat, APIMode(hosted.apiMode)) {
		t.Fatal("cron codex must run inline")
	}
	if got, err := ResolveDirectStaleTimeout(t.Context(), hosted, kw); err != nil || got < 600 {
		t.Fatalf("hosted %v %v", got, err)
	}
}

func TestInlineLocalResponsesEndpointKeepsItsConfiguredStaleBudget(t *testing.T) {
	unsetEnvForTest(t, CodexHardTimeoutEnv)
	t.Setenv(APICallStaleTimeoutEnv, "3000")
	local := &fakeChatAgent{provider: "custom", apiMode: "codex_responses", baseURL: "http://127.0.0.1:8080/v1", plat: "cron", model: "local-model",
		resolve: func(map[string]any) (any, error) { return 3000.0, nil }}
	kw := map[string]any{"model": "local-model", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("x ", 60000)}}}
	if got, err := ResolveDirectStaleTimeout(t.Context(), local, kw); err != nil || got != 3000 {
		t.Fatalf("local %v %v", got, err)
	}
}

func TestDirectStaleBudgetNonNumericDisarms(t *testing.T) {
	a := &fakeChatAgent{apiMode: "chat_completions", resolve: func(map[string]any) (any, error) { return true, nil }}
	if got, _ := ResolveDirectStaleTimeout(t.Context(), a, map[string]any{"model": "m"}); !math.IsInf(got, 1) {
		t.Fatalf("non numeric budget must disarm, got %v", got)
	}
}

func TestWatchdogKillFeedsTheCrossTurnStaleCircuitBreaker(t *testing.T) {
	a := cronChatAgent(0.2)
	stallingChatClient(a)
	_, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || a.retry.StaleStreak() != 1 {
		t.Fatalf("err %v streak %d", err, a.retry.StaleStreak())
	}
}

func TestHealthyCallIsUntouchedByTheWatchdog(t *testing.T) {
	a := cronChatAgent(30.0)
	fast, _ := recordingChatClient(map[string]any{"id": "fast"})
	a.create = func(string, map[string]any) (any, error) { return fast, nil }
	resp, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	if err != nil || resp.(map[string]any)["id"] != "fast" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if len(a.aborts) != 0 || len(a.closes) != 1 || a.closes[0].reason != "request_complete" {
		t.Fatalf("aborts %+v closes %+v", a.aborts, a.closes)
	}
}

func TestLocalEndpointInfiniteBudgetLeavesTheWatchdogDisarmed(t *testing.T) {
	a := cronChatAgent(math.Inf(1))
	slow, started, release := blockingChatClient("slow-but-healthy")
	a.create = func(string, map[string]any) (any, error) { return slow, nil }
	done := make(chan any, 1)
	go func() {
		resp, err := DirectAPICall(context.Background(), a, map[string]any{"model": "m", "messages": []any{}})
		if err != nil {
			done <- err
			return
		}
		done <- resp
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	select {
	case out := <-done:
		resp, _ := out.(map[string]any)
		if resp["id"] != "slow-but-healthy" {
			t.Fatalf("result %v", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call never returned")
	}
	if len(a.aborts) != 0 {
		t.Fatalf("aborts %+v", a.aborts)
	}
}

func TestInterruptAbortIsNotMisclassifiedAsProviderStaleness(t *testing.T) {
	a := cronChatAgent(30.0)
	release := make(chan struct{})
	var once sync.Once
	c := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("abort never released the request")
		}
		return nil, errors.New("ConnectionError: socket shut down")
	}}
	a.create = func(string, map[string]any) (any, error) { return c, nil }
	a.onAbort = func(any, string) { once.Do(func() { close(release) }) }
	fired := make(chan struct{})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hook := a.ActiveRequestAbort(); hook != nil {
				a.setInterrupted(true)
				if hook.Abort("interrupt_abort") {
					t.Error("interrupt abort must not own the stale transition")
				}
				if hook.Abort("stale_call_kill") {
					t.Error("a stale timer racing in after the interrupt must be inert")
				}
				close(fired)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	_, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	var ie *InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("err %v", err)
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("interrupt never fired")
	}
	if a.retry.StaleStreak() != 0 {
		t.Fatal("interrupt was misclassified as provider staleness")
	}
}

func TestLateStaleTimerAfterCompletionIsInert(t *testing.T) {
	a := cronChatAgent(30.0)
	ok, _ := recordingChatClient(map[string]any{"id": "ok"})
	a.create = func(string, map[string]any) (any, error) { return ok, nil }
	var captured atomic.Value
	prev := directWatchdogArmFn
	directWatchdogArmFn = func(d time.Duration, f func()) *time.Timer {
		captured.Store(f)
		return time.AfterFunc(time.Hour, f)
	}
	t.Cleanup(func() { directWatchdogArmFn = prev })
	resp, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	if err != nil || resp.(map[string]any)["id"] != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	fn, _ := captured.Load().(func())
	if fn == nil {
		t.Fatal("no watchdog timer armed")
	}
	fn()
	if a.retry.StaleStreak() != 0 || len(a.aborts) != 0 {
		t.Fatalf("streak %d aborts %+v", a.retry.StaleStreak(), a.aborts)
	}
}

func TestInlineCallPassesHardReadTimeoutToTheSdk(t *testing.T) {
	a := cronChatAgent(0.5)
	c, sent := recordingChatClient(map[string]any{"id": "ok"})
	a.create = func(string, map[string]any) (any, error) { return c, nil }
	resp, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	if err != nil || resp.(map[string]any)["id"] != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if timeout, ok := sentKwargs(sent)["timeout"].(pyhttpx.Timeout); !ok || timeout.Read != 500*time.Millisecond {
		t.Fatalf("timeout %v", sentKwargs(sent)["timeout"])
	}
}

func TestInlineCallDoesNotOverrideExplicitTimeout(t *testing.T) {
	a := cronChatAgent(30.0)
	c, sent := recordingChatClient(map[string]any{"id": "ok"})
	a.create = func(string, map[string]any) (any, error) { return c, nil }
	resp, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}, "timeout": 12.0})
	if err != nil || resp.(map[string]any)["id"] != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if kw := sentKwargs(sent); kw["timeout"] != 12.0 {
		t.Fatalf("timeout %v", kw["timeout"])
	}
}

func TestInfiniteBudgetDoesNotInjectAHardTimeout(t *testing.T) {
	a := cronChatAgent(math.Inf(1))
	c, sent := recordingChatClient(map[string]any{"id": "ok"})
	a.create = func(string, map[string]any) (any, error) { return c, nil }
	resp, err := DirectAPICall(t.Context(), a, map[string]any{"model": "m", "messages": []any{}})
	if err != nil || resp.(map[string]any)["id"] != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if kw := sentKwargs(sent); kw["timeout"] != nil {
		t.Fatalf("timeout %v", kw["timeout"])
	}
}

func TestDirectApiCallKeepsActivityAliveDuringSlowWait(t *testing.T) {
	prev := directAPIActivityHeartbeat
	directAPIActivityHeartbeat = 50 * time.Millisecond
	t.Cleanup(func() { directAPIActivityHeartbeat = prev })
	a := cronChatAgent(nil)
	a.plat = "subagent"
	slow, started, release := blockingChatClient("slow")
	a.create = func(string, map[string]any) (any, error) { return slow, nil }
	done := make(chan any, 1)
	go func() {
		resp, err := DirectAPICall(context.Background(), a, map[string]any{"model": "m", "messages": []any{}})
		if err != nil {
			done <- err
			return
		}
		done <- resp
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && a.touchCount() < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	touchesWhileBlocked := a.touchCount()
	close(release)
	select {
	case out := <-done:
		resp, _ := out.(map[string]any)
		if resp["id"] != "slow" {
			t.Fatalf("result %v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call never returned")
	}
	if touchesWhileBlocked < 3 {
		t.Fatalf("expected mid-wait activity heartbeats, got %d", touchesWhileBlocked)
	}
}

func TestShouldUseDirectApiCallGateIsUnchanged(t *testing.T) {
	ctx := t.Context()
	if !ShouldUseDirectAPICall(ctx, "openrouter", "cron", ModeChatCompletions) || !ShouldUseDirectAPICall(ctx, "openrouter", "subagent", ModeChatCompletions) {
		t.Fatal("cron and subagent route inline")
	}
	if ShouldUseDirectAPICall(ctx, "openrouter", "cli", ModeChatCompletions) || ShouldUseDirectAPICall(ctx, "openrouter", "cron", ModeAnthropicMessages) ||
		ShouldUseDirectAPICall(ctx, "moa", "cron", ModeChatCompletions) {
		t.Fatal("cli, anthropic_messages and moa stay on the worker")
	}
}
