package agent

import (
	"context"
	"testing"
)

// Engine holding a pending compaction checkpoint whose prune calls are counted
type pendingCheckpointEngine struct {
	stubEngine
	pruneCalls int
}

func (e *pendingCheckpointEngine) AwaitingRealUsageAfterCompression() bool { return true }
func (e *pendingCheckpointEngine) ShouldCompress(*int) bool                { return true }
func (e *pendingCheckpointEngine) PruneToolResultsOnly(messages []map[string]any, _ *int) ([]map[string]any, int) {
	e.pruneCalls++
	return messages, 0
}

func TestProactivePruneLoopWiring(t *testing.T) {
	t.Run("test_pending_checkpoint_waits_without_warning_or_pruning", func(t *testing.T) {
		caseHome(t)
		engine := &pendingCheckpointEngine{stubEngine: *newStubEngine(289_000, 0.8)}
		engine.LastPromptTokens, engine.ThresholdTokens = 255_933, 231_200
		a := spAgent(t)
		a.contextCompressor, a.compressionEnabled = engine, true
		messages := rowsToMessages([]map[string]any{{"role": "user", "content": "continue"}})
		system := "system"
		s := &LoopState{Messages: messages, SystemMessage: &system, UserMessage: "continue", ActiveSystemPrompt: "system", MaxCompressionAttempts: 3}
		endTurn, err := compressAfterToolResults(context.Background(), a, s)
		if err != nil || endTurn {
			t.Fatalf("end turn %v err %v", endTurn, err)
		}
		if len(s.Messages) != 1 || s.Messages[0] != messages[0] || &s.Messages[0] != &messages[0] {
			t.Fatal("messages replaced while the checkpoint was pending")
		}
		if a.lastCtxOverflowWarn != nil || engine.pruneCalls != 0 || engine.compressCalled {
			t.Fatalf("warn %v prunes %d compress %v", a.lastCtxOverflowWarn, engine.pruneCalls, engine.compressCalled)
		}
	})
}
