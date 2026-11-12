package agent

import (
	"fmt"
	"testing"
)

// Six user and assistant pairs of short history
func preflightWireHistory() []map[string]any {
	var rows []map[string]any
	for i := range 6 {
		rows = append(rows, map[string]any{"role": "user", "content": fmt.Sprintf("q%d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i)})
	}
	return rows
}

// Engine below any threshold that counts its preflight hook consultations
type preflightHookEngine struct {
	compactionEngine
	hookCalls int
}

func (e *preflightHookEngine) ShouldCompressPreflight([]map[string]any) bool {
	e.hookCalls++
	return true
}

func (e *preflightHookEngine) ShouldDeferPreflightToRealUsage(int) bool { return false }

// Prologue agent with compression over engine, counting compress passes and lifecycle statuses
func preflightWireAgent(t *testing.T, engine ContextEngine) (*AIAgent, *int, *[]string) {
	t.Helper()
	a := prologueAgent(t)
	a.compressionEnabled, a.contextCompressor = true, engine
	calls, statuses := new(int), &[]string{}
	a.statusCallback = func(kind string, msg any) {
		if kind == "lifecycle" {
			*statuses = append(*statuses, statusText(msg))
		}
	}
	swapFacadeCompress(t, func(messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
		*calls++
		return messages, "SYSTEM", nil
	})
	return a, calls, statuses
}

// Fails unless the turn history still holds the request history rows unchanged
func assertHistoryKept(t *testing.T, tc *TurnContext, history []map[string]any) {
	t.Helper()
	if len(tc.ConversationHistory) != len(history) {
		t.Fatalf("history rebaselined to %d rows", len(tc.ConversationHistory))
	}
	for i, row := range history {
		if got := tc.ConversationHistory[i]; got.Role != row["role"] || got.Content != row["content"] {
			t.Fatalf("history row %d %+v", i, got)
		}
	}
}

func TestEnginePreflightWire(t *testing.T) {
	caseHome(t)
	t.Run("test_true_engine_noop_does_not_defeat_retry_loop_blocking", func(t *testing.T) {
		engine := &preflightHookEngine{compactionEngine: *newCompactionEngine(1_000_000_000, 0)}
		engine.ProtectFirstN, engine.ProtectLastN = 2, 2
		a, calls, _ := preflightWireAgent(t, engine)
		history := preflightWireHistory()
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.ConversationHistory = history })
		if engine.hookCalls != 1 || *calls != 1 || tc.PreflightCompressionBlocked {
			t.Fatalf("hook %d compress %d blocked %v", engine.hookCalls, *calls, tc.PreflightCompressionBlocked)
		}
		assertHistoryKept(t, tc, history)
	})
	t.Run("test_builtin_compressor_default_sub_threshold_path_unchanged", func(t *testing.T) {
		if new(ContextCompressor).ShouldCompressPreflight([]map[string]any{}) {
			t.Fatal("default preflight hook is true")
		}
		comp := newCompressorWith(t, 1_000_000, func(o *ContextCompressorOptions) {
			o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.85, 2, 2
		})
		a, calls, statuses := preflightWireAgent(t, comp)
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.ConversationHistory = preflightWireHistory() })
		if *calls != 0 || len(*statuses) != 0 || tc.PreflightCompressionBlocked {
			t.Fatalf("compress %d statuses %q blocked %v", *calls, *statuses, tc.PreflightCompressionBlocked)
		}
	})
}
