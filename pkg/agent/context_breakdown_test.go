package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// An agent with the given tools and compressor whose prompt tiers the test fixes
func breakdownAgent(t *testing.T, parts SystemPromptParts, tools []any, contextLength, lastPrompt int) *AIAgent {
	t.Helper()
	if tools == nil {
		tools = []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "terminal", "description": "run"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "mcp_demo_tool", "description": "mcp"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "delegate_task", "description": "spawn"}},
		}
	}
	prev := breakdownPromptParts
	breakdownPromptParts = func(context.Context, *AIAgent) (SystemPromptParts, error) { return parts, nil }
	t.Cleanup(func() { breakdownPromptParts = prev })
	comp := &ContextCompressor{}
	comp.ContextEngineBase.ContextLength, comp.ContextEngineBase.LastPromptTokens = contextLength, lastPrompt
	comp.resolvedContextLength = &contextLength
	return &AIAgent{model: "openai/gpt-5.4", tools: tools, memoryEnabled: true, userProfileEnabled: true, contextCompressor: comp}
}

func defaultParts() SystemPromptParts {
	return SystemPromptParts{Stable: "identity and guidance", Volatile: "timestamp line"}
}

func TestBreakdownIncludesMajorCategories(t *testing.T) {
	parts := SystemPromptParts{Stable: "base guidance\n<available_skills>\n  demo:\n    - hello: hi\n</available_skills>",
		Context: "# Project Context\nFollow AGENTS.md", Volatile: "Current time: now"}
	a := breakdownAgent(t, parts, nil, 200_000, 0)
	data, err := ComputeSessionContextBreakdown(context.Background(), a, []map[string]any{{"role": "user", "content": "hello there"}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range data.Categories {
		ids[c.ID] = true
	}
	for _, id := range []string{"system_prompt", "tool_definitions", "rules", "skills", "mcp", "subagent_definitions", "conversation"} {
		if !ids[id] {
			t.Fatalf("category %s missing from %v", id, ids)
		}
	}
	if data.ContextMax != 200_000 || data.EstimatedTotal <= 0 {
		t.Fatalf("max %d total %d", data.ContextMax, data.EstimatedTotal)
	}
}

func TestContextUsedNeverExceedsModelWindow(t *testing.T) {
	history := []map[string]any{{"role": "user", "content": "start"}, {"role": "assistant", "content": "ok"}}
	a := breakdownAgent(t, defaultParts(), nil, 262_144, 0)
	a.turnBaseUsageAnchor = provider.CaptureUsageAnchor(250_000, 1_000, rowsToMessages(history))
	history = append(history, map[string]any{"role": "user", "content": strings.Repeat("x", 800_000)})
	data, err := ComputeSessionContextBreakdown(context.Background(), a, history)
	if err != nil {
		t.Fatal(err)
	}
	if data.ContextSource != "provider_usage_plus_estimate" || data.ContextUsed > data.ContextMax || data.ContextPercent != 100 {
		t.Fatalf("source %s used %d max %d pct %d", data.ContextSource, data.ContextUsed, data.ContextMax, data.ContextPercent)
	}
	seed := &ContextCompressor{lastRealPromptTokens: 150_000}
	seed.ContextEngineBase.ContextLength, seed.ContextEngineBase.LastPromptTokens = 262_144, 400_000
	window := 262_144
	seed.resolvedContextLength = &window
	seeded, ok := ContextUsageFieldsOf(seed)
	if !ok || seeded.ContextUsed > seeded.ContextMax {
		t.Fatalf("seeded %+v", seeded)
	}
}

func TestEmptyTranscriptStillReportsTheConversationCategory(t *testing.T) {
	a := breakdownAgent(t, defaultParts(), []any{map[string]any{"type": "function", "function": map[string]any{"name": "terminal", "description": "run"}}}, 200_000, 0)
	data, err := ComputeSessionContextBreakdown(context.Background(), a, []map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	categories := map[string]int{}
	for _, c := range data.Categories {
		categories[c.ID] = c.Tokens
	}
	tokens, ok := categories["conversation"]
	if !ok || tokens != 0 {
		t.Fatalf("conversation %d present %v", tokens, ok)
	}
	if _, ok := categories["mcp"]; ok {
		t.Fatal("unconfigured mcp reported")
	}
	if _, ok := categories["skills"]; ok {
		t.Fatal("unconfigured skills reported")
	}
}

func breakdownPayload() ContextBreakdown {
	return ContextBreakdown{Categories: []ContextCategory{{ID: "system_prompt", Label: "System prompt", Tokens: 10_000},
		{ID: "tool_definitions", Label: "Tool definitions", Tokens: 20_000}, {ID: "skills", Label: "Skills", Tokens: 5_000},
		{ID: "conversation", Label: "Conversation", Tokens: 15_000}},
		ContextMax: 200_000, ContextPercent: 25, ContextUsed: 50_000, EstimatedTotal: 50_000, Model: "openai/gpt-test"}
}

func TestGridIs5x20AndMostlyFree(t *testing.T) {
	rows := RenderContextGrid(breakdownPayload())
	if len(rows) != 5 {
		t.Fatalf("%d rows", len(rows))
	}
	cells := strings.Split(strings.Join(rows, " "), " ")
	count := func(glyph string) int {
		n := 0
		for _, c := range cells {
			if c == glyph {
				n++
			}
		}
		return n
	}
	if len(cells) != 100 || count("·") != 75 || count("■") != 5 || count("▣") != 10 {
		t.Fatalf("cells %d free %d system %d tools %d", len(cells), count("·"), count("■"), count("▣"))
	}
}

func TestBreakdownLinesGridToggle(t *testing.T) {
	ctx := context.Background()
	withGrid := RenderContextBreakdownLines(ctx, breakdownPayload(), nil, true)
	without := RenderContextBreakdownLines(ctx, breakdownPayload(), nil, false)
	found := false
	for _, line := range withGrid[:5] {
		found = found || strings.Contains(line, "·")
	}
	for _, line := range without[:2] {
		if strings.Contains(line, "·") {
			t.Fatalf("grid glyph without the grid: %q", without[:2])
		}
	}
	if !found {
		t.Fatalf("no grid glyph in %q", withGrid[:5])
	}
}

func TestContextProvenanceFollowsTheSelectedNumber(t *testing.T) {
	pin := 100_000
	ctx := context.Background()
	comp := NewContextCompressor(ctx, ContextCompressorOptions{Model: "fixture", ConfigContextLength: &pin, QuietMode: true})
	prev := breakdownPromptParts
	breakdownPromptParts = func(context.Context, *AIAgent) (SystemPromptParts, error) {
		return SystemPromptParts{Stable: "fixture"}, nil
	}
	t.Cleanup(func() { breakdownPromptParts = prev })
	a := &AIAgent{contextCompressor: comp, model: "fixture", memoryEnabled: true, userProfileEnabled: true}
	messages := []map[string]any{{"role": "user", "content": "fixture question"}}
	for _, source := range []string{"local_estimate", "provider_usage", "provider_usage_plus_estimate"} {
		switch source {
		case "provider_usage":
			comp.UpdateFromResponse(map[string]any{"prompt_tokens": 1234, "completion_tokens": 20})
			a.usageAnchor = provider.CaptureUsageAnchor(1234, 20, rowsToMessages(messages))
			messages = append(messages, map[string]any{"role": "assistant", "content": "fixture answer"})
		case "provider_usage_plus_estimate":
			messages = append(messages, map[string]any{"role": "user", "content": "a new unpriced question"})
		}
		data, err := ComputeSessionContextBreakdown(ctx, a, messages)
		if err != nil {
			t.Fatal(err)
		}
		estimated := source != "provider_usage"
		if data.ContextSource != source || data.ContextEstimated != estimated {
			t.Fatalf("want %s got %s estimated %v", source, data.ContextSource, data.ContextEstimated)
		}
		summary := ""
		for _, line := range RenderContextBreakdownLines(ctx, data, nil, true) {
			if strings.HasPrefix(line, "Context window:") {
				summary = line
				break
			}
		}
		if strings.Contains(summary, "Context window: ~") != estimated {
			t.Fatalf("source %s summary %q", source, summary)
		}
	}
}

func TestManifestMatchesWhatThePromptActuallyLoads(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	mustMkdir(t, filepath.Join(project, ".git"))
	write := func(path, text string) {
		t.Helper()
		mustMkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(project, ".icebath.md"), "icebath rules")
	write(filepath.Join(project, "AGENTS.md"), "root agents rules")
	sub := filepath.Join(project, "pkg")
	write(filepath.Join(sub, "AGENTS.override.md"), "")
	write(filepath.Join(sub, "AGENTS.md"), "pkg agents rules")
	write(filepath.Join(sub, "CLAUDE.md"), "claude rules")
	write(filepath.Join(sub, ".cursorrules"), "cursor rules")
	write(filepath.Join(sub, ".cursor", "rules", "a.mdc"), "mdc rule a")
	homeDir := t.TempDir()
	write(filepath.Join(homeDir, "SOUL.md"), "identity text")
	sources := ListContextFileSources(ctx, nil, ContextFileSourcesOptions{Cwd: &sub, HomeOverride: homeDir})
	prompt, err := BuildContextFilesPrompt(ctx, nil, ContextFilesOptions{Cwd: &sub, HomeOverride: homeDir})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, s := range sources {
		statuses[s.Label] = s.Status
	}
	want := map[string]string{".icebath.md": "loaded", "../AGENTS.md": "shadowed", "AGENTS.override.md": "empty", "AGENTS.md": "shadowed",
		"CLAUDE.md": "shadowed", ".cursorrules": "shadowed", ".cursor/rules/a.mdc": "shadowed", "SOUL.md": "loaded"}
	if len(statuses) != len(want) {
		t.Fatalf("statuses %v", statuses)
	}
	for k, v := range want {
		if statuses[k] != v {
			t.Fatalf("statuses %v want %v", statuses, want)
		}
	}
	for _, src := range sources {
		body := ""
		if src.Chars > 0 {
			raw, _ := os.ReadFile(src.Path)
			body = strings.TrimSpace(string(raw))
		}
		if src.Loaded != (body != "" && strings.Contains(prompt, body)) {
			t.Fatalf("source %+v against prompt %q", src, prompt)
		}
		if src.Chars > 0 && src.EstTokens <= 0 {
			t.Fatalf("source %+v has no token estimate", src)
		}
	}
	if err := os.Remove(filepath.Join(project, ".icebath.md")); err != nil {
		t.Fatal(err)
	}
	statuses = map[string]string{}
	for _, s := range ListContextFileSources(ctx, nil, ContextFileSourcesOptions{Cwd: &sub, HomeOverride: homeDir}) {
		statuses[s.Label] = s.Status
	}
	prompt, err = BuildContextFilesPrompt(ctx, nil, ContextFilesOptions{Cwd: &sub, HomeOverride: homeDir})
	if err != nil {
		t.Fatal(err)
	}
	if statuses["../AGENTS.md"] != "loaded" || statuses["AGENTS.md"] != "loaded" || !strings.Contains(prompt, "root agents rules") {
		t.Fatalf("statuses %v prompt %q", statuses, prompt)
	}
	if statuses["CLAUDE.md"] != "shadowed" || strings.Contains(prompt, "claude rules") {
		t.Fatalf("statuses %v prompt %q", statuses, prompt)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRenderContextFileLinesStatusesAndEmpty(t *testing.T) {
	if lines := RenderContextFileLines(nil); len(lines) != 0 {
		t.Fatalf("lines %q", lines)
	}
	lines := RenderContextFileLines([]ContextFileSource{{Label: "AGENTS.md", Status: "suppressed", EstTokens: 1234},
		{Label: "SOUL.md", Status: "flagged", EstTokens: 5}, {Label: "x", Status: "odd", EstTokens: 0}})
	want := []string{"Context files",
		"○ AGENTS.md ~    1,234 tokens  (not loaded — cwd fell back to the Icebath install tree)",
		"⚠ SOUL.md   ~        5 tokens  (loaded — matched prompt-injection pattern(s); review the file)",
		"• x         ~        0 tokens"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
