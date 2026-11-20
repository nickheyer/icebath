package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var watchdogSmallPrompt = map[string]any{"model": "gpt-5.5", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 6000)}}}

// Typed and agent form watchdogs of a gpt-5.5 openai-codex request at the given reasoning effort
func effortWatchdogs(t *testing.T, ctx context.Context, agent *fakeChatAgent, effort string, enabled bool) (codexWatchdogs, agentNonstreamWatchdogs) {
	t.Helper()
	c, err := newCodexResponsesClient(Route{Provider: "openai-codex", Model: "gpt-5.5", BaseURL: "https://chatgpt.com/backend-api/codex", Mode: ModeCodexResponses})
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Model: "gpt-5.5", Reasoning: Reasoning{Enabled: &enabled, Effort: effort}}
	settings := loadWireSettings(ctx)
	est := EstimateRequestContextTokens(ctx, watchdogSmallPrompt)
	typed := c.(*codexResponsesClient).resolveWatchdogs(ctx, req, settings, est, watchdogSmallPrompt)
	agent.reasoning = map[string]any{"enabled": enabled, "effort": effort}
	agent.resolve = func(kw map[string]any) (any, error) {
		return computeNonStreamStaleTimeout(ctx, settings, "openai-codex", req, false, true, est, kw), nil
	}
	form, err := resolveAgentNonstreamWatchdogsOf(ctx, agent, watchdogSmallPrompt)
	if err != nil {
		t.Fatal(err)
	}
	return typed, form
}

// Codex agent of the effort cases with every stale and watchdog env cleared
func effortCodexAgent(t *testing.T) *fakeChatAgent {
	t.Helper()
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	unsetEnvForTest(t, APICallStaleTimeoutEnv, CodexHardTimeoutEnv)
	return agent
}

func fuses(w codexWatchdogs) [3]float64 { return [3]float64{w.idle, w.ttfb, w.stale} }

func TestHighEffortSmallPromptGetsTheSilenceFloorOnAllThreeFuses(t *testing.T) {
	for _, effort := range []string{"high", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			typed, form := effortWatchdogs(t, t.Context(), effortCodexAgent(t), effort, true)
			for _, w := range []codexWatchdogs{typed, form.codexWatchdogs} {
				if !w.idleEnabled || !w.ttfbEnabled || w.idle < HighEffortSilenceFloorSeconds || w.ttfb < HighEffortSilenceFloorSeconds || w.stale < HighEffortSilenceFloorSeconds {
					t.Fatalf("%+v", w)
				}
			}
		})
	}
}

func TestDefaultEffortTiersAndExplicitOperatorValuesAreUntouched(t *testing.T) {
	mediumTyped, mediumForm := effortWatchdogs(t, t.Context(), effortCodexAgent(t), "medium", true)
	for _, f := range fuses(mediumTyped) {
		if f >= HighEffortSilenceFloorSeconds {
			t.Fatalf("medium %+v", mediumTyped)
		}
	}
	if fuses(mediumForm.codexWatchdogs) != fuses(mediumTyped) {
		t.Fatalf("medium agent form %+v typed %+v", mediumForm, mediumTyped)
	}
	disabledTyped, disabledForm := effortWatchdogs(t, t.Context(), effortCodexAgent(t), "high", false)
	if fuses(disabledTyped) != fuses(mediumTyped) || fuses(disabledForm.codexWatchdogs) != fuses(mediumTyped) {
		t.Fatalf("disabled %+v %+v", disabledTyped, disabledForm)
	}
	agent := effortCodexAgent(t)
	t.Setenv(CodexEventStaleTimeoutEnv, "20")
	t.Setenv(CodexTTFBTimeoutEnv, "45")
	t.Setenv(APICallStaleTimeoutEnv, "75")
	explicitTyped, explicitForm := effortWatchdogs(t, t.Context(), agent, "high", true)
	want := [3]float64{20, 45, 75}
	if fuses(explicitTyped) != want || fuses(explicitForm.codexWatchdogs) != want {
		t.Fatalf("explicit %+v %+v", explicitTyped, explicitForm)
	}
}

func TestEffortFloorNeverOutlivesTheRunBudgetCap(t *testing.T) {
	agent := effortCodexAgent(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(95*time.Second))
	defer cancel()
	typed, form := effortWatchdogs(t, ctx, agent, "high", true)
	if typed.stale != 60 || form.stale != 60 {
		t.Fatalf("typed %v agent form %v", typed.stale, form.stale)
	}
}

// Agent of the xAI Responses accommodation cases with a 0.2s baseline stale budget
func xaiResponsesAgent(t *testing.T, provider, baseURL string) *fakeChatAgent {
	t.Helper()
	agent := agentFormCodexAgent(t, provider, baseURL)
	unsetEnvForTest(t, CodexHardTimeoutEnv)
	agent.resolve = func(map[string]any) (any, error) { return 0.2, nil }
	return agent
}

func TestXaiResponsesReceivesStaleTimeoutFloorInAPICall(t *testing.T) {
	agent := xaiResponsesAgent(t, "xai-oauth", "https://api.x.ai/v1")
	if ResponsesRouteIsCodexBackend(agent.Provider(), agent.BaseURL()) {
		t.Fatal("xai-oauth classified as the codex backend")
	}
	t.Setenv(CodexTTFBTimeoutEnv, "0")
	sentinel := &struct{ ok bool }{true}
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
		}
		if anyContains(agent.reasons(&agent.aborts), "stale_call_kill") {
			return nil, errors.New("aborted by stale kill")
		}
		return sentinel, nil
	}
	kw := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("word ", 65000)}}, "stream": false}
	res, err := InterruptibleAPICall(t.Context(), agent, kw)
	if err != nil || res != sentinel {
		t.Fatalf("res %v err %v", res, err)
	}
	if anyContains(agent.reasons(&agent.aborts), "stale_call_kill") {
		t.Fatalf("aborts %v", agent.reasons(&agent.aborts))
	}
}

func TestXaiResponsesTTFBScaledForLargeRequests(t *testing.T) {
	agent := xaiResponsesAgent(t, "xai-oauth", "https://api.x.ai/v1")
	if ResponsesRouteIsCodexBackend(agent.Provider(), agent.BaseURL()) {
		t.Fatal("xai-oauth classified as the codex backend")
	}
	agent.resolve = func(map[string]any) (any, error) { return 60.0, nil }
	t.Setenv(CodexTTFBTimeoutEnv, "0.2")
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
		return nil, errors.New("hang")
	}
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": true})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || !anyContains(agent.reasons(&agent.aborts), "codex_ttfb_kill") {
		t.Fatalf("small err %v aborts %v", err, agent.reasons(&agent.aborts))
	}
	agent.mu.Lock()
	agent.aborts = nil
	agent.mu.Unlock()
	agent.codexStream = func(ctx context.Context, kw map[string]any) (any, error) {
		time.Sleep(500 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	}
	res, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("word ", 65000)}}, "stream": true})
	if m, _ := res.(map[string]any); err != nil || m["ok"] != true || anyContains(agent.reasons(&agent.aborts), "codex_ttfb_kill") {
		t.Fatalf("large res %v err %v aborts %v", res, err, agent.reasons(&agent.aborts))
	}
}

func TestHardCeilingClampsHostedButNotLocalResponsesEndpoints(t *testing.T) {
	kw := map[string]any{"input": []any{map[string]any{"role": "user", "content": "hi"}}}
	hosted := xaiResponsesAgent(t, "xai-oauth", "https://api.x.ai/v1")
	local := xaiResponsesAgent(t, "custom", "http://127.0.0.1:1234/v1")
	for _, a := range []*fakeChatAgent{hosted, local} {
		a.resolve = func(map[string]any) (any, error) { return 3000.0, nil }
	}
	if w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), hosted, kw); err != nil || w.stale != 1500 {
		t.Fatalf("hosted %v %v", w.stale, err)
	}
	if w, err := resolveAgentNonstreamWatchdogsOf(t.Context(), local, kw); err != nil || w.stale != 3000 {
		t.Fatalf("local %v %v", w.stale, err)
	}
}
