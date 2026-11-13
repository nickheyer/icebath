package memory

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

type insightsFixture struct {
	db   *state.DB
	conn *sql.DB
}

// Temp session store plus a raw connection on the same file for direct row edits
func newInsightsFixture(t *testing.T) *insightsFixture {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "test_insights.db")
	db, err := state.Open(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); db.Close() })
	return &insightsFixture{db: db, conn: conn}
}

func (f *insightsFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.conn.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *insightsFixture) session(t *testing.T, id, source, model string) {
	t.Helper()
	if err := f.db.CreateSession(state.NewSession{ID: id, Source: source, Model: model, UserID: "user1"}); err != nil {
		t.Fatal(err)
	}
}

func (f *insightsFixture) tokens(t *testing.T, id string, u insightsUsage) {
	t.Helper()
	if err := f.db.UpdateTokenCounts(id, u.delta()); err != nil {
		t.Fatal(err)
	}
}

func (f *insightsFixture) msg(t *testing.T, id, role, content, toolName string, calls ...state.ToolCall) {
	t.Helper()
	fields := map[string]any{"role": role, "content": content}
	if toolName != "" {
		fields["tool_name"] = toolName
	}
	if len(calls) > 0 {
		list := make([]any, len(calls))
		for i, tc := range calls {
			list[i] = state.ToolCallToDict(tc)
		}
		fields["tool_calls"] = list
	}
	if _, err := f.db.AppendMessage(context.Background(), id, fields, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func insightsCall(name, args string) state.ToolCall {
	return state.ToolCall{Function: state.ToolCallFunction{Name: name, Arguments: args}}
}

func (f *insightsFixture) ended(t *testing.T, id string, startedAgo, length float64) {
	t.Helper()
	now := float64(time.Now().Unix())
	f.exec(t, "UPDATE sessions SET started_at = ? WHERE id = ?", now-startedAgo, id)
	if err := f.db.EndSession(id, "user_exit"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE sessions SET ended_at = ? WHERE id = ?", now-startedAgo+length, id)
}

// The populated store of the hermes fixture, four sessions in the window and one 45 days old
func populatedInsights(t *testing.T) *insightsFixture {
	f := newInsightsFixture(t)
	day := 86400.0
	f.session(t, "s1", "cli", "anthropic/claude-sonnet-4-20250514")
	f.ended(t, "s1", 2*day, 3600)
	f.tokens(t, "s1", insightsUsage{InputTokens: 50000, OutputTokens: 15000})
	f.msg(t, "s1", "user", "Hello, help me fix a bug", "")
	f.msg(t, "s1", "assistant", "Sure, let me look into that.", "")
	f.msg(t, "s1", "assistant", "Let me search the files.", "", insightsCall("search_files", ""))
	f.msg(t, "s1", "tool", "Found 3 matches", "search_files")
	f.msg(t, "s1", "assistant", "Let me read the file.", "", insightsCall("read_file", ""))
	f.msg(t, "s1", "tool", "file contents...", "read_file")
	f.msg(t, "s1", "assistant", "I found the bug. Let me fix it.", "", insightsCall("patch", ""))
	f.msg(t, "s1", "tool", "patched successfully", "patch")
	f.msg(t, "s1", "assistant", "Let me load the PR workflow skill.", "", insightsCall("skill_view", `{"name":"github-pr-workflow"}`))
	f.msg(t, "s1", "user", "Thanks!", "")
	f.msg(t, "s1", "assistant", "You're welcome!", "")

	f.session(t, "s2", "telegram", "gpt-4o")
	f.ended(t, "s2", 5*day, 1800)
	f.tokens(t, "s2", insightsUsage{InputTokens: 20000, OutputTokens: 8000})
	f.msg(t, "s2", "user", "Search the web for something", "")
	f.msg(t, "s2", "assistant", "Searching...", "", insightsCall("web_search", ""))
	f.msg(t, "s2", "tool", "results...", "web_search")
	f.msg(t, "s2", "assistant", "Here's what I found", "")

	f.session(t, "s3", "cli", "deepseek-chat")
	f.ended(t, "s3", 10*day, 7200)
	f.tokens(t, "s3", insightsUsage{InputTokens: 100000, OutputTokens: 40000})
	f.msg(t, "s3", "user", "Run this terminal command", "")
	f.msg(t, "s3", "assistant", "Running...", "", insightsCall("terminal", ""))
	f.msg(t, "s3", "tool", "output...", "terminal")
	f.msg(t, "s3", "assistant", "Let me run another", "", insightsCall("terminal", ""))
	f.msg(t, "s3", "tool", "more output...", "terminal")
	f.msg(t, "s3", "assistant", "And search files", "", insightsCall("search_files", ""))
	f.msg(t, "s3", "tool", "found stuff", "search_files")
	f.msg(t, "s3", "assistant", "Load the debugging skill.", "", insightsCall("skill_view", `{"name":"systematic-debugging"}`))

	f.session(t, "s4", "discord", "anthropic/claude-sonnet-4-20250514")
	f.ended(t, "s4", day, 900)
	f.tokens(t, "s4", insightsUsage{InputTokens: 10000, OutputTokens: 5000})
	f.msg(t, "s4", "user", "Quick question", "")
	f.msg(t, "s4", "assistant", "Sure, go ahead", "")
	f.msg(t, "s4", "assistant", "Load and update GitHub skills.", "",
		insightsCall("skill_view", `{"name":"github-pr-workflow"}`), insightsCall("skill_manage", `{"name":"github-code-review"}`))

	f.session(t, "s_old", "cli", "gpt-4o-mini")
	f.ended(t, "s_old", 45*day, 600)
	f.tokens(t, "s_old", insightsUsage{InputTokens: 5000, OutputTokens: 2000})
	f.msg(t, "s_old", "user", "old message", "")
	f.msg(t, "s_old", "assistant", "old reply", "")
	return f
}

func (f *insightsFixture) engine(t *testing.T) *InsightsEngine {
	t.Helper()
	e, err := OpenInsightsEngine(f.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func (f *insightsFixture) report(t *testing.T, days int, source string) (*InsightsEngine, *InsightsReport) {
	t.Helper()
	e := f.engine(t)
	r, err := e.Generate(context.Background(), days, source)
	if err != nil {
		t.Fatal(err)
	}
	return e, r
}

func TestInsightsPricingHelpers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	for _, m := range []string{"FP16_Hermes_4.5", "my-custom-model", "glm-5", "", "some-opus-model", "future-sonnet-v2"} {
		if provider.HasKnownPricing(ctx, m, "", "", "") {
			t.Fatalf("%q must not have known pricing", m)
		}
	}
	cost, status, err := insightsEstimateCost(ctx, "anthropic/claude-sonnet-4-20250514",
		map[string]any{"input_tokens": int64(1000), "output_tokens": int64(500), "cache_read_tokens": int64(2000), "cache_write_tokens": int64(400)}, "anthropic", "")
	if err != nil || status != "estimated" {
		t.Fatalf("status %q err %v", status, err)
	}
	if want := (1000*3.0 + 500*15.0 + 2000*0.30 + 400*3.75) / 1_000_000; math.Abs(cost-want) > 0.0001 {
		t.Fatalf("cost %v want %v", cost, want)
	}
	if provider.FormatDurationCompact(45) != "45s" || provider.FormatDurationCompact(5400) != "1h 30m" {
		t.Fatal("duration format")
	}
}

func TestInsightsBarChart(t *testing.T) {
	bars := insightsBarChart([]int64{10, 5, 0, 20}, 10)
	if len(bars) != 4 || len([]rune(bars[3])) != 10 || len([]rune(bars[0])) != 5 || bars[2] != "" {
		t.Fatalf("bars %q", bars)
	}
	for _, b := range insightsBarChart([]int64{0, 0, 0}, 10) {
		if b != "" {
			t.Fatalf("zero bars %q", b)
		}
	}
}

func TestInsightsEmptyStore(t *testing.T) {
	e, r := newInsightsFixture(t).report(t, 30, "")
	if !r.Empty || r.Overview != nil || e.FormatTerminal(r) == "" || e.FormatGateway(r) == "" {
		t.Fatalf("empty report %+v", r)
	}
}

func TestInsightsOverviewTokenTotals(t *testing.T) {
	_, r := populatedInsights(t).report(t, 30, "")
	in, out := int64(50000+20000+100000+10000), int64(15000+8000+40000+5000)
	if o := r.Overview; o.TotalInputTokens != in || o.TotalOutputTokens != out || o.TotalTokens != in+out {
		t.Fatalf("overview %+v", o)
	}
}

func insightsModels(r *InsightsReport) map[string]InsightsModel {
	out := map[string]InsightsModel{}
	for _, m := range r.Models {
		out[m.Model] = m
	}
	return out
}

func TestInsightsModelBreakdownSplitsMidSessionSwitch(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "sw", "cli", "deepseek/deepseek-v4-pro")
	f.tokens(t, "sw", insightsUsage{InputTokens: 40000, OutputTokens: 8000, Model: "deepseek/deepseek-v4-pro", BillingProvider: "deepseek", APICalls: 2})
	if err := f.db.UpdateSessionModel("sw", "anthropic/claude-opus-4.8", "", ""); err != nil {
		t.Fatal(err)
	}
	f.tokens(t, "sw", insightsUsage{InputTokens: 50000, OutputTokens: 4000, Model: "anthropic/claude-opus-4.8", BillingProvider: "openrouter", APICalls: 3})
	_, r := f.report(t, 30, "")
	m := insightsModels(r)
	ds, op := m["deepseek-v4-pro"], m["claude-opus-4.8"]
	if ds.InputTokens.I != 40000 || op.InputTokens.I != 50000 || op.APICalls.I != 3 || ds.TotalTokens.I != 48000 || op.TotalTokens.I != 54000 {
		t.Fatalf("models %+v", m)
	}
}

func TestInsightsOverviewCostMatchesPerModelStoredCost(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "cost", "cli", "model-a")
	f.tokens(t, "cost", insightsUsage{InputTokens: 10, Model: "model-a", BillingProvider: "custom", EstimatedCostUSD: 1.25, ActualCostUSD: 1.0,
		HasActualCost: true, CostStatus: "estimated", CostSource: "provider", APICalls: 1})
	if err := f.db.UpdateSessionModel("cost", "model-b", "custom-b", ""); err != nil {
		t.Fatal(err)
	}
	f.tokens(t, "cost", insightsUsage{InputTokens: 20, Model: "model-b", BillingProvider: "custom-b", EstimatedCostUSD: 2.5, ActualCostUSD: 2.0,
		HasActualCost: true, CostStatus: "estimated", CostSource: "provider", APICalls: 1})
	_, r := f.report(t, 30, "")
	sum := 0.0
	for _, m := range r.Models {
		sum += m.Cost
	}
	if math.Abs(sum-3.75) > 1e-9 || math.Abs(r.Overview.EstimatedCost-3.75) > 1e-9 || math.Abs(r.Overview.ActualCost-3.0) > 1e-9 {
		t.Fatalf("sum %v overview %+v", sum, r.Overview)
	}
}

func insightsFindTool(r []InsightsTool, name string) (InsightsTool, bool) {
	for _, t := range r {
		if t.Tool == name {
			return t, true
		}
	}
	return InsightsTool{}, false
}

func TestInsightsToolUsageSumsDisjointSessions(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "gw", "gateway", "m")
	f.msg(t, "gw", "tool", "r", "search_files")
	f.session(t, "cli", "cli", "m")
	f.msg(t, "cli", "assistant", "x", "", insightsCall("search_files", "{}"))
	f.session(t, "both", "cli", "m")
	f.msg(t, "both", "assistant", "x", "", insightsCall("search_files", "{}"))
	f.msg(t, "both", "tool", "r", "search_files")
	_, r := f.report(t, 30, "")
	if tool, ok := insightsFindTool(r.Tools, "search_files"); !ok || tool.Count != 3 {
		t.Fatalf("tools %+v", r.Tools)
	}
}

func TestInsightsToolAndSkillBreakdown(t *testing.T) {
	_, r := populatedInsights(t).report(t, 30, "")
	for _, name := range []string{"terminal", "search_files", "read_file", "patch", "web_search"} {
		if _, ok := insightsFindTool(r.Tools, name); !ok {
			t.Fatalf("missing %s in %+v", name, r.Tools)
		}
	}
	pct := 0.0
	for _, tool := range r.Tools {
		pct += tool.Percentage
	}
	if term, _ := insightsFindTool(r.Tools, "terminal"); term.Count != 2 || math.Abs(pct-100) > 0.1 {
		t.Fatalf("terminal %+v pct %v", term, pct)
	}
	s := r.Skills
	if s.DistinctSkillsUsed != 3 || s.TotalSkillLoads != 3 || s.TotalSkillEdits != 1 || s.TotalSkillActions != 4 {
		t.Fatalf("skills %+v", s)
	}
	top := s.TopSkills[0]
	if top.Skill != "github-pr-workflow" || top.ViewCount != 2 || top.ManageCount != 0 || top.TotalCount != 2 || top.LastUsedAt == nil {
		t.Fatalf("top skill %+v", top)
	}
}

var insightsPinned = []struct {
	base   string
	source bool
}{{"tool_calls", false}, {"tool_calls", true}, {"skill_calls", false}, {"skill_calls", true}}

func insightsPinnedSQL(e *InsightsEngine, base string, source bool) (string, []any) {
	if source {
		return e.queries[base][1], []any{0.0, "cli", "cli"}
	}
	return e.queries[base][0], []any{0.0}
}

func insightsRows(t *testing.T, conn *sql.DB, q string, args []any) []string {
	t.Helper()
	rows, err := conn.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, v := range vals {
			b.WriteString(insightsCellStr(v) + "\x00")
		}
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out
}

func TestInsightsAssistantCallQueriesUsePartialIndex(t *testing.T) {
	f := populatedInsights(t)
	for _, idx := range insightsRows(t, f.conn, "SELECT sql FROM sqlite_master WHERE type = 'index'", nil) {
		if strings.Contains(idx, "ANALYZE") {
			t.Fatal("ANALYZE ran")
		}
	}
	e := f.engine(t)
	if !e.hasAssistantCallsIndex {
		t.Fatal("index probe failed")
	}
	for _, p := range insightsPinned {
		q, args := insightsPinnedSQL(e, p.base, p.source)
		if plan := strings.Join(insightsRows(t, f.conn, "EXPLAIN QUERY PLAN "+q, args), "\n"); !strings.Contains(plan, insightsAssistantCallsIndex) {
			t.Fatalf("%s source=%v did not use the index:\n%s", p.base, p.source, plan)
		}
		pinned := insightsRows(t, f.conn, q, args)
		unpinned := insightsRows(t, f.conn, strings.ReplaceAll(q, " INDEXED BY "+insightsAssistantCallsIndex, ""), args)
		if !reflect.DeepEqual(pinned, unpinned) {
			t.Fatalf("%s rows differ", p.base)
		}
	}
}

func insightsToolNames(t *testing.T, e *InsightsEngine) []string {
	t.Helper()
	tools, err := e.getToolUsage(context.Background(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, insightsCellStr(tool.name))
	}
	sort.Strings(names)
	return names
}

func TestInsightsMissingIndexFallsBackToUnpinnedQueries(t *testing.T) {
	f := populatedInsights(t)
	before := insightsToolNames(t, f.engine(t))
	f.exec(t, "DROP INDEX IF EXISTS "+insightsAssistantCallsIndex)
	e := f.engine(t)
	if e.hasAssistantCallsIndex || strings.Contains(e.queries["tool_calls"][0], "INDEXED BY") {
		t.Fatal("engine kept the pinned query")
	}
	if after := insightsToolNames(t, e); !reflect.DeepEqual(before, after) {
		t.Fatalf("before %v after %v", before, after)
	}
	if !strings.Contains(insightsBaseQueries()["tool_calls"][0], "INDEXED BY") {
		t.Fatal("base queries lost the pin")
	}
}

func TestInsightsUsageBreakdownMatchesGenerateAndFilters(t *testing.T) {
	f := populatedInsights(t)
	e, full := f.report(t, 30, "")
	focused, err := e.GetUsageBreakdown(context.Background(), 30, "")
	if err != nil || !reflect.DeepEqual(focused.Skills, full.Skills) || !reflect.DeepEqual(focused.Tools, full.Tools) {
		t.Fatalf("focused %+v err %v", focused, err)
	}
	cli, err := e.GetUsageBreakdown(context.Background(), 30, "cli")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cli.Skills.TopSkills {
		names = append(names, s.Skill)
	}
	if !strings.Contains(strings.Join(names, ","), "github-pr-workflow") || strings.Contains(strings.Join(names, ","), "github-code-review") {
		t.Fatalf("cli skills %v", names)
	}
}

func TestInsightsSkillPrefilterIgnoresNonSkillSubstring(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "sx", "cli", "gpt-4o")
	f.msg(t, "sx", "assistant", "Just using read_file.", "", insightsCall("read_file", `{"path":"/tmp/x"}`))
	b, err := f.engine(t).GetUsageBreakdown(context.Background(), 30, "")
	if err != nil || b.Skills.TotalSkillActions != 0 || len(b.Skills.TopSkills) != 0 {
		t.Fatalf("breakdown %+v err %v", b, err)
	}
}

func TestInsightsFormatting(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "s1", "cli", "my-custom-model")
	f.tokens(t, "s1", insightsUsage{InputTokens: 1000, OutputTokens: 500})
	e, r := f.report(t, 30, "")
	if !strings.Contains(e.FormatTerminal(r), "Unknown") {
		t.Fatal("unknown bucket missing")
	}
	e, r = populatedInsights(t).report(t, 30, "")
	term, gw := e.FormatTerminal(r), e.FormatGateway(r)
	if len(gw) >= len(term) {
		t.Fatalf("gateway text not shorter\n%s\n%s", gw, term)
	}
	for _, want := range []string{"Icebath Insights", "Last 30 days", "Models Used", "Platforms", "Top Tools", "Top Skills", "Notable Sessions", "Peak hours"} {
		if !strings.Contains(term, want) {
			t.Fatalf("terminal lacks %q\n%s", want, term)
		}
	}
}

func TestInsightsSessionWithNoModel(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "s1", "cli", "")
	f.tokens(t, "s1", insightsUsage{InputTokens: 1000, OutputTokens: 500})
	_, r := f.report(t, 30, "")
	if r.Empty || len(r.Models) != 1 || r.Models[0].Model != "unknown" || r.Models[0].HasPricing {
		t.Fatalf("models %+v", r.Models)
	}
}

func TestInsightsMixedCommercialAndCustomModels(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "s1", "cli", "anthropic/claude-sonnet-4-20250514")
	f.tokens(t, "s1", insightsUsage{InputTokens: 10000, OutputTokens: 5000, BillingProvider: "anthropic", Model: "anthropic/claude-sonnet-4-20250514"})
	f.session(t, "s2", "cli", "my-local-llama")
	f.tokens(t, "s2", insightsUsage{InputTokens: 10000, OutputTokens: 5000, Model: "my-local-llama"})
	_, r := f.report(t, 30, "")
	o := r.Overview
	if o.EstimatedCost <= 0 || !strings.Contains(strings.Join(o.ModelsWithPricing, ","), "claude-sonnet-4-20250514") ||
		!strings.Contains(strings.Join(o.ModelsWithoutPricing, ","), "my-local-llama") {
		t.Fatalf("overview %+v", o)
	}
	m := insightsModels(r)
	if c := m["claude-sonnet-4-20250514"]; !c.HasPricing || c.Cost <= 0 {
		t.Fatalf("claude %+v", c)
	}
	if l := m["my-local-llama"]; l.HasPricing || l.Cost != 0 {
		t.Fatalf("llama %+v", l)
	}
}

func TestInsightsOnlyOnePlatform(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "s1", "cli", "test")
	e, r := f.report(t, 30, "")
	if len(r.Platforms) != 1 || r.Platforms[0].Platform != "cli" || strings.Contains(e.FormatTerminal(r), "Platforms") {
		t.Fatalf("platforms %+v", r.Platforms)
	}
}

func TestInsightsCostBuckets(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "est", "cli", "model-a")
	f.tokens(t, "est", insightsUsage{InputTokens: 100, Model: "model-a", BillingProvider: "custom", EstimatedCostUSD: 1.50, ActualCostUSD: 1.0,
		HasActualCost: true, CostStatus: "estimated", CostSource: "provider", APICalls: 1})
	f.session(t, "inc", "cli", "gpt-5.4-mini")
	f.tokens(t, "inc", insightsUsage{InputTokens: 200, Model: "gpt-5.4-mini", BillingProvider: "openai-codex", HasActualCost: true,
		CostStatus: "included", CostSource: "none", APICalls: 1})
	e, r := f.report(t, 30, "")
	text := strings.ToLower(e.FormatTerminal(r))
	if !strings.Contains(text, "~$1.50") || !strings.Contains(text, "included") || !strings.Contains(text, "subscription") {
		t.Fatalf("terminal\n%s", text)
	}

	f = newInsightsFixture(t)
	f.session(t, "est", "cli", "model-a")
	f.tokens(t, "est", insightsUsage{InputTokens: 100, Model: "model-a", BillingProvider: "custom", EstimatedCostUSD: 0.0046,
		HasActualCost: true, CostStatus: "estimated", CostSource: "provider", APICalls: 1})
	e, r = f.report(t, 30, "")
	term, gw := e.FormatTerminal(r), e.FormatGateway(r)
	if strings.Contains(term, "~$0.00\n") || !strings.Contains(term, "~$0.0046") || strings.Contains(gw, "~$0.00 estimated") || !strings.Contains(gw, "~$0.0046 estimated") {
		t.Fatalf("terminal\n%s\ngateway\n%s", term, gw)
	}
}

func (f *insightsFixture) label(t *testing.T, id, source, createdSource string) {
	if len(insightsRows(t, f.conn, "SELECT name FROM pragma_table_info('sessions') WHERE name = 'created_source'", nil)) == 0 {
		f.exec(t, "ALTER TABLE sessions ADD COLUMN created_source TEXT")
	}
	f.session(t, id, source, "model-a")
	f.msg(t, id, "user", "hi", "")
	f.exec(t, "UPDATE sessions SET created_source = ? WHERE id = ?", createdSource, id)
}

func TestInsightsSourceFilterIncludesLocalRowsThatSurfaceCreated(t *testing.T) {
	f := newInsightsFixture(t)
	f.label(t, "legacy-cli", "cli", "cli")
	f.label(t, "local-cli", "local", "cli")
	f.label(t, "local-tui", "local", "tui")
	f.label(t, "tg", "telegram", "telegram")
	for source, want := range map[string]int{"cli": 2, "tui": 1, "telegram": 1} {
		if _, r := f.report(t, 30, source); r.Overview.TotalSessions != want {
			t.Fatalf("%s: %d sessions, want %d", source, r.Overview.TotalSessions, want)
		}
	}
}

func TestInsightsSourceFilterWithoutCreatedSource(t *testing.T) {
	f := newInsightsFixture(t)
	f.label(t, "legacy-cli", "cli", "cli")
	f.exec(t, "DROP INDEX IF EXISTS idx_sessions_local_created_started")
	f.exec(t, "ALTER TABLE sessions DROP COLUMN created_source")
	if _, r := f.report(t, 30, "cli"); r.Overview.TotalSessions != 1 {
		t.Fatalf("sessions %d", r.Overview.TotalSessions)
	}
}

func TestInsightsSurvivesCorruptTimestampRows(t *testing.T) {
	f := newInsightsFixture(t)
	for _, sid := range []string{"good", "bad-text", "bad-huge"} {
		f.session(t, sid, "cli", "")
		f.msg(t, sid, "user", "hello from "+sid, "")
	}
	f.exec(t, "UPDATE sessions SET started_at='not-a-timestamp', last_activity_at='not-a-timestamp' WHERE id='bad-text'")
	f.exec(t, "UPDATE messages SET timestamp='not-a-timestamp' WHERE session_id='bad-text'")
	f.exec(t, "UPDATE sessions SET started_at=8.4e252 WHERE id='bad-huge'")
	f.exec(t, "UPDATE messages SET timestamp=1e30 WHERE session_id='bad-huge'")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	e, r := f.report(t, 365_000, "")
	if r.Overview.TotalSessions != 3 || !strings.Contains(buf.String(), "bad-huge") {
		t.Fatalf("sessions %d log %q", r.Overview.TotalSessions, buf.String())
	}
	if !strings.Contains(e.FormatTerminal(r), "Notable Sessions") {
		t.Fatal("terminal report missing")
	}
}

func TestInsightsOverviewTotalsIncludeAuxUsage(t *testing.T) {
	f := newInsightsFixture(t)
	f.session(t, "s1", "cli", "")
	f.tokens(t, "s1", insightsUsage{InputTokens: 1000, OutputTokens: 100, Model: "main-model", BillingProvider: "nous", APICalls: 1})
	f.tokens(t, "s1", insightsUsage{Task: "compression", Model: "glm-5", BillingProvider: "openrouter", InputTokens: 5000, OutputTokens: 500})
	_, r := f.report(t, 30, "")
	if r.Overview.TotalInputTokens != 6000 || r.Overview.TotalOutputTokens != 600 {
		t.Fatalf("overview %+v", r.Overview)
	}
	m := insightsModels(r)
	if _, ok := m["main-model"]; !ok {
		t.Fatalf("models %+v", r.Models)
	}
	if _, ok := m["glm-5"]; !ok {
		t.Fatalf("models %+v", r.Models)
	}
}

// Keyword arguments of one update_token_counts call, zero values standing for the omitted keywords
type insightsUsage struct {
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, ReasoningTokens int64
	EstimatedCostUSD, ActualCostUSD                                               float64
	HasActualCost                                                                 bool
	CostStatus, CostSource, PricingVersion, Model, BillingProvider                string
	BillingBaseURL, BillingMode, Task                                             string
	APICalls                                                                      int
}

// Delta passing only the keywords a test set
func (u insightsUsage) delta() state.TokenCountDelta {
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	d := state.TokenCountDelta{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens, Model: str(u.Model),
		CostStatus: str(u.CostStatus), CostSource: str(u.CostSource), PricingVersion: str(u.PricingVersion),
		BillingProvider: str(u.BillingProvider), BillingBaseURL: str(u.BillingBaseURL), BillingMode: str(u.BillingMode),
		APICallCount: u.APICalls, Task: u.Task}
	if u.EstimatedCostUSD != 0 {
		est := u.EstimatedCostUSD
		d.EstimatedCostUSD = &est
	}
	if u.HasActualCost {
		act := u.ActualCostUSD
		d.ActualCostUSD = &act
	}
	return d
}
