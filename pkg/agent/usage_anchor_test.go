package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

func anchorRow(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func anchorImageRow() map[string]any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "look at this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 40000)}},
	}}
}

func anchorHistoryWithImages(n int) []map[string]any {
	rows := []map[string]any{anchorRow("user", "start")}
	for i := range n {
		rows = append(rows, anchorRow("assistant", fmt.Sprintf("taking screenshot %d", i)), anchorImageRow())
	}
	return append(rows, anchorRow("assistant", "done looking"))
}

func anchorPreflight(anchor *provider.UsageAnchor, rows []map[string]any, system string) int {
	a := &AIAgent{provider: "openai", usageAnchor: anchor}
	return preflightRequestTokens(context.Background(), a, rowsToMessages(rows), system)
}

func TestPreflightPrefersAnchor(t *testing.T) {
	rows := anchorHistoryWithImages(10)
	anchor := provider.CaptureUsageAnchor(50_000, 250, rowsToMessages(rows))
	rows = append(rows, anchorRow("assistant", "reply"), anchorRow("user", "ok"))
	got := anchorPreflight(anchor, rows, strings.Repeat("SYSTEM PROMPT ", 500))
	want, ok := provider.AnchoredContextTokens(context.Background(), rowsToMessages(rows), anchor, true)
	if !ok || got != want || got <= 50_000 || got >= 50_500 {
		t.Fatalf("preflight %d anchored %d %v", got, want, ok)
	}
}

func TestThresholdDecisionFlipsWithAnchor(t *testing.T) {
	rows := anchorHistoryWithImages(10)
	anchor := provider.CaptureUsageAnchor(12_000, 100, rowsToMessages(rows))
	rows = append(rows, anchorRow("assistant", "reply"))
	heuristic := estimateRowsRough(context.Background(), rows)
	anchored, ok := provider.AnchoredContextTokens(context.Background(), rowsToMessages(rows), anchor, true)
	if heuristic < 15_000 || !ok || anchored >= 15_000 {
		t.Fatalf("heuristic %d anchored %d %v", heuristic, anchored, ok)
	}
}

func codexAnchorTurn(usage bool) *provider.CodexTurnResult {
	if !usage {
		return &provider.CodexTurnResult{}
	}
	obj := pyjson.NewObject()
	for _, kv := range [][2]any{{"inputTokens", 12_000}, {"cachedInputTokens", 0}, {"outputTokens", 100}, {"reasoningOutputTokens", 0}, {"totalTokens", 12_100}} {
		obj.Set(kv[0].(string), kv[1])
	}
	return &provider.CodexTurnResult{TokenUsageLast: obj}
}

func TestCodexTurnUsageSetsAnchor(t *testing.T) {
	rows := anchorHistoryWithImages(10)
	a := &AIAgent{provider: "openai", model: "codex-test-model"}
	provider.RecordCodexAppServerUsage(a, codexAnchorTurn(true), rows)
	if a.usageAnchor == nil || a.usageAnchor.PromptTokens != 12_000 || a.usageAnchor.BaseCount != len(rows) {
		t.Fatalf("anchor %+v", a.usageAnchor)
	}
	if a.sessionPromptTokens != 12_000 || a.sessionCompletionTokens != 100 || a.sessionTotalTokens != 12_100 || a.sessionAPICalls != 1 {
		t.Fatalf("session counters %d %d %d %d", a.sessionPromptTokens, a.sessionCompletionTokens, a.sessionTotalTokens, a.sessionAPICalls)
	}
	rows = append(rows, anchorRow("user", "follow-up"))
	if got := preflightRequestTokens(context.Background(), a, rowsToMessages(rows), ""); got <= 12_000 || got >= 12_200 {
		t.Fatalf("preflight %d", got)
	}
}

func TestCodexUsageLessTurnKeepsPreviousAnchor(t *testing.T) {
	rows := anchorHistoryWithImages(2)
	prior := provider.CaptureUsageAnchor(9_000, 50, rowsToMessages(rows))
	a := &AIAgent{provider: "openai", model: "codex-test-model", usageAnchor: prior}
	provider.RecordCodexAppServerUsage(a, codexAnchorTurn(false), rows)
	if a.usageAnchor != prior {
		t.Fatalf("anchor replaced %+v", a.usageAnchor)
	}
}

func TestSetUsageAnchorMirrorsTheSessionRowAndRestoreHonoursThePricedPrefix(t *testing.T) {
	a, db := newPersistTestAgent(t, "s1")
	rows := anchorHistoryWithImages(1)
	anchor := provider.CaptureUsageAnchor(3_000, 20, rowsToMessages(rows))
	a.setUsageAnchor(anchor, true)
	if a.usageAnchor != anchor || a.turnBaseUsageAnchor != anchor {
		t.Fatalf("set %+v %+v", a.usageAnchor, a.turnBaseUsageAnchor)
	}
	a.setUsageAnchor(provider.CaptureUsageAnchor(3_100, 20, rowsToMessages(rows)), false)
	if a.turnBaseUsageAnchor != anchor {
		t.Fatal("non turn base anchor replaced the turn base")
	}
	fresh := &AIAgent{sessionID: "s1", sessionDB: db}
	restoreUsageAnchor(fresh, rows)
	if fresh.usageAnchor == nil || fresh.usageAnchor.PromptTokens != 3_100 {
		t.Fatalf("restored %+v", fresh.usageAnchor)
	}
	stale := &AIAgent{sessionID: "s1", sessionDB: db}
	restoreUsageAnchor(stale, []map[string]any{anchorRow("user", "rewritten")})
	if stale.usageAnchor != nil {
		t.Fatal("stale anchor adopted")
	}
	if raw, _, _ := db.GetModelConfigValue("s1", provider.UsageAnchorModelConfigKey); raw != nil {
		t.Fatalf("stale blob kept %v", raw)
	}
	a.setUsageAnchor(nil, false)
	if a.usageAnchor != nil || a.turnBaseUsageAnchor != nil {
		t.Fatal("nil anchor did not clear both")
	}
}
