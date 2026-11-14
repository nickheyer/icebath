package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

var noticeRe = regexp.MustCompile(`\n\n\.\.\. \[(?P<label>[A-Z ]+) TRUNCATED - (?P<omitted>[\d,]+) (?P<unit>chars|bytes) omitted out of (?P<total>[\d,]+) total\] \.\.\.\n\n`)

func sampleText() string {
	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "%04d|", i)
	}
	return b.String()
}

func assertHeadTail(t *testing.T, result string, budget, total int) string {
	t.Helper()
	text := sampleText()
	locs := noticeRe.FindAllStringSubmatchIndex(result, -1)
	if len(locs) != 1 {
		t.Fatalf("expected one notice, got %d in %q", len(locs), result[:min(200, len(result))])
	}
	loc := locs[0]
	head, tail := result[:loc[0]], result[loc[1]:]
	headLen, tailLen := HeadTailSplit(budget)
	if head != text[:headLen] {
		t.Fatalf("head mismatch")
	}
	if tail != text[len(text)-tailLen:] {
		t.Fatalf("tail mismatch")
	}
	sub := noticeRe.FindStringSubmatch(result)
	omitted, _ := strconv.Atoi(strings.ReplaceAll(sub[noticeRe.SubexpIndex("omitted")], ",", ""))
	totalGot, _ := strconv.Atoi(strings.ReplaceAll(sub[noticeRe.SubexpIndex("total")], ",", ""))
	if omitted != total-budget || totalGot != total {
		t.Fatalf("omitted %d total %d", omitted, totalGot)
	}
	return sub[noticeRe.SubexpIndex("label")]
}

func TestTruncateHeadTail(t *testing.T) {
	text := sampleText()
	if label := assertHeadTail(t, TruncateHeadTail(text, 1000, "OUTPUT"), 1000, 10000); label != "OUTPUT" {
		t.Fatalf("label %q", label)
	}
	if label := assertHeadTail(t, TruncateHeadTail(text, 1000, "MCP RESULT"), 1000, 10000); label != "MCP RESULT" {
		t.Fatalf("label %q", label)
	}
	for _, size := range []int{0, 999, 1000} {
		if got := TruncateHeadTail(text[:size], 1000, "OUTPUT"); got != text[:size] {
			t.Fatalf("size %d changed", size)
		}
	}
	if got := TruncationNotice(1234567, 2000000, "", ""); got != "\n\n... [OUTPUT TRUNCATED - 1,234,567 chars omitted out of 2,000,000 total] ...\n\n" {
		t.Fatalf("notice %q", got)
	}
}

func limitsEnv(t *testing.T, section any) *Env {
	t.Helper()
	ResetToolOutputLimitsCache()
	return &Env{Home: t.TempDir(), Config: config.Tree{"tool_output": section}}
}

func TestToolOutputLimits(t *testing.T) {
	limits := ToolOutputLimits(limitsEnv(t, nil))
	if limits != (OutputLimits{DefaultMaxBytes, DefaultMaxLines, DefaultMaxLineLength}) {
		t.Fatalf("defaults %+v", limits)
	}
	limits = ToolOutputLimits(limitsEnv(t, map[string]any{"max_bytes": 100000, "max_lines": 5000, "max_line_length": 4096}))
	if limits != (OutputLimits{100000, 5000, 4096}) {
		t.Fatalf("overrides %+v", limits)
	}
	if ToolOutputLimits(limitsEnv(t, "nonsense")).MaxBytes != DefaultMaxBytes {
		t.Fatal("non mapping section must fall back")
	}
	for _, bad := range []any{nil, "not a number", -1, 0, []any{}, map[string]any{}} {
		limits = ToolOutputLimits(limitsEnv(t, map[string]any{"max_bytes": bad, "max_lines": bad, "max_line_length": bad}))
		if limits != (OutputLimits{DefaultMaxBytes, DefaultMaxLines, DefaultMaxLineLength}) {
			t.Fatalf("bad %v gave %+v", bad, limits)
		}
	}
	if ToolOutputLimits(limitsEnv(t, map[string]any{"max_bytes": "75000"})).MaxBytes != 75000 {
		t.Fatal("string integer must coerce")
	}
	env := limitsEnv(t, map[string]any{"max_bytes": 111})
	ToolOutputLimits(env)
	env.Config = config.Tree{"tool_output": map[string]any{"max_bytes": 222}}
	if ToolOutputLimits(env).MaxBytes != 111 {
		t.Fatal("limits are cached per home")
	}
	if config.Defaults().Map("tool_output")["max_bytes"] != DefaultMaxBytes {
		t.Fatal("default config must carry tool_output")
	}
}

func TestTruncateResultUsesRegistryCap(t *testing.T) {
	env := limitsEnv(t, map[string]any{"max_bytes": 1000})
	text := sampleText()
	assertHeadTail(t, TruncateResult(env, "no_such_tool", text), 1000, 10000)
	Default.Register(probeDef("zz_truncate_probe", 500))
	defer Default.Remove("zz_truncate_probe")
	assertHeadTail(t, TruncateResult(env, "zz_truncate_probe", text), 500, 10000)
}

func TestResolveThreshold(t *testing.T) {
	env := &Env{Home: t.TempDir(), Config: config.Tree{}}
	cfg := BudgetConfig{DefaultResultSize: DefaultResultSizeChars, ToolOverrides: map[string]int{"read_file": 1}}
	if cfg.ResolveThreshold(env, "read_file") != BudgetUnlimited {
		t.Fatal("pinned wins over override")
	}
	cfg = BudgetConfig{DefaultResultSize: DefaultResultSizeChars, ToolOverrides: map[string]int{"my_tool": 42}}
	if cfg.ResolveThreshold(env, "my_tool") != 42 {
		t.Fatal("override wins over default")
	}
	Default.Register(probeDef("zz_budget_probe", 100000))
	defer Default.Remove("zz_budget_probe")
	if (BudgetConfig{DefaultResultSize: 30000}).ResolveThreshold(env, "zz_budget_probe") != 30000 {
		t.Fatal("registry value capped at default")
	}
	if DefaultBudget.ResolveThreshold(env, "zz_budget_probe") != 100000 {
		t.Fatal("default budget keeps 100K")
	}
	if DefaultBudget.ResolveThreshold(env, "mcp_composio_search_tools") != DefaultMCPResultSizeChars {
		t.Fatal("mcp prefix threshold")
	}
	if DefaultBudget.ResolveThreshold(env, "some_random_tool") != DefaultResultSizeChars {
		t.Fatal("generic default")
	}
	if (BudgetConfig{DefaultResultSize: DefaultResultSizeChars, MCPResultSize: DefaultMCPResultSizeChars, ToolOverrides: map[string]int{"mcp_special": 75000}}).ResolveThreshold(env, "mcp_special") != 75000 {
		t.Fatal("override wins over mcp prefix")
	}
	if (BudgetConfig{DefaultResultSize: 20000, MCPResultSize: 50000}).ResolveThreshold(env, "mcp_anything") != 20000 {
		t.Fatal("mcp capped by scaled default")
	}
}

func TestBudgetForContextWindow(t *testing.T) {
	env := &Env{Home: t.TempDir(), Config: config.Tree{}}
	for _, window := range []int{0, -5} {
		got := BudgetForContextWindow(env, window)
		if got.DefaultResultSize != DefaultBudget.DefaultResultSize || got.TurnBudget != DefaultBudget.TurnBudget || got.MCPResultSize != DefaultBudget.MCPResultSize || got.ToolOverrides != nil {
			t.Fatal("non positive window returns the default")
		}
	}
	cfg := BudgetForContextWindow(env, 65536)
	if cfg.ResolveThreshold(env, "mcp_firecrawl_firecrawl_search") >= 279549 || cfg.DefaultResultSize >= 279549 {
		t.Fatal("scaled budget must constrain an oversized result")
	}
	small := BudgetForContextWindow(env, 16384)
	if small.DefaultResultSize >= 50000 || small.ResolveThreshold(env, "mcp_tool") != small.DefaultResultSize {
		t.Fatalf("small window %+v", small)
	}
	override := &Env{Home: t.TempDir(), Config: config.Tree{"tool_budget": map[string]any{"mcp_result_size_chars": 30000}}}
	if BudgetForContextWindow(override, 0).ResolveThreshold(override, "mcp_composio_multi_execute") != 30000 {
		t.Fatal("config override")
	}
	if BudgetForContextWindow(override, 0).DefaultResultSize != DefaultResultSizeChars {
		t.Fatal("generic tools untouched by the mcp knob")
	}
	if BudgetForContextWindow(override, 200000).MCPResultSize != 30000 {
		t.Fatal("override survives scaling")
	}
	malformed := &Env{Home: t.TempDir(), Config: config.Tree{"tool_budget": "not-a-mapping"}}
	if BudgetForContextWindow(malformed, 0).ResolveThreshold(malformed, "mcp_x_y") != DefaultMCPResultSizeChars {
		t.Fatal("malformed config falls back")
	}
}

func probeDef(name string, maxChars int) Def {
	return Def{Name: name, Toolset: "test", MaxResultChars: maxChars, Schema: map[string]any{"name": name},
		Handler: func(context.Context, *Call) string { return "" }}
}
