package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Engine double exposing the compressor attributes the turn start passes read
type compactionEngine struct {
	ContextEngineBase
	ratio       float64
	preflight   bool
	compressAll bool
}

func (e *compactionEngine) Name() string { return "compaction" }

func (e *compactionEngine) UpdateFromResponse(map[string]any) {}

func (e *compactionEngine) Compress(messages []map[string]any, _ *int, _ *string, _ bool, _ string) ([]map[string]any, error) {
	return messages, nil
}

func (e *compactionEngine) CloneForAgent() ContextEngine {
	c := *e
	c.ContextEngineBase = e.CloneBase()
	return &c
}

func (e *compactionEngine) SummaryTargetRatio() float64 { return e.ratio }

func (e *compactionEngine) ShouldCompressPreflight([]map[string]any) bool { return e.preflight }

func (e *compactionEngine) ShouldCompress(promptTokens *int) bool {
	tokens := e.LastPromptTokens
	if promptTokens != nil {
		tokens = *promptTokens
	}
	return e.compressAll || tokens >= e.ThresholdTokens
}

func newCompactionEngine(threshold, contextLength int) *compactionEngine {
	e := &compactionEngine{ContextEngineBase: NewContextEngineBase(), ratio: 0.5}
	e.ThresholdTokens, e.ContextLength = threshold, contextLength
	return e
}

// Replaces the compress seam with fn for one test
func swapFacadeCompress(t *testing.T, fn func(messages []map[string]any, systemMessage *string, o compressContextOptions) ([]map[string]any, string, error)) {
	t.Helper()
	orig := facadeCompressContext
	facadeCompressContext = func(_ context.Context, _ *AIAgent, messages []map[string]any, systemMessage *string, o compressContextOptions) ([]map[string]any, string, error) {
		return fn(messages, systemMessage, o)
	}
	t.Cleanup(func() { facadeCompressContext = orig })
}

// Pins the compaction request estimate for one test
func swapCompactionTokens(t *testing.T, fn func(ctx context.Context, a *AIAgent, messages []*state.Message, system string) int) {
	t.Helper()
	orig := compactionRequestTokens
	compactionRequestTokens = fn
	t.Cleanup(func() { compactionRequestTokens = orig })
}

func TestCodexNativeAutoCompactionGate(t *testing.T) {
	if !codexNativeAutoCompaction(&AIAgent{apiMode: "codex_app_server", codexAppServerAutoCompaction: "native"}) {
		t.Fatal("native mode")
	}
	if !codexNativeAutoCompaction(&AIAgent{apiMode: "codex_app_server", codexAppServerAutoCompaction: "OFF"}) {
		t.Fatal("off mode")
	}
	if codexNativeAutoCompaction(&AIAgent{apiMode: "codex_app_server", codexAppServerAutoCompaction: "hermes"}) {
		t.Fatal("hermes mode")
	}
	if codexNativeAutoCompaction(&AIAgent{apiMode: "chat_completions"}) {
		t.Fatal("chat completions")
	}
}

func TestDisabledCompressionRearmsOverflowWarnWhenUnderWindow(t *testing.T) {
	a := &AIAgent{contextCompressor: newCompactionEngine(1000, 8000), sessionID: "s1", model: "m"}
	a.lastCtxOverflowWarn = &ctxOverflowWarnKey{}
	snapshot := 7
	a.turnReceivedProviderResponse, a.turnPreflightDisplaySnapshot = true, &snapshot
	msgs := []*state.Message{{Role: "user", Content: "hi"}}
	out, err := runTurnStartCompaction(context.Background(), a, turnStartCompactionRequest{
		Messages: msgs, ActiveSystemPrompt: "sys", CurrentTurnUserIdx: 0, UserMessage: "hi", EffectiveTaskID: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if &out.Messages[0] != &msgs[0] || out.CurrentTurnUserIdx != 0 || out.Compressed || out.Blocked {
		t.Fatalf("outcome %+v", out)
	}
	if a.lastCtxOverflowWarn != nil || a.turnReceivedProviderResponse || a.turnPreflightDisplaySnapshot != nil {
		t.Fatalf("warn %v received %v snapshot %v", a.lastCtxOverflowWarn, a.turnReceivedProviderResponse, a.turnPreflightDisplaySnapshot)
	}
}

func TestNoProgressPreflightFailsClosedOnlyWhenTheRequestExceedsTheWindow(t *testing.T) {
	swapFacadeCompress(t, func(messages []map[string]any, system *string, _ compressContextOptions) ([]map[string]any, string, error) {
		return messages, *system, nil
	})
	preflight := func(contextLength int, transient any, requestTokens int) (CompactionOutcome, error) {
		swapCompactionTokens(t, func(context.Context, *AIAgent, []*state.Message, string) int { return requestTokens })
		engine := newCompactionEngine(65_536, contextLength)
		a := &AIAgent{contextCompressor: engine, sessionID: "s1", model: "m", maxCompressionAttempts: 3, compressionBlockedTransient: transient}
		a.statusCallback = func(string, any) {}
		out := CompactionOutcome{Messages: []*state.Message{{Role: "user", Content: "hi"}}, ActiveSystemPrompt: "sys"}
		system := "sys"
		err := runPreflightPasses(context.Background(), a, &out, engine, requestTokens, &system, "t")
		return out, err
	}
	_, err := preflight(131_072, nil, 356_113)
	var timedOut *PreflightCompressionTimedOut
	if !errors.As(err, &timedOut) || !strings.Contains(err.Error(), "Start a new session with /new") {
		t.Fatalf("over window no progress err = %v", err)
	}
	for _, c := range []struct {
		window    int
		transient any
	}{{1_000_000, nil}, {0, nil}, {131_072, "cooldown:42"}} {
		out, err := preflight(c.window, c.transient, 356_113)
		if err != nil || !out.Blocked {
			t.Fatalf("window %d transient %v: blocked %v err %v", c.window, c.transient, out.Blocked, err)
		}
	}
}

var errCompressionReached = errors.New("compression reached")

func TestAutomaticCallerPassesItsTriggerLabel(t *testing.T) {
	triggerMessages := func() []*state.Message {
		return []*state.Message{
			{Role: "user", Content: strings.Repeat("x", 8_000)},
			{Role: "assistant", Content: strings.Repeat("y", 8_000)},
			{Role: "user", Content: "current ask"},
		}
	}
	newAgent := func() *AIAgent {
		engine := newCompactionEngine(1_000, 200_000)
		engine.ratio, engine.ProtectFirstN, engine.ProtectLastN, engine.LastPromptTokens, engine.compressAll = 0.2, 1, 1, 5_000, true
		a := &AIAgent{contextCompressor: engine, compressionEnabled: true, model: "m", sessionID: "s1", maxCompressionAttempts: 3, quietMode: true}
		a.compressionFeasibilityChecked = true
		a.statusCallback = func(string, any) {}
		return a
	}
	outcome := func(messages []*state.Message) *CompactionOutcome {
		return &CompactionOutcome{Messages: messages, ActiveSystemPrompt: "sys", CurrentTurnUserIdx: len(messages) - 1}
	}
	system := "sys"
	drivers := []struct {
		name  string
		drive func(ctx context.Context, a *AIAgent) error
	}{
		{"idle", func(ctx context.Context, a *AIAgent) error {
			a.compressionIdleCompactAfterSeconds, a.lastActivityTS = 60, loopNow()-3_600
			return idleCompaction(ctx, a, outcome(triggerMessages()), &system, "current ask", "task")
		}},
		{"turn_start_threshold", func(ctx context.Context, a *AIAgent) error {
			return runPreflightPasses(ctx, a, outcome(triggerMessages()), a.contextCompressor, 5_000, &system, "task")
		}},
		{"engine_preflight", func(ctx context.Context, a *AIAgent) error {
			a.contextCompressor.(*compactionEngine).preflight = true
			return enginePreflightMaintenance(ctx, a, outcome(triggerMessages()), a.contextCompressor, 500, &system, "task")
		}},
		{"pre_api", func(ctx context.Context, a *AIAgent) error {
			pressure := 0
			s := &LoopState{Messages: triggerMessages(), ActiveSystemPrompt: "sys", APICallCount: 1, SystemMessage: &system,
				UserMessage: "current ask", MaxCompressionAttempts: 3, EffectiveTaskID: "task", LastPreflightPressure: &pressure}
			_, err := runPreflightCompression(ctx, a, s, a.contextCompressor, 5_000, false, func(int) bool { return false })
			return err
		}},
		{"post_tool", func(ctx context.Context, a *AIAgent) error {
			s := &LoopState{Messages: triggerMessages(), ActiveSystemPrompt: "sys", SystemMessage: &system, UserMessage: "current ask",
				MaxCompressionAttempts: 3, EffectiveTaskID: "task", CurrentTurnUserIdx: 2}
			_, err := compressAfterToolResults(ctx, a, s)
			return err
		}},
	}
	swapCompactionTokens(t, func(ctx context.Context, _ *AIAgent, messages []*state.Message, system string) int {
		return provider.EstimateRequestTokens(ctx, messages, system, nil, true)
	})
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			var labels []string
			swapFacadeCompress(t, func(_ []map[string]any, _ *string, o compressContextOptions) ([]map[string]any, string, error) {
				if o.Trigger != nil {
					labels = append(labels, *o.Trigger)
				}
				return nil, "", errCompressionReached
			})
			if err := d.drive(context.Background(), newAgent()); !errors.Is(err, errCompressionReached) {
				t.Fatalf("driver err = %v", err)
			}
			if len(labels) != 1 || labels[0] != d.name {
				t.Fatalf("labels = %v", labels)
			}
		})
	}
}
