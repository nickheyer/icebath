package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent with no prompt overrides, the hermes _bare_agent whose prompt attributes are deleted
func brBareAgent(t *testing.T) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &AIAgent{quietMode: true, requestOverrides: map[string]any{}}
}

// Prompt the review target would run for a memory and skills scope with an optional focus
func brSpawnPrompt(t *testing.T, a *AIAgent, reviewMemory, reviewSkills bool, focus *string) string {
	t.Helper()
	_, prompt, err := spawnBackgroundReviewThread(context.Background(), a, backgroundReviewKwargs{
		messagesSnapshot: []map[string]any{}, reviewMemory: reviewMemory, reviewSkills: reviewSkills, focus: focus}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func TestNoFocusPromptIsByteIdentical(t *testing.T) {
	agent := brBareAgent(t)
	blank := "   "
	for _, focus := range []*string{nil, nil, &blank} {
		if prompt := brSpawnPrompt(t, agent, true, true, focus); prompt != combinedReviewPrompt {
			t.Fatalf("prompt %q", prompt)
		}
	}
}

func TestFocusIsAppendedToPrompt(t *testing.T) {
	agent := brBareAgent(t)
	focus := "save the deploy workflow as a skill"
	prompt := brSpawnPrompt(t, agent, true, true, &focus)
	if !strings.HasPrefix(prompt, combinedReviewPrompt) || !strings.Contains(prompt, focus) {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestFocusWorksWithMemoryOnlyPrompt(t *testing.T) {
	agent := brBareAgent(t)
	focus := "remember my timezone"
	prompt := brSpawnPrompt(t, agent, true, false, &focus)
	if !strings.HasPrefix(prompt, memoryReviewPrompt) || !strings.Contains(prompt, focus) {
		t.Fatalf("prompt %q", prompt)
	}
}

// Captures the default logger at warning level for the test
func brCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Fork constructor keywords of a routed review built from a parent reasoning at high effort
func brRoutedForkKwargs(t *testing.T, taskCfg map[string]any) AIAgentOptions {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rec := recordForks(t, nil)
	replaceReviewRuntime(t, routedAuxRuntime())
	parent := reviewParentAgent()
	parent.reasoningConfig = map[string]any{"enabled": true, "effort": "high"}
	_, _, routed, err := buildCacheParityFork(context.Background(), parent, taskCfg, 5, "")
	if err != nil || !routed || len(rec.opts) != 1 {
		t.Fatalf("routed %v err %v forks %d", routed, err, len(rec.opts))
	}
	return rec.opts[0]
}

func TestRoutedReviewAppliesConfiguredEffortNotParents(t *testing.T) {
	got := brRoutedForkKwargs(t, map[string]any{"reasoning_effort": "xhigh"}).ReasoningConfig
	if got["enabled"] != true || got["effort"] != "xhigh" || len(got) != 2 {
		t.Fatalf("xhigh %v", got)
	}
	got = brRoutedForkKwargs(t, map[string]any{"reasoning_effort": "none"}).ReasoningConfig
	if got["enabled"] != false || len(got) != 1 {
		t.Fatalf("none %v", got)
	}
}

func TestRoutedReviewFallsBackToProviderDefault(t *testing.T) {
	if got := brRoutedForkKwargs(t, map[string]any{"reasoning_effort": ""}).ReasoningConfig; got != nil {
		t.Fatalf("empty %v", got)
	}
	if got := brRoutedForkKwargs(t, map[string]any{}).ReasoningConfig; got != nil {
		t.Fatalf("unset %v", got)
	}
	logs := brCaptureLog(t)
	if got := brRoutedForkKwargs(t, map[string]any{"reasoning_effort": "ludicrous"}).ReasoningConfig; got != nil {
		t.Fatalf("ludicrous %v", got)
	}
	if !strings.Contains(logs.String(), "ludicrous") {
		t.Fatalf("warning missing: %s", logs.String())
	}
}

// Client declaring whether it can carry tool calls back
type brGuardClient struct{ supports bool }

func (c brGuardClient) SupportsHermesToolCalls() bool { return c.supports }

// Client saying nothing about the capability
type brPlainClient struct{}

// The minimal parent surface the review worker touches before the fork
func brFakeParent(client any) *AIAgent {
	return &AIAgent{provider: "acp-agent", model: "acp-agent", client: client, sessionID: "s1", platform: "cli",
		requestOverrides: map[string]any{}, acpCommand: "acp-agent", acpArgs: []string{"--acp"}, apiKey: "k",
		baseURL: "acp://agent", apiMode: "chat_completions", quietMode: true, activeChildrenLock: &sync.Mutex{},
		env: &tools.Env{Values: &sync.Map{}}}
}

// Runs the review worker with the fork constructor recorded and its conversation skipped, returning the forks built
func brRunWorker(t *testing.T, parent *AIAgent, taskCfg map[string]any) int {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rec := recordForks(t, func(fork *AIAgent) { fork.env = &tools.Env{Values: &sync.Map{}} })
	prev := runReviewForkConversation
	runReviewForkConversation = func(context.Context, *AIAgent, string, []map[string]any) error { return nil }
	t.Cleanup(func() { runReviewForkConversation = prev })
	runReviewInThread(context.Background(), parent, []map[string]any{{"role": "user", "content": "hi"}}, "review please", taskCfg, nil, true, false)
	return len(rec.opts)
}

func TestForkIsSkippedWhenTheProviderCannotEmitToolCalls(t *testing.T) {
	logs := brCaptureLog(t)
	if built := brRunWorker(t, brFakeParent(brGuardClient{supports: false}), nil); built != 0 {
		t.Fatalf("fork built %d times", built)
	}
	if !strings.Contains(logs.String(), "auxiliary.background_review") {
		t.Fatalf("the override knob is not named: %s", logs.String())
	}
}

func TestForkIsSpawnedWhenTheProviderCanEmitToolCalls(t *testing.T) {
	if built := brRunWorker(t, brFakeParent(brGuardClient{supports: true}), nil); built == 0 {
		t.Fatal("fork not built")
	}
}

func TestOrdinaryProvidersAreUnaffected(t *testing.T) {
	if built := brRunWorker(t, brFakeParent(brPlainClient{}), nil); built == 0 {
		t.Fatal("fork not built")
	}
}

func TestAnIncapableProviderStillReviewsWhenTheReviewIsRoutedAway(t *testing.T) {
	replaceReviewRuntime(t, reviewRuntime{Provider: "openai", Model: "gpt-5", APIKey: "k", APIMode: "chat_completions",
		RequestOverrides: map[string]any{}, Args: []string{}, Routed: true})
	if built := brRunWorker(t, brFakeParent(brGuardClient{supports: false}), nil); built == 0 {
		t.Fatal("fork not built")
	}
}
