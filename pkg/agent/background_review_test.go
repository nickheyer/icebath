package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func costControlAgent(providerName, model string) *AIAgent {
	return &AIAgent{provider: providerName, model: model, apiKey: "parent-key",
		baseURL: "https://chatgpt.com/backend-api/codex", apiMode: "codex_app_server", requestOverrides: map[string]any{}}
}

func reviewCfg(provider, model string) map[string]any {
	return map[string]any{"provider": provider, "model": model}
}

// Replaces the runtime resolver for the test
func replaceReviewResolver(t *testing.T, fn func(ctx context.Context, h, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*provider.Runtime, error)) {
	prev := reviewResolveRuntimeProvider
	t.Cleanup(func() { reviewResolveRuntimeProvider = prev })
	reviewResolveRuntimeProvider = fn
}

func TestRoutingAutoInheritsParentAndDowngradesCodexAppServer(t *testing.T) {
	rt := resolveReviewRuntime(context.Background(), costControlAgent("openai-codex", "gpt-5.5"), reviewCfg("auto", ""))
	if rt.Routed || rt.Provider != "openai-codex" || rt.Model != "gpt-5.5" || rt.APIMode != "codex_responses" {
		t.Fatalf("runtime %+v", rt)
	}
}

func TestRoutingToDifferentModelMarksRoutedAndResolvesCredentials(t *testing.T) {
	pool := &provider.Pool{}
	replaceReviewResolver(t, func(_ context.Context, _, requested, targetModel, _, _ string) (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "openrouter", APIKey: "or-key", BaseURL: "https://openrouter.ai/api/v1",
			APIMode: "chat_completions", CredentialPool: pool,
			RequestOverrides: map[string]any{"extra_body": map[string]any{"store": false}}}, nil
	})
	rt := resolveReviewRuntime(context.Background(), costControlAgent("openai-codex", "gpt-5.5"), reviewCfg("openrouter", "google/gemini-3-flash-preview"))
	if !rt.Routed || rt.Provider != "openrouter" || rt.Model != "google/gemini-3-flash-preview" || rt.APIKey != "or-key" ||
		rt.CredentialPool != pool || rt.MaxTokens != 0 ||
		!reflect.DeepEqual(rt.RequestOverrides, map[string]any{"extra_body": map[string]any{"store": false}}) {
		t.Fatalf("runtime %+v", rt)
	}
}

func TestUnroutedRuntimeKeepsParentPoolAndOverrides(t *testing.T) {
	a := costControlAgent("openai-codex", "gpt-5.5")
	a.credentialPool = &provider.Pool{}
	a.requestOverrides = map[string]any{"service_tier": "priority"}
	a.maxTokens = 4096
	rt := resolveReviewRuntime(context.Background(), a, map[string]any{})
	if rt.CredentialPool != a.credentialPool || !reflect.DeepEqual(rt.RequestOverrides, map[string]any{"service_tier": "priority"}) || rt.MaxTokens != 4096 {
		t.Fatalf("runtime %+v", rt)
	}
}

func TestRoutingSameModelAsParentIsNotRouted(t *testing.T) {
	a := costControlAgent("openrouter", "anthropic/claude-opus-4.8")
	if rt := resolveReviewRuntime(context.Background(), a, reviewCfg("openrouter", "anthropic/claude-opus-4.8")); rt.Routed {
		t.Fatalf("runtime %+v", rt)
	}
}

func TestRoutingResolutionFailureFallsBackToParent(t *testing.T) {
	replaceReviewResolver(t, func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		return nil, errors.New("boom")
	})
	rt := resolveReviewRuntime(context.Background(), costControlAgent("openai-codex", "gpt-5.5"), reviewCfg("openrouter", "google/gemini-3-flash-preview"))
	if rt.Routed || rt.Provider != "openai-codex" {
		t.Fatalf("runtime %+v", rt)
	}
}

func digestMsg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func TestDigestUnderTailReturnsFull(t *testing.T) {
	msgs := []map[string]any{digestMsg("user", "hi"), digestMsg("assistant", "hello")}
	if out := digestHistory(msgs, 24); !reflect.DeepEqual(out, msgs) {
		t.Fatalf("digest %v", out)
	}
}

func TestDigestCollapsesOldKeepsTailVerbatim(t *testing.T) {
	var msgs []map[string]any
	for i := range 60 {
		msgs = append(msgs, digestMsg("user", fmt.Sprintf("u%d ", i)+strings.Repeat("x", 50)),
			digestMsg("assistant", fmt.Sprintf("a%d ", i)+strings.Repeat("y", 50)))
	}
	out := digestHistory(msgs, 10)
	if out[0]["role"] != "user" || !reflect.DeepEqual(out[len(out)-1], msgs[len(msgs)-1]) || len(out) != 11 {
		t.Fatalf("digest head %v len %d", out[0]["role"], len(out))
	}
}

func TestDigestDoesNotOpenTailOnAToolMessage(t *testing.T) {
	var msgs []map[string]any
	for range 40 {
		call := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "terminal", "arguments": "{}"}}}}
		msgs = append(msgs, digestMsg("user", "u"+strings.Repeat("x", 50)), call, digestMsg("tool", "result "+strings.Repeat("w", 50)))
	}
	if out := digestHistory(msgs, 2); out[1]["role"] == "tool" {
		t.Fatal("tail opened on a tool message")
	}
}

func TestEnabledFalseDisablesAutomaticReview(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("auxiliary:\n  background_review:\n    enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := loadBackgroundReviewSettings(context.Background()); enabled {
		t.Fatal("disabled config left automatic review enabled")
	}
}

func TestUnresolvableReviewProviderFallsBackWithVisibleWarning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := costControlAgent("openai-codex", "gpt-5.5")
	var emitted []string
	a.statusCallback = func(kind string, msg any) { emitted = append(emitted, fmt.Sprint(msg)) }
	cfg := reviewCfg("no-such-provider", "review-model")
	rt := resolveReviewRuntime(context.Background(), a, cfg)
	resolveReviewRuntime(context.Background(), a, cfg)
	if rt.Routed || rt.Model != "gpt-5.5" {
		t.Fatalf("runtime %+v", rt)
	}
	if len(emitted) != 1 || !strings.Contains(emitted[0], "no-such-provider") {
		t.Fatalf("emitted %v", emitted)
	}
}

func TestReviewRunFinishLeavesASuccessorRunInstalled(t *testing.T) {
	a := &AIAgent{}
	first := prepareBackgroundReviewRun(a)
	finishBackgroundReviewRun(a, first)
	second := prepareBackgroundReviewRun(a)
	if second == nil || second == first {
		t.Fatal("successor run not installed")
	}
	finishBackgroundReviewRun(a, first)
	if currentBackgroundReviewRun(a) != second || second.requestDone.IsSet() {
		t.Fatal("stale cleanup cleared or signalled the newer review")
	}
	if prepareBackgroundReviewRun(a) != nil {
		t.Fatal("a second review started while one held its request phase")
	}
	finishBackgroundReviewRun(a, second)
	if currentBackgroundReviewRun(a) != nil || !second.requestDone.IsSet() {
		t.Fatal("finished run left installed")
	}
}

func TestCancelFencesTheRunAndProceedsPastTheDeadline(t *testing.T) {
	a := &AIAgent{}
	run := prepareBackgroundReviewRun(a)
	review := &scopeStopAgent{}
	if !run.beginRequest(review) {
		t.Fatal("request not admitted")
	}
	cancelBackgroundReviewForLiveTurn(a)
	if !run.cancelRequestedSet() || run.beginRequest(review) {
		t.Fatal("cancel did not fence the run")
	}
	if run.cancel() != nil {
		t.Fatal("cancel dispatched twice")
	}
}
