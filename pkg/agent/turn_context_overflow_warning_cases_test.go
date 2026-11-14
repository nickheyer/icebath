package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Compressor over a 96K window where the small context floor puts the threshold at 72K tokens
func overflowWarnCompressor(t *testing.T) *ContextCompressor {
	t.Helper()
	return newCompressorWith(t, 96000, func(o *ContextCompressorOptions) {
		o.Model, o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = "test-model", 0.65, 2, 3
	})
}

// Prologue agent with compression on over comp, recording warnings and counting compress passes
func overflowWarnAgent(t *testing.T, comp *ContextCompressor) (*AIAgent, *[]string, *int) {
	t.Helper()
	a := prologueAgent(t)
	a.compressionEnabled, a.contextCompressor = true, comp
	warnings, calls := &[]string{}, new(int)
	a.statusCallback = func(kind string, msg any) {
		if kind == "warn" {
			*warnings = append(*warnings, statusText(msg))
		}
	}
	swapFacadeCompress(t, func(messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
		*calls++
		return messages, "SYSTEM", nil
	})
	return a, warnings, calls
}

// Runs the turn prologue with the preflight gate open and the request estimate pinned to tokens
func overflowWarnBuild(t *testing.T, a *AIAgent, tokens int) {
	t.Helper()
	swapCompactionTokens(t, func(context.Context, *AIAgent, []*state.Message, string) int { return tokens })
	huge := strings.Repeat("hello ", 60000)
	prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = huge })
}

func TestShouldCompressInfo(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	over := 73000
	t.Run("test_cooldown_reports_reason", func(t *testing.T) {
		comp := overflowWarnCompressor(t)
		comp.LastPromptTokens, comp.lastRealPromptTokens = over, over
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 60
		should, reason := comp.ShouldCompressInfo(&over)
		if should || reason == nil || !strings.HasPrefix(*reason, "cooldown:") {
			t.Fatalf("should %v reason %v", should, reason)
		}
	})
	t.Run("test_should_compress_bool_shim_unchanged", func(t *testing.T) {
		comp := overflowWarnCompressor(t)
		comp.LastPromptTokens, comp.lastRealPromptTokens = over, over
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 60
		var result any = comp.ShouldCompress(&over)
		if b, ok := result.(bool); !ok || b {
			t.Fatalf("result %#v", result)
		}
	})
}

func TestTurnContextOverflowWarning(t *testing.T) {
	t.Run("test_warns_on_cooldown_block", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		comp := overflowWarnCompressor(t)
		comp.LastPromptTokens, comp.lastRealPromptTokens = 73000, 73000
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 30
		a, warnings, _ := overflowWarnAgent(t, comp)
		overflowWarnBuild(t, a, 80000)
		if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "cooldown") {
			t.Fatalf("warnings %q", *warnings)
		}
	})
	t.Run("test_dedup_resets_when_block_clears_while_over_threshold", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		comp := overflowWarnCompressor(t)
		comp.LastPromptTokens, comp.lastRealPromptTokens = 73000, 73000
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 30
		a, warnings, calls := overflowWarnAgent(t, comp)
		overflowWarnBuild(t, a, 80000)
		if len(*warnings) != 1 {
			t.Fatalf("turn 1 warnings %q", *warnings)
		}
		comp.summaryFailureCooldownUntil = 0
		overflowWarnBuild(t, a, 80000)
		if *calls == 0 || a.lastCtxOverflowWarn != nil {
			t.Fatalf("turn 2 compress calls %d warn key %v", *calls, a.lastCtxOverflowWarn)
		}
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 30
		overflowWarnBuild(t, a, 80000)
		if len(*warnings) != 2 {
			t.Fatalf("turn 3 warnings %q", *warnings)
		}
	})
	t.Run("test_no_warning_below_threshold_with_persisted_cooldown", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		comp := overflowWarnCompressor(t)
		comp.LastPromptTokens = 10000
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 30
		a, warnings, _ := overflowWarnAgent(t, comp)
		overflowWarnBuild(t, a, 10000)
		if len(*warnings) != 0 {
			t.Fatalf("warnings %q", *warnings)
		}
	})
}

// Plugin engine implementing only the bool gate, answering with the fixed verdict
type boolGateEngine struct {
	ContextEngineBase
	verdict bool
}

func (e *boolGateEngine) Name() string                      { return "bool-gate" }
func (e *boolGateEngine) UpdateFromResponse(map[string]any) {}
func (e *boolGateEngine) ShouldCompress(*int) bool          { return e.verdict }
func (e *boolGateEngine) Compress(messages []map[string]any, _ *int, _ *string, _ bool, _ string) ([]map[string]any, error) {
	return messages, nil
}
func (e *boolGateEngine) CloneForAgent() ContextEngine { return e }

func TestPluginEngineDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_abc_default_returns_tuple", func(t *testing.T) {
		tokens := 123456
		if should, reason := ShouldCompressInfo(&boolGateEngine{ContextEngineBase: NewContextEngineBase()}, &tokens); should || reason != nil {
			t.Fatalf("got %v %v", should, reason)
		}
	})
	t.Run("test_abc_default_delegates_to_should_compress", func(t *testing.T) {
		tokens := 1
		if should, reason := ShouldCompressInfo(&boolGateEngine{ContextEngineBase: NewContextEngineBase(), verdict: true}, &tokens); !should || reason != nil {
			t.Fatalf("got %v %v", should, reason)
		}
	})
}
