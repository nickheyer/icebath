package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func salvageRows(rows ...map[string]any) []map[string]any { return rows }

func TestSalvageStubsOldToolsAndKeepsTodoWhenStubbingSuffices(t *testing.T) {
	ctx := context.Background()
	original := salvageRows(map[string]any{"role": "user", "content": "go"}, map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "tool", "tool_call_id": "a", "content": strings.Repeat("A", 4000)},
		map[string]any{"role": "tool", "tool_call_id": "b", "content": strings.Repeat("B", 4000)},
		map[string]any{"role": "tool", "tool_call_id": "c", "content": "keep-latest"})
	grown := append(append([]map[string]any{}, original...), map[string]any{"role": "user", "content": "Current todos:\n- [ ] x", "_todo_snapshot_synthetic": true})
	if estimateRowsRough(ctx, grown) <= estimateRowsRough(ctx, original) {
		t.Fatal("grown candidate must cost more")
	}
	out := SalvageGrownTranscript(ctx, original, grown, nil)
	if out == nil || estimateRowsRough(ctx, out) >= estimateRowsRough(ctx, original) {
		t.Fatalf("salvage failed: %v", out)
	}
	var todo bool
	var toolRows []string
	for _, m := range out {
		todo = todo || m["_todo_snapshot_synthetic"] == true
		if m["role"] == "tool" {
			toolRows = append(toolRows, m["content"].(string))
		}
	}
	if !todo || toolRows[len(toolRows)-1] != "keep-latest" || !strings.Contains(strings.Join(toolRows, "|"), "cleared to save context space") {
		t.Fatalf("unexpected salvage %v %v", todo, toolRows)
	}
}

func TestSalvageDropsTodoOnlyAsLastResort(t *testing.T) {
	ctx := context.Background()
	original := salvageRows(map[string]any{"role": "user", "content": "please do the thing " + strings.Repeat("o", 600)}, map[string]any{"role": "assistant", "content": "ok"})
	grown := salvageRows(map[string]any{"role": "user", "content": "summary of the ask"}, map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "Current todos:\n- [ ] " + strings.Repeat("t", 800), "_todo_snapshot_synthetic": true})
	out := SalvageGrownTranscript(ctx, original, grown, nil)
	if out == nil || estimateRowsRough(ctx, out) >= estimateRowsRough(ctx, original) {
		t.Fatal("salvage failed")
	}
	for _, m := range out {
		if m["_todo_snapshot_synthetic"] == true {
			t.Fatal("todo snapshot kept")
		}
	}
}

func TestSalvageLastResortPreservesPrunedSkillReloadNotice(t *testing.T) {
	ctx := context.Background()
	notice := prunedSkillReloadNoticeHeader + "\nReload with skill_view(name='example-skill') before acting."
	original := salvageRows(map[string]any{"role": "user", "content": "please do the thing " + strings.Repeat("o", 3000)}, map[string]any{"role": "assistant", "content": "ok"})
	grown := salvageRows(map[string]any{"role": "user", "content": "summary of the ask"}, map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "Current todos:\n- [ ] " + strings.Repeat("t", 4000) + "\n\n" + notice, "_todo_snapshot_synthetic": true})
	out := SalvageGrownTranscript(ctx, original, grown, nil)
	if out == nil || estimateRowsRough(ctx, out) >= estimateRowsRough(ctx, original) {
		t.Fatal("salvage failed")
	}
	var snaps []string
	for _, m := range out {
		if m["_todo_snapshot_synthetic"] == true {
			snaps = append(snaps, m["content"].(string))
		}
	}
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0], prunedSkillReloadNoticeHeader) || strings.Contains(snaps[0], "Current todos") {
		t.Fatalf("snapshot rows %q", snaps)
	}
}

func TestSalvageReturnsNoneWhenNothingCanShrink(t *testing.T) {
	out := SalvageGrownTranscript(context.Background(), salvageRows(map[string]any{"role": "user", "content": "tiny"}),
		salvageRows(map[string]any{"role": "user", "content": strings.Repeat("X", 200_000)}), nil)
	if out != nil {
		t.Fatal("expected None")
	}
}

func TestSalvageCapsOversizedSummary(t *testing.T) {
	ctx := context.Background()
	original := salvageRows(map[string]any{"role": "user", "content": "ask " + strings.Repeat("o", 12_000)}, map[string]any{"role": "assistant", "content": "short reply"})
	grown := salvageRows(map[string]any{"role": "user", "content": "[CONTEXT COMPACTION] " + strings.Repeat("S", 20_000) + "\n\n" + state.SummaryEndMarker,
		state.CompressedSummaryMetadataKey: true}, map[string]any{"role": "assistant", "content": "short reply"})
	out := SalvageGrownTranscript(ctx, original, grown, nil)
	if out == nil {
		t.Fatal("salvage failed")
	}
	c := out[0]["content"].(string)
	if len([]rune(c)) >= 12_000 || !strings.HasSuffix(c, state.SummaryEndMarker) {
		t.Fatalf("summary not capped: %d", len(c))
	}
}

func TestSalvageNeverTruncatesMergedSummaryWithLiveUserTail(t *testing.T) {
	merged := salvageRows(map[string]any{"role": "user", "content": "[CONTEXT COMPACTION] " + strings.Repeat("S", 20_000) + "\n\n" + state.SummaryEndMarker + "\n\nLIVE USER REQUEST",
		state.CompressedSummaryMetadataKey: true})
	if SalvageGrownTranscript(context.Background(), salvageRows(map[string]any{"role": "user", "content": strings.Repeat("O", 12_000)}), merged, nil) != nil {
		t.Fatal("expected None")
	}
	if !strings.HasSuffix(merged[0]["content"].(string), "LIVE USER REQUEST") {
		t.Fatal("input mutated")
	}
}

func TestSalvageDoesNotCapPlainOrUnmarkedSummaryShapedUserText(t *testing.T) {
	original := salvageRows(map[string]any{"role": "user", "content": strings.Repeat("O", 20_000)})
	for _, text := range []string{
		"ordinary user text " + strings.Repeat("Q", 12_000) + "\n\n" + state.SummaryEndMarker,
		"[CONTEXT COMPACTION] " + strings.Repeat("U", 12_000) + "\n\n" + state.SummaryEndMarker,
	} {
		out := SalvageGrownTranscript(context.Background(), original, salvageRows(map[string]any{"role": "user", "content": text}), nil)
		if out == nil || out[0]["content"] != text {
			t.Fatal("live user text was capped")
		}
	}
}

func TestSalvageNeverStripsReasoningFromRetainedTurns(t *testing.T) {
	reasoning := func() map[string]any {
		return map[string]any{"reasoning": strings.Repeat("r", 300), "reasoning_content": strings.Repeat("rc", 150),
			"reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": strings.Repeat("d", 300), "signature": "s", "index": 0}}}
	}
	withReasoning := func(m map[string]any) map[string]any {
		for k, v := range reasoning() {
			m[k] = v
		}
		return m
	}
	original := salvageRows(map[string]any{"role": "user", "content": "go"},
		withReasoning(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "a"}}}),
		map[string]any{"role": "tool", "tool_call_id": "a", "content": strings.Repeat("A", 6000)},
		withReasoning(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "b"}}}),
		map[string]any{"role": "tool", "tool_call_id": "b", "content": strings.Repeat("B", 6000)},
		map[string]any{"role": "tool", "tool_call_id": "c", "content": strings.Repeat("C", 6000)},
		map[string]any{"role": "tool", "tool_call_id": "d", "content": "keep-latest"})
	grown := append(append([]map[string]any{}, original...), map[string]any{"role": "user", "content": "Current todos:\n- [ ] x", "_todo_snapshot_synthetic": true})
	out := SalvageGrownTranscript(context.Background(), original, grown, nil)
	if out == nil {
		t.Fatal("salvage failed")
	}
	for _, m := range out {
		if m["role"] != "assistant" {
			continue
		}
		for k, v := range reasoning() {
			if !reflect.DeepEqual(m[k], v) {
				t.Fatalf("reasoning key %s changed", k)
			}
		}
	}
}

func signedThinkingBlock(thinkingChars, sigChars int) map[string]any {
	return map[string]any{"type": "thinking", "thinking": strings.Repeat("t", thinkingChars), "signature": strings.Repeat("s", sigChars)}
}

func budgetRow(extra map[string]any, content string) map[string]any {
	m := map[string]any{"role": "assistant", "content": content}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestBudgetExcludesReasoningDetailsEnvelope(t *testing.T) {
	ctx := context.Background()
	plain := estimateMsgBudgetTokens(ctx, budgetRow(nil, "hello there"), true)
	if d := estimateMsgBudgetTokens(ctx, budgetRow(map[string]any{"reasoning_details": []any{signedThinkingBlock(0, 40_000)}}, "hello there"), true) - plain; d >= 50 {
		t.Fatalf("signed envelope inflated the budget by %d tokens", d)
	}
	if d := estimateMsgBudgetTokens(ctx, budgetRow(map[string]any{"reasoning_details": []any{signedThinkingBlock(8_000, 0)}}, "hello there"), true) - plain; d <= 1_000 {
		t.Fatalf("thinking text dropped from the budget, delta %d", d)
	}
	prose := strings.Repeat("deep thought ", 1_000)
	once := estimateMsgBudgetTokens(ctx, budgetRow(map[string]any{"reasoning_content": prose}, "hi"), true)
	both := estimateMsgBudgetTokens(ctx, budgetRow(map[string]any{"reasoning_content": prose,
		"reasoning_details": []any{map[string]any{"type": "thinking", "thinking": prose, "signature": strings.Repeat("s", 5_000)}}}, "hi"), true)
	if both-once >= 100 {
		t.Fatalf("thinking prose charged twice, delta %d", both-once)
	}
	base := budgetRow(nil, "hi")
	loaded := budgetRow(map[string]any{"codex_reasoning_items": []any{map[string]any{"type": "reasoning", "encrypted_content": strings.Repeat("e", 20_000)}}}, "hi")
	if estimateMsgBudgetTokens(ctx, loaded, true)-estimateMsgBudgetTokens(ctx, base, true) >= 50 {
		t.Fatal("encrypted codex content charged by the budget estimator")
	}
	if estimateRowsRough(ctx, []map[string]any{loaded})-estimateRowsRough(ctx, []map[string]any{base}) >= 50 {
		t.Fatal("encrypted codex content charged by the rough estimator")
	}
}

func TestReasoningDetailsTextChars(t *testing.T) {
	for _, v := range []any{nil, []any{}, ""} {
		if reasoningDetailsTextChars(v) != 0 {
			t.Fatalf("%v counted", v)
		}
	}
	if reasoningDetailsTextChars(strings.Repeat("abcd", 10)) != 40 {
		t.Fatal("string not counted fully")
	}
	blocks := []any{map[string]any{"type": "thinking", "thinking": "abc", "signature": strings.Repeat("S", 999)},
		map[string]any{"type": "redacted_thinking", "data": strings.Repeat("D", 999)}, map[string]any{"type": "text", "text": "xy"}}
	if reasoningDetailsTextChars(blocks) != 5 {
		t.Fatal("envelope keys counted")
	}
}

func TestSummaryRefusalAndFailureClassification(t *testing.T) {
	if !isSummaryRefusal("I'm sorry, but I can't summarize this conversation.") || isSummaryRefusal("I can't do X.\n## Goal\nsummary") {
		t.Fatal("refusal detection mismatch")
	}
	if isSummaryRefusal("I cannot open the file, so the checkpoint below is partial") != true {
		t.Fatal("checkpoint refusal not detected")
	}
	k := classifySummaryFailure(&summaryRuntimeError{"Summary LLM returned empty content"})
	if !k.EmptyContent || k.FallbackReason() != "returned empty content" {
		t.Fatalf("empty content kind %+v", k)
	}
	if k := classifySummaryFailure(&summaryRuntimeError{"summary truncated: finish_reason=length"}); !k.Truncated {
		t.Fatalf("truncated kind %+v", k)
	}
	if (summaryFailureKind{}).FallbackReason() != "failed" {
		t.Fatal("default reason")
	}
	n := 0
	if nextTimeoutCooldown(&n) != 60 || nextTimeoutCooldown(&n) != 300 || nextTimeoutCooldown(&n) != 900 || nextTimeoutCooldown(&n) != 900 {
		t.Fatal("cooldown ladder")
	}
	if !isHygienePreagentOnlyCooldown("Session hygiene compression timed out after 30s") || isHygienePreagentOnlyCooldown(nil) {
		t.Fatal("hygiene cooldown marker")
	}
}

func TestDropShadowedCheckpointsKeepsNewestCarrier(t *testing.T) {
	cp := map[string]any{"type": "compaction", "encrypted_content": "x"}
	msgs := []map[string]any{
		{"role": "assistant", "codex_reasoning_items": []any{cp}},
		{"role": "assistant", "codex_reasoning_items": []any{cp, map[string]any{"type": "reasoning"}}},
		{"role": "user", "content": "next"},
		{"role": "assistant", "codex_reasoning_items": []any{cp}},
	}
	if got := DropShadowedCheckpoints(msgs, "codex_reasoning_items", nil); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("rewritten %v", got)
	}
	if _, ok := msgs[0]["codex_reasoning_items"]; ok || len(msgs[1]["codex_reasoning_items"].([]any)) != 1 || len(msgs[3]["codex_reasoning_items"].([]any)) != 1 {
		t.Fatal("sidecars not rewritten")
	}
}
