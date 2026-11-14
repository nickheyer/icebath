package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type metricsAgentFake struct {
	provider, model, sessionID string
	tools                      []map[string]any
	memoryOrigin               string
	persistDisabled            bool
	scratch                    sync.Map
}

func (a *metricsAgentFake) DelegateDepth() int                     { return 0 }
func (a *metricsAgentFake) Platform() string                       { return "" }
func (a *metricsAgentFake) BackgroundReview() bool                 { return false }
func (a *metricsAgentFake) ToolSearchScopedNames() map[string]bool { return nil }
func (a *metricsAgentFake) Provider() string                       { return a.provider }
func (a *metricsAgentFake) Model() string                          { return a.model }
func (a *metricsAgentFake) SessionID() string                      { return a.sessionID }
func (a *metricsAgentFake) ToolDefinitions() []map[string]any      { return a.tools }
func (a *metricsAgentFake) MemoryWriteOrigin() string              { return a.memoryOrigin }
func (a *metricsAgentFake) PersistDisabled() bool                  { return a.persistDisabled }
func (a *metricsAgentFake) MetricsScratch() *sync.Map              { return &a.scratch }
func (a *metricsAgentFake) ValidToolNames() map[string]bool {
	out := map[string]bool{}
	for _, tool := range a.tools {
		out[tool["function"].(map[string]any)["name"].(string)] = true
	}
	return out
}

// Agent of session s1 on openrouter with three 400 character tool schemas by default
func effAgent(model string, toolDefs []map[string]any) *metricsAgentFake {
	if toolDefs == nil {
		for _, name := range []string{"read_file", "terminal", "acme_private_tool"} {
			toolDefs = append(toolDefs, map[string]any{"type": "function", "function": map[string]any{
				"name": name, "description": strings.Repeat("d", 400), "parameters": map[string]any{"type": "object"}}})
		}
	}
	return &metricsAgentFake{provider: "openrouter", model: model, sessionID: "s1", tools: toolDefs}
}

func effUsage(total, cacheRead int) any {
	return map[string]any{"input_tokens": total / 2, "output_tokens": total - total/2, "cache_read_tokens": cacheRead, "total_tokens": total}
}

// One user turn whose calls are its primary call usages and toolsRun the tools it ran
func effTurn(ctx context.Context, sessionID, taskID string, result map[string]any, calls []any, toolsRun [][2]string, model string, start map[string]any) {
	base := map[string]any{"session_id": sessionID, "task_id": taskID, "provider": "openrouter", "model": model}
	invokeHook(ctx, "pre_llm_call", base, map[string]any{"platform": "cli"}, start)
	if len(calls) == 0 {
		calls = []any{nil}
	}
	for i, usage := range calls {
		request := mergeEvent(base, map[string]any{"api_request_id": fmt.Sprintf("%s-r%d", taskID, i)})
		invokeHook(ctx, "pre_api_request", request)
		invokeHook(ctx, "post_api_request", request, map[string]any{"usage": usage})
	}
	for i, tr := range toolsRun {
		call := mergeEvent(base, map[string]any{"tool_call_id": fmt.Sprintf("%s-c%d", taskID, i), "tool_name": tr[0], "toolset": tr[1]})
		invokeHook(ctx, "pre_tool_call", call, map[string]any{"args": map[string]any{}})
		invokeHook(ctx, "post_tool_call", call, map[string]any{"args": map[string]any{}, "result": "ok", "status": "ok"})
	}
	if result != nil {
		FinishTaskRun(ctx, sessionID, taskID, "cli", result, nil)
	}
}

func dimsKey(d map[string]any) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, d[k])
	}
	return strings.Join(parts, ",")
}

// Flushes the relay then lists one metric's rows as sorted dimension keys with their values
func effRows(t *testing.T, ctx context.Context, metric string) []string {
	t.Helper()
	if err := getRuntime(ctx, true, nil).relay.Subscribers.Flush(); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range storedValues(t, ctx, metric) {
		out = append(out, fmt.Sprintf("%s|%d", dimsKey(v.dims), v.value))
	}
	sort.Strings(out)
	return out
}

func rowSet(rows ...string) []string {
	sort.Strings(rows)
	return rows
}

const effRoute = "model=model-a,provider=openrouter"

func TestTaskCostIsOneRowPerUserTurnWithItsTokensAndWork(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	terminal := [][2]string{{"terminal", "terminal"}, {"terminal", "terminal"}, {"terminal", "terminal"}}
	effTurn(ctx, "s1", "t1", map[string]any{"completed": true}, []any{effUsage(1500, 0), effUsage(900, 0)}, terminal, "model-a", nil)
	effTurn(ctx, "s1", "t2", map[string]any{"interrupted": true, "turn_exit_reason": "interrupted_by_user"}, []any{effUsage(60_000, 0)}, nil, "model-a", nil)
	effTurn(ctx, "s1", "t3", map[string]any{"failed": true, "turn_exit_reason": "failed"}, []any{nil}, nil, "model-a", nil)
	effTurn(ctx, "s1", "t4", nil, []any{effUsage(10, 0)}, nil, "model-a", nil)
	finalizeSession(ctx, "s1")
	effTurn(ctx, "child", "t1", map[string]any{"completed": true}, []any{effUsage(10, 0)}, nil, "model-a", map[string]any{"parent_session_id": "s1"})
	finalizeSession(ctx, "child")
	want := rowSet(
		"api_calls_bucket=2,model=model-a,outcome=completed,provider=openrouter,tokens_bucket=2k_to_10k,tool_calls_bucket=3_to_5|1",
		"api_calls_bucket=1,model=model-a,outcome=interrupted,provider=openrouter,tokens_bucket=50k_to_200k,tool_calls_bucket=0|1",
		"api_calls_bucket=1,model=model-a,outcome=failed,provider=openrouter,tokens_bucket=unknown,tool_calls_bucket=0|1",
	)
	if got := effRows(t, ctx, TaskCostMetric); !slices.Equal(got, want) {
		t.Fatalf("task cost %v", got)
	}
	if got := effRows(t, ctx, WastedTokensMetric); !slices.Equal(got, []string{effRoute + ",reason=interrupt,tokens_bucket=50k_to_200k|1"}) {
		t.Fatalf("wasted %v", got)
	}
}

func TestUndoAndRetryCountTheTokensOfTheTurnsTheyDiscard(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	done := map[string]any{"completed": true}
	effTurn(ctx, "s1", "t1", done, []any{effUsage(1_000, 0)}, nil, "model-a", nil)
	effTurn(ctx, "s1", "t2", done, []any{effUsage(20_000, 0)}, nil, "model-a", nil)
	effTurn(ctx, "s1", "t3", done, []any{effUsage(300_000, 0)}, nil, "model-a", nil)
	friction := func(signal, sessionID string, turns int) {
		RecordModelFriction(ctx, signal, FrictionOptions{SessionID: sessionID, Provider: "openrouter", Model: "model-a", Turns: turns})
	}
	friction("undo", "s1", 2)
	effTurn(ctx, "s1", "t4", map[string]any{"interrupted": true, "turn_exit_reason": "interrupted_by_user"}, []any{effUsage(5_000, 0)}, nil, "model-a", nil)
	friction("undo", "s1", 0)
	friction("retry", "s1", 0)
	friction("retry", "gone", 0)
	want := rowSet(effRoute+",reason=undo,tokens_bucket=200k_to_1m|1", effRoute+",reason=undo,tokens_bucket=10k_to_50k|1",
		effRoute+",reason=interrupt,tokens_bucket=2k_to_10k|1", effRoute+",reason=retry,tokens_bucket=lt_2k|1",
		effRoute+",reason=retry,tokens_bucket=unknown|1")
	if got := effRows(t, ctx, WastedTokensMetric); !slices.Equal(got, want) {
		t.Fatalf("wasted %v", got)
	}
}

func TestToolOutputTruncationIsJudgedAfterTheTurnBudget(t *testing.T) {
	_, ctx := directRuntime(t)
	agent := effAgent("model-a", nil)
	budget := tools.BudgetConfig{DefaultResultSize: 5_000, TurnBudget: 12_000, PreviewSize: 100, MCPResultSize: tools.DefaultMCPResultSizeChars}
	var cut bytes.Buffer
	enc := json.NewEncoder(&cut)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tools.TruncateHeadTail(strings.Repeat("e", 300_000), 3_000, "OUTPUT")); err != nil {
		t.Fatal(err)
	}
	cutResult := `{"output": ` + strings.TrimSuffix(cut.String(), "\n") + `, "exit_code": 0}`
	results := [][2]string{{"read_file", strings.Repeat("a", 200)}, {"terminal", strings.Repeat("b", 80_000)},
		{"mcp_acme_query", strings.Repeat("c", 9_000)}, {"read_file", strings.Repeat("d", 4_900)}, {"execute_code", cutResult}}
	var messages []*state.Message
	var before []any
	for i, r := range results {
		id := fmt.Sprintf("c%d", i)
		kept := tools.MaybePersistToolResult(ctx, nil, nil, r[1], r[0], id, budget, tools.ThresholdResolve)
		NoteToolResult(ctx, agent, r[0], id, r[1], kept)
		messages = append(messages, &state.Message{Role: "tool", ToolCallID: id, Content: kept})
		before = append(before, kept)
	}
	messages = tools.EnforceTurnBudget(ctx, nil, nil, messages, budget)
	batch := make([]map[string]any, len(messages))
	for i, m := range messages {
		batch[i] = map[string]any{"role": m.Role, "tool_call_id": m.ToolCallID, "content": m.Content}
	}
	RecordToolBatch(ctx, agent, batch, before)
	if pending, ok := agent.scratch.Load(pendingOutputsKey); ok && len(pending.(*pendingOutputs).entries) != 0 {
		t.Fatalf("tool outputs left pending %v", pending)
	}
	rows := effRows(t, ctx, ToolOutputTruncationMetric)
	if strings.Contains(strings.Join(rows, " "), "acme") {
		t.Fatalf("acme leaked %v", rows)
	}
	total := 0
	got := map[string]bool{}
	for _, v := range storedValues(t, ctx, ToolOutputTruncationMetric) {
		got[fmt.Sprintf("%v/%v/%v", v.dims["tool"], v.dims["original_size_bucket"], v.dims["truncated"])] = true
		total += int(v.value)
	}
	for _, want := range []string{"read_file/lt_1k/no", "terminal/50k_to_100k/yes", "mcp/1k_to_10k/yes", "execute_code/100k_to_500k/yes"} {
		if !got[want] {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	if total != 5 {
		t.Fatalf("rows %v", rows)
	}
}

func TestToolOverheadAndUnusedToolsetsReportOncePerConversation(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	toolsets := map[string]string{"read_file": "file", "terminal": "terminal", "acme_private_tool": "acme-private"}
	saved := toolsetForTool
	toolsetForTool = func(_ context.Context, name string) string { return toolsets[name] }
	t.Cleanup(func() { toolsetForTool = saved })
	agent := effAgent("model-a", nil)
	effTurn(ctx, "s1", "t1", nil, []any{effUsage(100, 0)}, nil, "model-a", nil)
	ObserveRequestTools(ctx, agent, agent.tools)
	ObserveRequestTools(ctx, agent, agent.tools)
	effTurn(ctx, "s1", "t1", map[string]any{"completed": true}, nil, [][2]string{{"terminal", "terminal"}}, "model-a", nil)
	finalizeSession(ctx, "s1")
	if got := effRows(t, ctx, ToolOverheadMetric); !slices.Equal(got, []string{"enabled_tool_count_bucket=3_to_5,execution_surface=cli,tool_schema_tokens_bucket=lt_2k|1"}) {
		t.Fatalf("overhead %v", got)
	}
	want := rowSet("toolset=custom,used=no|1", "toolset=file,used=no|1", "toolset=terminal,used=yes|1")
	got := effRows(t, ctx, ToolEnabledUnusedMetric)
	if !slices.Equal(got, want) || strings.Contains(strings.Join(got, " "), "acme") {
		t.Fatalf("unused %v", got)
	}
}

func TestSeveralKnownCausesBeforeOneColdReadAreOneBreak(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	agent := effAgent("model-a", nil)
	effTurn(ctx, "s1", "t1", map[string]any{"completed": true}, []any{effUsage(9_000, 8_000)}, nil, "model-a", nil)
	RecordCacheBreak(ctx, agent, "compression")
	RecordCacheBreak(ctx, agent, "compression")
	RecordCacheBreak(ctx, agent, "model_switch")
	effTurn(ctx, "s1", "t2", map[string]any{"completed": true}, []any{effUsage(9_000, 0)}, nil, "model-a", nil)
	if got := effRows(t, ctx, CacheBreakMetric); !slices.Equal(got, []string{"cause=compression," + effRoute + "|1"}) {
		t.Fatalf("breaks %v", got)
	}
}

func TestCacheBreaksCountKnownCausesOnceAndUnannouncedColdReadsAsMisses(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	agent := effAgent("model-a", nil)
	done := map[string]any{"completed": true}
	effTurn(ctx, "s1", "t1", done, []any{effUsage(9_000, 0), effUsage(9_000, 8_000)}, nil, "model-a", nil)
	RecordCacheBreak(ctx, agent, "compression")
	effTurn(ctx, "s1", "t2", done, []any{effUsage(9_000, 0)}, nil, "model-a", nil)
	effTurn(ctx, "s1", "t3", done, []any{effUsage(9_000, 8_000), effUsage(9_000, 0)}, nil, "model-a", nil)
	effTurn(ctx, "s1", "t4", done, []any{effUsage(9_000, 0)}, nil, "model-b", nil)
	effTurn(ctx, "s1", "t5", done, []any{effUsage(9_000, 8_000)}, nil, "model-a", nil)
	rt := getRuntime(ctx, true, nil)
	session, _ := rt.sessions.Get("s1")
	session.mu.Lock()
	session.efficiency.CacheEndedNS -= int64(CacheTTLSeconds*1e9) + 1
	session.mu.Unlock()
	effTurn(ctx, "s1", "t6", done, []any{effUsage(9_000, 0)}, nil, "model-a", nil)
	saved := toolsetForTool
	toolsetForTool = func(context.Context, string) string { return "" }
	t.Cleanup(func() { toolsetForTool = saved })
	ObserveRequestTools(ctx, agent, agent.tools)
	ObserveRequestTools(ctx, agent, agent.tools[:1])
	review := effAgent("model-a", nil)
	review.memoryOrigin = "background_review"
	RecordCacheBreak(ctx, review, "compression")
	disabled := effAgent("model-a", nil)
	disabled.persistDisabled = true
	RecordCacheBreak(ctx, disabled, "compression")
	want := rowSet("cause=compression,"+effRoute+"|1", "cause=provider_reported_miss,"+effRoute+"|1",
		"cause=cache_expired,"+effRoute+"|1", "cause=toolset_change,"+effRoute+"|1")
	if got := effRows(t, ctx, CacheBreakMetric); !slices.Equal(got, want) {
		t.Fatalf("breaks %v", got)
	}
}

func TestPromptRebuildNamesAModelSwitchApartFromOtherRebuilds(t *testing.T) {
	var causes []string
	saved, savedIdentity := recordCacheBreak, IdentityLineValueHook
	recordCacheBreak = func(_ context.Context, _ MetricsAgent, cause string) { causes = append(causes, cause) }
	IdentityLineValueHook = func(prompt, label string) string {
		trailer := prompt
		if i := strings.LastIndex(prompt, "\n\n"); i >= 0 {
			trailer = prompt[i+2:]
		}
		value := ""
		for _, line := range strings.Split(trailer, "\n") {
			if strings.HasPrefix(line, label+":") {
				value = strings.TrimSpace(line[len(label)+1:])
			}
		}
		return value
	}
	t.Cleanup(func() { recordCacheBreak, IdentityLineValueHook = saved, savedIdentity })
	ctx := context.Background()
	agent := effAgent("model-b", nil)
	stored := "You are Icebath.\nModel: model-a\nProvider: openrouter\n"
	RecordPromptRebuild(ctx, agent, stored, "stale_runtime", strings.Replace(stored, "model-a", "model-b", 1))
	RecordPromptRebuild(ctx, effAgent("model-a", nil), stored, "stale_runtime", stored+"Host: x\n")
	RecordPromptRebuild(ctx, agent, nil, "null", "rebuilt")
	RecordPromptRebuild(ctx, agent, stored, "stale_runtime", stored)
	RecordPromptRebuild(ctx, agent, nil, "missing", "fresh")
	if want := []string{"model_switch", "system_prompt_rebuild", "system_prompt_rebuild"}; !slices.Equal(causes, want) {
		t.Fatalf("causes %v", causes)
	}
}

func TestNothingIsRecordedWhileDisabled(t *testing.T) {
	_, ctx := directRuntime(t)
	writeProfileConfig(t, home.Dir(ctx), "{}\n")
	agent := effAgent("model-a", nil)
	NoteToolResult(ctx, agent, "terminal", "c1", strings.Repeat("x", 10), strings.Repeat("x", 10))
	if pending, ok := agent.scratch.Load(pendingOutputsKey); ok && len(pending.(*pendingOutputs).entries) != 0 {
		t.Fatalf("tool output noted %v", pending)
	}
	RecordCacheBreak(ctx, agent, "compression")
	ObserveRequestTools(ctx, agent, agent.tools)
	if _, err := os.Stat(metricsRoot(ctx)); !os.IsNotExist(err) {
		t.Fatalf("telemetry written %v", err)
	}
}

func TestTurnActivityBucketsReachLongAgenticLoops(t *testing.T) {
	for count, bucket := range map[int]string{0: "0", 2: "2", 5: "3_to_5", 25: "11_to_25", 50: "26_to_50", 100: "51_to_100", 101: "gte_101"} {
		if got := ActivityBucket(count); got != bucket || !TurnActivityBuckets.Has(bucket) {
			t.Fatalf("%d %s", count, got)
		}
	}
}
