package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestPreflightFallsBackWithoutAnchor(t *testing.T) {
	if got := anchorPreflight(nil, anchorHistoryWithImages(3), "sys"); got < 4_500 {
		t.Fatalf("heuristic preflight %d", got)
	}
}

func TestSabotageDisablingAnchorChangesResult(t *testing.T) {
	rows := anchorHistoryWithImages(10)
	anchor := provider.CaptureUsageAnchor(12_000, 100, rowsToMessages(rows))
	rows = append(rows, anchorRow("assistant", "reply"), anchorRow("user", "ok"))
	anchored := anchorPreflight(anchor, rows, "")
	sabotaged := anchorPreflight(nil, rows, "")
	if sabotaged-anchored <= 2_800 {
		t.Fatalf("anchored %d sabotaged %d", anchored, sabotaged)
	}
}

func anchorReasoningMsg(role, content, reasoning string) *state.Message {
	return &state.Message{Role: role, Content: content, ReasoningContent: reasoning}
}

func TestChargeStaleThinkingKwargDeltaExcludesStaleReasoning(t *testing.T) {
	ctx := context.Background()
	messages := []*state.Message{{Role: "user", Content: "start"}, {Role: "assistant", Content: "base reply"}}
	anchor := provider.CaptureUsageAnchor(10_000, 100, messages)
	if anchor == nil {
		t.Fatal("no anchor")
	}
	bigThinking := strings.Repeat("deliberation ", 5_000)
	messages = append(messages, &state.Message{Role: "assistant", Content: "the anchored reply itself"},
		anchorReasoningMsg("assistant", "step one", bigThinking),
		&state.Message{Role: "tool", Content: "tool output", ToolCallID: "c1"},
		anchorReasoningMsg("assistant", "step two", bigThinking))
	charged, okCharged := provider.AnchoredContextTokens(ctx, messages, anchor, true)
	uncharged, okUncharged := provider.AnchoredContextTokens(ctx, messages, anchor, false)
	if !okCharged || !okUncharged {
		t.Fatal("anchor did not match")
	}
	oneThinking := provider.EstimateMessagesTokens([]*state.Message{anchorReasoningMsg("assistant", "", bigThinking)}, provider.DefaultImageTokenCost, true)
	if float64(charged-uncharged) < float64(oneThinking)*0.9 {
		t.Fatalf("charged %d uncharged %d one thinking %d", charged, uncharged, oneThinking)
	}
	if uncharged < 10_000+100 {
		t.Fatalf("uncharged %d dropped the anchor base", uncharged)
	}
}

func TestChargeStaleThinkingKwargDefaultRemainsFullCharge(t *testing.T) {
	ctx := context.Background()
	messages := []*state.Message{{Role: "user", Content: "s"}, {Role: "assistant", Content: "r"}}
	anchor := provider.CaptureUsageAnchor(1_000, 10, messages)
	messages = append(messages, &state.Message{Role: "assistant", Content: "reply"})
	charged, _ := provider.AnchoredContextTokens(ctx, messages, anchor, true)
	if got, _ := provider.AnchoredContextTokens(ctx, messages, anchor, true); got != charged {
		t.Fatalf("%d vs %d", got, charged)
	}
}

func TestInvalidationSitesClearTurnBaseAnchorClearingTheAnchorClearsTheTurnBaseToo(t *testing.T) {
	messages := rowsToMessages([]map[string]any{anchorRow("user", "a"), anchorRow("assistant", "b")})
	a := &AIAgent{}
	first := provider.CaptureUsageAnchor(1_000, 10, messages)
	a.setUsageAnchor(first, true)
	a.setUsageAnchor(provider.CaptureUsageAnchor(2_000, 10, messages), false)
	if a.turnBaseUsageAnchor.Load() != first {
		t.Fatalf("turn base %+v", a.turnBaseUsageAnchor.Load())
	}
	a.setUsageAnchor(nil, false)
	if a.usageAnchor != nil || a.turnBaseUsageAnchor.Load() != nil {
		t.Fatalf("anchors %+v %+v", a.usageAnchor, a.turnBaseUsageAnchor.Load())
	}
}
