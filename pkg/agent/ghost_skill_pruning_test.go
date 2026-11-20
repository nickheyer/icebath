package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

func ghostCompressor(t *testing.T, edit func(*ContextCompressorOptions)) *ContextCompressor {
	return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
		o.ProtectFirstN, o.ProtectLastN = 1, 2
		if edit != nil {
			edit(o)
		}
	})
}

func skillViewPair(callID, name string, size int) []map[string]any {
	return []map[string]any{
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "skill_view", "arguments": `{"name":"` + name + `"}`}}}},
		{"role": "tool", "tool_call_id": callID, "content": "# " + name + " instructions\n" + strings.Repeat("x", size)},
	}
}

func TestGhostedSkillCollectorAndFallbackSkipFailedViews(t *testing.T) {
	failed := dumps(t, obj("success", false, "error", "Skill 'nope' not found. "+strings.Repeat("Available: a, b. ", 30)))
	failedStub := summarizeToolResult("skill_view", `{"name":"nope"}`, failed)
	for _, content := range []string{failed, failedStub} {
		turns := skillViewPair("call_nope", "nope", 6000)
		turns[1]["content"] = content
		if names := collectGhostedSkillNames(turns); len(names) != 0 {
			t.Fatalf("failed view collected %v", names)
		}
		if s := ghostCompressor(t, nil).buildStaticFallbackSummary(turns, ""); strings.Contains(s, "skill_view(name='nope')") {
			t.Fatalf("failed view listed for reload %q", s)
		}
	}
	one, two := skillViewPair("call_1", "one", 300), skillViewPair("call_2", "two", 300)
	row := map[string]any{"role": "assistant", "content": "", "tool_calls": append(append([]any{}, one[0]["tool_calls"].([]any)...), two[0]["tool_calls"].([]any)...)}
	if names := collectGhostedSkillNames([]map[string]any{row, one[1], two[1]}); len(names) != 2 || names[0] != "one" || names[1] != "two" {
		t.Fatalf("got %v", names)
	}
}

func TestMarkerExtractorRoundTripsTheEmittedMarker(t *testing.T) {
	summary := summarizeToolResult("skill_view", `{"name":"pdf"}`, strings.Repeat("x", 6000))
	if names := extractPrunedSkillNames(summary); len(names) != 1 || names[0] != "pdf" || !strings.Contains(summary, skillPrunedMarker("pdf")) {
		t.Fatalf("got %v %q", names, summary)
	}
}

func TestReinjectPrunedSkillMarkers(t *testing.T) {
	out := reinjectPrunedSkillMarkers("The pdf skill was loaded earlier but its content was summarized.", []string{"pdf"})
	if !strings.Contains(out, skillPrunedMarker("pdf")) || !strings.Contains(out, "## Pruned Skills") {
		t.Fatalf("got %q", out)
	}
	out = reinjectPrunedSkillMarkers("body\n"+skillPrunedMarker("alpha"), []string{"alpha", "beta"})
	if strings.Count(out, skillPrunedMarker("alpha")) != 1 || strings.Count(out, skillPrunedMarker("beta")) != 1 {
		t.Fatalf("got %q", out)
	}
	body := reinjectPrunedSkillMarkers("## Goal\nwork\n", []string{"pdf"})
	if got := state.ClassifySummaryContent(state.SummaryPrefix + "\n\n" + body); got != "standalone" {
		t.Fatalf("got %q", got)
	}
	secret := "ghp_" + strings.Repeat("a1B2", 6)
	if out := reinjectPrunedSkillMarkers("body", []string{"x " + secret}); strings.Contains(out, secret) {
		t.Fatalf("secret leaked %q", out)
	}
}

func ghostFiller(n, start int) []map[string]any {
	var out []map[string]any
	for i := 0; i < n; i++ {
		role := "user"
		if (start+i)%2 == 1 {
			role = "assistant"
		}
		out = append(out, map[string]any{"role": role, "content": "filler " + strconv.Itoa(start+i) + " " + strings.Repeat("y", 400)})
	}
	return out
}

func TestProtectedSkillPrune(t *testing.T) {
	c := ghostCompressor(t, nil)
	msgs := append(append(ghostFiller(10, 0), skillViewPair("call_s", "fresh-skill", 6000)...), ghostFiller(6, 10)...)
	result, _ := c.pruneOldToolResults(msgs, 4, nil, pruneMinChars)
	if s := strContent(result[11]); !strings.HasPrefix(s, "# fresh-skill instructions") || strings.Contains(s, skillPrunedMarkerPrefix) {
		t.Fatalf("recent skill pruned %q", s[:60])
	}
	msgs = append(append(ghostFiller(2, 0), skillViewPair("call_s", "fresh-skill", 60000)...), map[string]any{"role": "user", "content": "active ask"})
	result, pruned := c.pruneOldToolResults(msgs, 4, iptr(100), pruneMinChars)
	if pruned < 1 || !strings.Contains(strContent(result[3]), skillPrunedMarker("fresh-skill")) {
		t.Fatalf("pressure demotion kept the skill %d", pruned)
	}
}

func patchTailCut(t *testing.T, idx int) {
	prev := compressorFindTailCutByTokens
	compressorFindTailCutByTokens = func(*ContextCompressor, []map[string]any, int, *int) int { return idx }
	t.Cleanup(func() { compressorFindTailCutByTokens = prev })
}

func ghostSummaryText(t *testing.T, result []map[string]any) string {
	t.Helper()
	for _, m := range result {
		if state.ClassifySummaryContent(m["content"]) != "" {
			return strContent(m)
		}
	}
	t.Fatalf("no summary message in %v", result)
	return ""
}

func TestMarkerSurvivesRealCompress(t *testing.T) {
	t.Run("reinjected_when_summarizer_drops_it", func(t *testing.T) {
		c := ghostCompressor(t, nil)
		msgs := []map[string]any{
			{"role": "system", "content": "System prompt"},
			{"role": "user", "content": "Build the PDF report"},
			toolCallRow("call_pdf", "skill_view", `{"name":"pdf"}`),
			{"role": "tool", "tool_call_id": "call_pdf", "content": "[skill_view] name=pdf (48,201 chars) " + skillPrunedMarker("pdf")},
			{"role": "assistant", "content": "Loaded the skill, working."},
			{"role": "user", "content": "continue"},
			{"role": "assistant", "content": "more work " + strings.Repeat("z", 500)},
			{"role": "user", "content": "latest ask"},
			{"role": "assistant", "content": "ack"},
		}
		patchTailCut(t, 9)
		calls := patchCallLLM(t, func(map[string]any) (any, error) {
			return contentResponse("## Goal\nBuild the PDF report.\n\n## Completed Actions\n1. Loaded some skills and worked on the report."), nil
		})
		result, err := c.CompressContext(context.Background(), msgs, nil, nil, true, "")
		if err != nil || *calls == 0 {
			t.Fatalf("calls %d err %v", *calls, err)
		}
		if !strings.Contains(ghostSummaryText(t, result), skillPrunedMarker("pdf")) || c.previousSummary == nil || !strings.Contains(*c.previousSummary, skillPrunedMarker("pdf")) {
			t.Fatalf("marker lost %v", result)
		}
	})
	t.Run("survives_iterative_recompression", func(t *testing.T) {
		c := ghostCompressor(t, nil)
		msgs := []map[string]any{
			{"role": "system", "content": "System prompt"},
			{"role": "user", "content": state.SummaryPrefix + "\n\n## Goal\nOld work.\n\n## Pruned Skills\n" + skillPrunedMarker("pdf")},
		}
		for i := 0; i < 8; i++ {
			role := "assistant"
			if i%2 == 1 {
				role = "user"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": "turn " + strconv.Itoa(i) + " " + strings.Repeat("q", 300)})
		}
		patchTailCut(t, 8)
		patchCallLLM(t, func(map[string]any) (any, error) { return contentResponse("## Goal\nNext task in flight."), nil })
		result, err := c.CompressContext(context.Background(), msgs, nil, nil, true, "")
		if err != nil || !strings.Contains(ghostSummaryText(t, result), skillPrunedMarker("pdf")) {
			t.Fatalf("marker lost %v %v", result, err)
		}
	})
}

func joinedTexts(messages []map[string]any) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		parts = append(parts, contentStrOrEmpty(m["content"]))
	}
	return strings.Join(parts, "\n")
}

func contentStrOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return dumpsAny(v)
}

func dumpsAny(v any) string {
	return pyrepr.Str(v)
}

func safetyTranscript() []map[string]any {
	messages := []map[string]any{{"role": "system", "content": "sys"}}
	for i := 0; i < 30; i++ {
		messages = append(messages, map[string]any{"role": "user", "content": "q" + strconv.Itoa(i) + " " + strings.Repeat("facts ", 300)},
			map[string]any{"role": "assistant", "content": "a" + strconv.Itoa(i) + " " + strings.Repeat("detail ", 1500)})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "build the pdf"})
	type call struct{ tool, args, content string }
	calls := []call{{"skill_view", `{"name":"pdf"}`, strings.Repeat("P", 9000)}, {"terminal", `{"cmd":"ls"}`, strings.Repeat("A", 9000)}, {"terminal", `{"cmd":"ls"}`, strings.Repeat("B", 9000)}}
	for i := 0; i < 5; i++ {
		calls = append(calls, call{"terminal", `{"cmd":"ls"}`, "ok"})
	}
	for i, c := range calls {
		id := "c" + strconv.Itoa(i)
		messages = append(messages, toolCallRow(id, c.tool, c.args), map[string]any{"role": "tool", "tool_call_id": id, "content": c.content})
	}
	return append(messages, map[string]any{"role": "assistant", "content": "done"}, map[string]any{"role": "user", "content": "latest ask"})
}

func TestPruneOnlyThenFullCompactionKeepsOneSkillSafetyRule(t *testing.T) {
	c := newCompressorWith(t, 1_000_000, func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.5, 2, 4
		o.ProactivePruneTokens, o.ProactivePruneMinResultChars = 48_000, 8_000
	})
	pruned, count := c.pruneToolResultsOnly(safetyTranscript(), iptr(120_000))
	if count == 0 || strings.Count(joinedTexts(pruned), skillSafetyRule) != 1 {
		t.Fatalf("prune-only count %d rules %d", count, strings.Count(joinedTexts(pruned), skillSafetyRule))
	}
	patchCallLLM(t, func(map[string]any) (any, error) {
		return contentResponse("## Goal\nOld chat.\n\n## Completed Actions\n1. talked."), nil
	})
	compacted, err := c.Compress(pruned, nil, nil, true, "")
	if err != nil || !strings.Contains(joinedTexts(compacted), skillPrunedMarkerPrefix) || strings.Count(joinedTexts(compacted), skillSafetyRule) != 1 {
		t.Fatalf("compaction rules %d err %v", strings.Count(joinedTexts(compacted), skillSafetyRule), err)
	}
}

func TestSkillSafetyRuleRidesTheCompactionHandoffNotTheSystemPrompt(t *testing.T) {
	empty := ""
	fresh := toolGuidanceBlock(context.Background(), &AIAgent{validToolNames: map[string]bool{"skill_manage": true}, kanbanWorkerGuidance: &empty})
	if fresh == "" || strings.Contains(fresh, "SKILL_PRUNED") || strings.Contains(fresh, "Skill Safety Rule") {
		t.Fatalf("system prompt carries the rule %q", fresh)
	}
	transcript := func(tool, content string) []map[string]any {
		return []map[string]any{
			{"role": "system", "content": "System prompt"},
			{"role": "user", "content": "Build the PDF report"},
			toolCallRow("call_1", tool, `{"name":"pdf"}`),
			{"role": "tool", "tool_call_id": "call_1", "content": content},
			{"role": "assistant", "content": "Loaded the skill, working."},
			{"role": "user", "content": "continue"},
			{"role": "assistant", "content": "more work " + strings.Repeat("z", 500)},
			{"role": "user", "content": "latest ask"},
			{"role": "assistant", "content": "ack"},
		}
	}
	for _, pruned := range []bool{true, false} {
		c := ghostCompressor(t, func(o *ContextCompressorOptions) { o.Model = "test/model" })
		patchTailCut(t, 9)
		patchCallLLM(t, func(map[string]any) (any, error) {
			return contentResponse("## Goal\nBuild the report.\n\n## Completed Actions\n1. Worked on it."), nil
		})
		msgs := transcript("terminal", "ok")
		if pruned {
			msgs = transcript("skill_view", "[skill_view] name=pdf (48,201 chars) "+skillPrunedMarker("pdf"))
		}
		result, err := c.Compress(msgs, nil, nil, true, "")
		if err != nil {
			t.Fatal(err)
		}
		summary := ghostSummaryText(t, result)
		if strings.Contains(summary, "## Skill Safety Rule\nA skill placeholder containing `[SKILL_PRUNED]`") != pruned || strings.Count(summary, "## Skill Safety Rule") != map[bool]int{true: 1, false: 0}[pruned] {
			t.Fatalf("pruned=%v summary %q", pruned, summary)
		}
	}
}
