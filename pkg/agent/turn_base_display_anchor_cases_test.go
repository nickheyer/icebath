package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Agent over a pinned million token compressor with the system prompt parts reduced to a stable sys tier
func turnBaseBreakdownAgent(t *testing.T) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	window := 1_000_000
	prev := breakdownPromptParts
	breakdownPromptParts = func(context.Context, *AIAgent) (SystemPromptParts, error) {
		return SystemPromptParts{Stable: "sys"}, nil
	}
	t.Cleanup(func() { breakdownPromptParts = prev })
	comp := NewContextCompressor(context.Background(), ContextCompressorOptions{Model: "test/model", ConfigContextLength: &window, QuietMode: true})
	return &AIAgent{contextCompressor: comp, model: "test/model"}
}

// test_turn_base_display_anchor.py TestContextBreakdownPrefersTurnBaseAnchor
func TestContextBreakdownPrefersTurnBaseAnchor(t *testing.T) {
	ctx := context.Background()
	t.Run("test_breakdown_uses_turn_base_over_last_response", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": "start"}, {"role": "assistant", "content": "reply"}}
		turnBase := provider.CaptureUsageAnchor(400_000, 200, rowsToMessages(messages))
		messages = append(messages, map[string]any{"role": "assistant", "content": "anchored reply"})
		lastAnchor := provider.CaptureUsageAnchor(900_000, 50, rowsToMessages(messages))
		a := turnBaseBreakdownAgent(t)
		a.contextCompressor.Base().LastPromptTokens = 900_000
		a.usageAnchor = lastAnchor
		a.turnBaseUsageAnchor.Store(turnBase)
		payload, err := ComputeSessionContextBreakdown(ctx, a, messages)
		if err != nil {
			t.Fatal(err)
		}
		if payload.ContextUsed < 400_000 || payload.ContextUsed >= 450_000 {
			t.Fatalf("context used %d", payload.ContextUsed)
		}
	})
	t.Run("test_breakdown_falls_back_to_last_response_anchor", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": "start"}, {"role": "assistant", "content": "reply"}}
		lastAnchor := provider.CaptureUsageAnchor(300_000, 50, rowsToMessages(messages))
		a := turnBaseBreakdownAgent(t)
		a.contextCompressor.Base().LastPromptTokens = 1
		a.usageAnchor = lastAnchor
		a.turnBaseUsageAnchor.Store(nil)
		payload, err := ComputeSessionContextBreakdown(ctx, a, messages)
		if err != nil {
			t.Fatal(err)
		}
		if payload.ContextUsed < 300_000 {
			t.Fatalf("context used %d", payload.ContextUsed)
		}
	})
}
