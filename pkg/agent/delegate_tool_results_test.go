package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	delegatedSecret = "sk-ant-" + strings.Repeat("syntheticdelegationcredential", 3)
	delegatedPEM    = "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("c3ludGhldGlj", 100) + "\n-----END PRIVATE KEY-----"
)

// Parent whose compressor reports a window and output reserve and whose last call used the given prompt size
func budgetParent(contextLength, usedTokens, maxTokens int, sessionTotal int) *AIAgent {
	cc := &ContextCompressor{resolvedContextLength: &contextLength, maxTokens: &maxTokens}
	return &AIAgent{contextCompressor: cc, lastTurnUsage: map[string]any{"prompt_tokens": usedTokens}, sessionPromptTokens: sessionTotal}
}

// Isolated home with an optional delegation summary ceiling and the redaction opt out
func delegationTestEnv(t *testing.T, maxSummaryChars int, optOut bool) *tools.Env {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	tools.ResetTerminalScopeCache()
	t.Cleanup(tools.ResetTerminalScopeCache)
	if maxSummaryChars > 0 {
		cfg := fmt.Sprintf("delegation:\n  max_summary_chars: %d\n", maxSummaryChars)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := &tools.Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}}
	if optOut {
		if err := env.Secrets.Set("ICEBATH_REDACT_SECRETS", "false"); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

type delegationRecorder struct {
	memory.BaseProvider
	mu      sync.Mutex
	results []string
}

func (r *delegationRecorder) Name() string                   { return "builtin" }
func (r *delegationRecorder) Available(context.Context) bool { return true }
func (r *delegationRecorder) Initialize(context.Context, string, memory.InitOptions) error {
	return nil
}
func (r *delegationRecorder) ToolSchemas(context.Context) ([]map[string]any, error) { return nil, nil }
func (r *delegationRecorder) OnDelegation(_ context.Context, _, result string, _ memory.DelegationOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
	return nil
}

// Parent of the redaction tests with a recording memory provider
func redactionParent(t *testing.T, env *tools.Env) (*AIAgent, *delegationRecorder) {
	t.Helper()
	rec := &delegationRecorder{}
	parent := &AIAgent{sessionID: "parent-session", env: env, memoryManager: memory.NewMemoryManager(context.Background(), nil, nil)}
	if err := parent.memoryManager.AddProvider(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return parent, rec
}

// Captures every subagent_stop hook invocation on the active plugin manager
func captureSubagentStop(t *testing.T) func() []map[string]any {
	t.Helper()
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	ctx := context.Background()
	m := plugins.GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, false)
	var mu sync.Mutex
	var calls []map[string]any
	c := plugins.NewContext(&plugins.Manifest{Name: "stop-probe", Key: "stop-probe"}, m)
	c.RegisterHook("subagent_stop", func(_ context.Context, kw map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, kw)
		return nil, nil
	}, false)
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

type cannedConversation struct {
	responses []string
	calls     []any
}

// Child agent double whose conversation pops one canned final response per run
func cannedDelegateChild(t *testing.T, env *tools.Env, responses ...string) (*AIAgent, *cannedConversation) {
	t.Helper()
	canned := &cannedConversation{responses: responses}
	child := &AIAgent{model: "test-model", env: env, delegateDepth: 1, platform: "subagent", quietMode: true, suppressStatusOutput: true}
	scripted := map[*AIAgent]*cannedConversation{child: canned}
	prev := conversationLoopRun
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		s, ok := scripted[a]
		if !ok {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		s.calls = append(s.calls, userMessage)
		text := s.responses[0]
		s.responses = s.responses[1:]
		return map[string]any{"final_response": text, "completed": true, "api_calls": 1, "messages": []any{}}, nil
	}
	t.Cleanup(func() { conversationLoopRun = prev })
	return child, canned
}

// Spill files the delegation cache of a home holds
func delegationSpills(t *testing.T, h string) []string {
	t.Helper()
	spills, err := filepath.Glob(filepath.Join(h, "cache", "delegation", "subagent-summary-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return spills
}

// Text of a file decoded the way utf-8-sig reads it
func readUTF8Sig(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// Text of an entry field, a parent copy read as the parent sees it
func entryText(e *pyjson.Object, key string) string {
	v, _ := e.Get(key)
	return pyrepr.Str(v)
}

// test_delegate_summary_budget.py test_small_summaries_pass_through_untouched
func TestSmallSummariesPassThroughUntouched(t *testing.T) {
	delegationTestEnv(t, 0, false)
	parent := budgetParent(200_000, 10_000, 8_000, 10_000)
	results := []*pyjson.Object{
		pyjson.ObjectOf("task_index", 0, "summary", "short result A", "status", "completed"),
		pyjson.ObjectOf("task_index", 1, "summary", "short result B", "status", "completed"),
	}
	applySummaryBudget(context.Background(), results, parent)
	if entryText(results[0], "summary") != "short result A" {
		t.Fatalf("summary changed %v", results[0])
	}
	for _, r := range results {
		if _, ok := r.Get("summary_truncated"); ok {
			t.Fatalf("small summary marked truncated %v", r)
		}
	}
}

// test_delegate_summary_budget.py test_batch_overflow_trimmed_and_spilled_losslessly
func TestBatchOverflowTrimmedAndSpilledLosslessly(t *testing.T) {
	delegationTestEnv(t, 0, false)
	big := "HEAD_MARKER\n" + strings.Repeat("X", 50_000) + "\nTAIL_MARKER"
	parent := budgetParent(131_000, 120_000, 8_000, 120_000)
	results := make([]*pyjson.Object, 5)
	for i := range results {
		results[i] = pyjson.ObjectOf("task_index", i, "summary", big, "status", "completed")
	}
	applySummaryBudget(context.Background(), results, parent)
	for _, r := range results {
		summary := entryText(r, "summary")
		if v, _ := r.Get("summary_truncated"); v != true {
			t.Fatalf("not truncated %v", r.Keys)
		}
		if pystr.Len(summary) >= pystr.Len(big) || !strings.Contains(summary, "HEAD_MARKER") || !strings.Contains(summary, "TAIL_MARKER") {
			t.Fatalf("head and tail window %q", summary)
		}
		path := entryText(r, "summary_full_path")
		if path == "" || readUTF8Sig(t, path) != big {
			t.Fatalf("spill at %q is not the full text", path)
		}
		if !strings.Contains(summary, "read_file") || !strings.Contains(summary, "offset=") {
			t.Fatalf("footer %q", summary)
		}
		if !strings.Contains(path, filepath.Join("cache", "delegation")) {
			t.Fatalf("spill outside the delegation cache %q", path)
		}
	}
}

// test_delegate_summary_budget.py test_budget_uses_current_prompt_size_not_the_session_sum
func TestBudgetUsesCurrentPromptSizeNotTheSessionSum(t *testing.T) {
	longLived := budgetParent(200_000, 30_000, 8_000, 25_000_000)
	fresh := budgetParent(200_000, 30_000, 8_000, 30_000)
	a, okA := parentSummaryCharBudget(longLived, 1)
	b, okB := parentSummaryCharBudget(fresh, 1)
	if !okA || !okB || a != b || a <= minSummaryChars {
		t.Fatalf("budgets %d %v %d %v", a, okA, b, okB)
	}
}

// test_delegate_summary_budget.py test_unknown_parent_usage_means_static_ceiling_not_zero_context
func TestUnknownParentUsageMeansStaticCeilingNotZeroContext(t *testing.T) {
	contextLength, maxTokens := 200_000, 0
	parent := &AIAgent{contextCompressor: &ContextCompressor{resolvedContextLength: &contextLength, maxTokens: &maxTokens}}
	if budget, ok := parentSummaryCharBudget(parent, 1); ok {
		t.Fatalf("budget %d for a parent with no usage", budget)
	}
}

// test_delegate_summary_budget.py test_moa_fold_does_not_inflate_the_parents_prompt_size
func TestMoaFoldDoesNotInflateTheParentsPromptSize(t *testing.T) {
	folded := budgetParent(200_000, 190_000, 0, 0)
	folded.lastPromptSizeTokens = 50_000
	unfolded := budgetParent(200_000, 50_000, 0, 0)
	a, okA := parentSummaryCharBudget(folded, 1)
	b, okB := parentSummaryCharBudget(unfolded, 1)
	if !okA || !okB || a != b {
		t.Fatalf("folded %d %v unfolded %d %v", a, okA, b, okB)
	}
}

var delegationFooterPathRe = regexp.MustCompile(`read_file path="([^"]+)"`)

// test_truncation_footer_sandbox_paths.py test_truncation_footers_render_the_sandbox_visible_cache_path
func TestTruncationFootersRenderTheSandboxVisibleCachePath(t *testing.T) {
	t.Setenv("TERMINAL_ENV", "docker")
	env := delegationTestEnv(t, 0, false)
	rows := make([]string, 5000)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	body := strings.Join(rows, "\n")
	webOut, _ := tools.TruncateWithFooter(env, body, "https://example.com/doc", 3000)
	delegOut, _ := trimSummaryWithFooter(context.Background(), env, body, 3000, 0)
	for _, tc := range []struct{ text, subdir string }{{webOut, "cache/web"}, {pyrepr.Str(delegOut), "cache/delegation"}} {
		m := delegationFooterPathRe.FindStringSubmatch(tc.text)
		if m == nil || !strings.HasPrefix(m[1], tools.DefaultContainerBase+"/"+tc.subdir+"/") {
			t.Fatalf("footer path %v for %s", m, tc.subdir)
		}
		if _, err := os.Stat(filepath.Join(env.Home, tc.subdir, filepath.Base(m[1]))); err != nil {
			t.Fatalf("host copy missing %v", err)
		}
	}
}

// test_delegated_output_redaction.py test_child_result_events_memory_hooks_and_spill_use_redacted_output
func TestChildResultEventsMemoryHooksAndSpillUseRedactedOutput(t *testing.T) {
	for name, text := range map[string]string{"secret": delegatedSecret, "pem": delegatedPEM} {
		t.Run(name, func(t *testing.T) {
			env := delegationTestEnv(t, 2000, false)
			stops := captureSubagentStop(t)
			summary := text + "\n" + strings.Repeat("ordinary explanation\n", 180)
			child, _ := cannedDelegateChild(t, env, summary)
			var mu sync.Mutex
			var completions []any
			child.toolProgressCallback = func(event, _ string, preview any, _ any, _ map[string]any) {
				mu.Lock()
				defer mu.Unlock()
				if event == "subagent.complete" {
					completions = append(completions, preview)
				}
			}
			parent, rec := redactionParent(t, env)

			entry := runChildLifecycle(context.Background(), 0, "summarize the task", child, parent)

			if entryText(entry, "status") != "completed" || strings.Contains(entryText(entry, "summary"), text) {
				t.Fatalf("entry %v", entry)
			}
			if len(completions) == 0 || completions[0] != pystr.Slice(tools.RedactForEgress(env, summary), 0, 160) {
				t.Fatalf("completion previews %v", completions)
			}
			if len(rec.results) == 0 || strings.Contains(rec.results[len(rec.results)-1], text) {
				t.Fatalf("memory results %v", rec.results)
			}
			var stop map[string]any
			for _, kw := range stops() {
				stop = kw
				break
			}
			if stop == nil || strings.Contains(pyrepr.Str(stop["child_summary"]), text) {
				t.Fatalf("subagent_stop %v", stop)
			}
			spills := delegationSpills(t, env.Home)
			if len(spills) == 0 || strings.Contains(readUTF8Sig(t, spills[0]), text) {
				t.Fatalf("spills %v", spills)
			}
		})
	}
}

// test_delegated_output_redaction.py test_redacted_schema_verdict_describes_delivered_summary_without_rerunning_child
func TestRedactedSchemaVerdictDescribesDeliveredSummaryWithoutRerunningChild(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	answer, _ := pyjson.Dumps(map[string]any{"value": delegatedSecret})
	child, canned := cannedDelegateChild(t, env, answer)
	child.delegateOutputSchema = map[string]any{
		"type": "object", "properties": map[string]any{"value": map[string]any{"const": delegatedSecret}}, "required": []any{"value"},
	}
	parent, _ := redactionParent(t, env)

	entry := runSingleChild(context.Background(), 0, "produce structured output", child, parent, "", nil, nil)

	text, _ := pyjson.Dumps(entry)
	if v, _ := entry.Get("schema_valid"); entryText(entry, "status") != "completed" || v != false || strings.Contains(text, delegatedSecret) {
		t.Fatalf("entry %s", text)
	}
	if len(canned.calls) != 1 {
		t.Fatalf("child ran %d times", len(canned.calls))
	}
}

// test_delegated_output_redaction.py test_structured_scalar_summary_stays_valid_json
func TestStructuredScalarSummaryStaysValidJSON(t *testing.T) {
	cases := []struct {
		answer any
		schema map[string]any
	}{
		{"OPENAI_API_KEY=opaqueSyntheticValueXYZ1234", map[string]any{"type": "string"}},
		{"ordinary scalar answer", map[string]any{"type": "string"}},
		{nil, map[string]any{"type": "null"}},
		{true, map[string]any{"type": "boolean"}},
		{17, map[string]any{"type": "integer"}},
		{1.5, map[string]any{"type": "number"}},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			env := delegationTestEnv(t, 0, false)
			summary, _ := pyjson.Dumps(tc.answer)
			child, canned := cannedDelegateChild(t, env, summary)
			child.delegateOutputSchema = tc.schema
			parent, _ := redactionParent(t, env)

			entry := runSingleChild(context.Background(), 0, "produce structured output", child, parent, "", nil, nil)

			delivered, err := pyjson.Loads(entryText(entry, "summary"))
			if v, _ := entry.Get("schema_valid"); err != nil || v != true || len(canned.calls) != 1 {
				t.Fatalf("entry %v delivered %v %v", entry, delivered, err)
			}
			if s, ok := tc.answer.(string); ok && strings.HasPrefix(s, "OPENAI_API_KEY=") {
				d, _ := delivered.(string)
				if strings.Contains(d, "opaqueSyntheticValueXYZ1234") || !strings.HasPrefix(d, "OPENAI_API_KEY=") {
					t.Fatalf("delivered %q", d)
				}
			} else if entryText(entry, "summary") != summary {
				t.Fatalf("summary %q want %q", entryText(entry, "summary"), summary)
			}
		})
	}
}

// Durable dispatch record of test_delegated_output_redaction.py
func delegationRecord(t *testing.T, env *tools.Env) map[string]any {
	t.Helper()
	record := map[string]any{
		"delegation_id": "synthetic-delegation", "dispatched_at": float64(time.Now().UnixNano()) / 1e9, "session_key": "agent:example:dm:route",
		"origin_session_id": "origin-session", "parent_session_id": "parent-session", "goal": "retain the parent goal",
		"context": "retain the parent context", "model": "test-model",
	}
	if err := tools.PersistDispatch(env, record); err != nil {
		t.Fatal(err)
	}
	return record
}

// Event and result JSON of the only durable delegation row
func durableDelegationRow(t *testing.T, env *tools.Env) string {
	t.Helper()
	var eventJSON, resultJSON string
	err := tools.AsyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT event_json,result_json FROM async_delegations").Scan(&eventJSON, &resultJSON)
	})
	if err != nil {
		t.Fatal(err)
	}
	return eventJSON + resultJSON
}

// Drains the shared completion queue and returns its last event
func lastCompletionEvent() map[string]any {
	var last map[string]any
	for {
		evt, ok := tools.DefaultProcessRegistry().CompletionQueue.GetNowait()
		if !ok {
			return last
		}
		last = evt
	}
}

// test_delegated_output_redaction.py test_parent_copy_honours_opt_out_while_persisted_copies_stay_forced
func TestParentCopyHonoursOptOutWhilePersistedCopiesStayForced(t *testing.T) {
	env := delegationTestEnv(t, 2000, true)
	stops := captureSubagentStop(t)
	summary := delegatedSecret + "\n" + strings.Repeat("ordinary explanation\n", 180)
	child, _ := cannedDelegateChild(t, env, summary)
	parent, rec := redactionParent(t, env)

	entry := runChildLifecycle(context.Background(), 0, "summarize", child, parent)

	if !strings.Contains(entryText(entry, "summary"), delegatedSecret) {
		t.Fatalf("opt out parent copy lost the text %q", entryText(entry, "summary"))
	}
	if len(rec.results) == 0 || strings.Contains(rec.results[len(rec.results)-1], delegatedSecret) {
		t.Fatalf("memory results %v", rec.results)
	}
	calls := stops()
	if len(calls) == 0 || strings.Contains(pyrepr.Str(calls[0]["child_summary"]), delegatedSecret) {
		t.Fatalf("subagent_stop %v", calls)
	}
	spills := delegationSpills(t, env.Home)
	if len(spills) == 0 || strings.Contains(readUTF8Sig(t, spills[0]), delegatedSecret) {
		t.Fatalf("spills %v", spills)
	}

	lastCompletionEvent()
	record := delegationRecord(t, env)
	tools.PushCompletionEvent(env, record, map[string]any{"status": "completed", "summary": delegatedSecret}, "completed")
	if evt := lastCompletionEvent(); evt == nil || !strings.Contains(pyrepr.Str(evt["summary"]), delegatedSecret) {
		t.Fatalf("in process event %v", evt)
	}
	if strings.Contains(durableDelegationRow(t, env), delegatedSecret) {
		t.Fatal("durable row kept the secret")
	}
}

// test_delegated_output_redaction.py test_forced_copies_are_projected_from_complete_output_before_the_parent_budget
func TestForcedCopiesAreProjectedFromCompleteOutputBeforeTheParentBudget(t *testing.T) {
	env := delegationTestEnv(t, 2000, true)
	lines := make([]string, 4)
	for i := range lines {
		lines[i] = "MC4CAQAwBQYDK2VwBCIEI" + strings.Repeat(string(rune(65+i)), 43)
	}
	body := strings.Join(lines, "\n")
	pem := "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----\n"
	cut := strings.Index(pem, "KEY-----\n")
	tail := pem[cut:] + strings.Repeat("ordinary ending\n", 10)
	tail += strings.Repeat("x", 500-len(tail))
	structured, _ := pyjson.Dumps(pyjson.ObjectOf("env", "OPENAI_API_KEY="+delegatedSecret, "pad", strings.Repeat("z", 3000), "n", 2))
	summaries := []string{strings.Repeat("ordinary explanation\n", 200) + pem[:cut] + tail, structured}
	parent, rec := redactionParent(t, env)
	entries := make([]*pyjson.Object, len(summaries))
	for i, s := range summaries {
		entries[i] = pyjson.ObjectOf("task_index", i, "status", "completed", "summary", s)
	}
	entries[1].Set("schema_valid", true)
	schemaChild := &AIAgent{delegateOutputSchema: map[string]any{"type": "object"}}

	finalizeChildResults(context.Background(), entries, []map[string]any{{"goal": "a"}, {"goal": "b"}},
		[]delegateChild{{Index: 0, Task: map[string]any{}}, {Index: 1, Task: map[string]any{}, Child: schemaChild}}, parent)

	for _, e := range entries {
		if v, _ := e.Get("summary_truncated"); v != true {
			t.Fatalf("not truncated %v", e.Keys)
		}
	}
	if !strings.Contains(entryText(entries[0], "summary"), body) {
		t.Fatal("opt out parent copy lost the key body")
	}
	if len(rec.results) == 0 || strings.Contains(rec.results[0], body[len(body)-43:]) {
		t.Fatalf("memory copy %v", rec.results)
	}
	spilled, err := pyjson.Loads(readUTF8Sig(t, entryText(entries[1], "summary_full_path")))
	if err != nil {
		t.Fatalf("spill is not valid JSON %v", err)
	}
	obj := spilled.(*pyjson.Object)
	if n, _ := obj.Get("n"); !pyrepr.Equal(n, 2) || strings.Contains(entryText(obj, "env"), delegatedSecret) {
		t.Fatalf("spilled %v", obj)
	}

	lastCompletionEvent()
	record := delegationRecord(t, env)
	record["is_batch"] = true
	tools.PushCompletionEvent(env, record, map[string]any{"results": []any{entries[0], entries[1]}}, "completed")
	evt := lastCompletionEvent()
	results, _ := evt["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("batch event %v", evt)
	}
	first, _ := pyjson.AsObject(results[0])
	if first == nil || !strings.Contains(entryText(first, "summary"), body) {
		t.Fatalf("in process batch event %v", evt)
	}
	if row := durableDelegationRow(t, env); strings.Contains(row, body[len(body)-43:]) || strings.Contains(row, delegatedSecret) {
		t.Fatal("durable row kept the key body or the secret")
	}
}

// Tool content flattening, error detection, output tail and argument summaries of a child conversation
func TestDelegateToolTraceHelpers(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	blocks := []any{map[string]any{"type": "text", "text": "Error: boom"}, map[string]any{"type": "image", "n": 1}, 7}
	if got := stringifyToolContent(blocks); got != "Error: boom\n{\"n\": 1, \"type\": \"image\"}\n7" {
		t.Fatalf("flattened %q", got)
	}
	for content, want := range map[string]bool{
		`{"error": "x"}`: true, `{"status": " Failed "}`: true, `{"status": "ok"}`: false, "Traceback (most recent call last)": true,
		"all good\nError: later": false, "": false, "  exception: bad": true, "the error count is zero": false,
	} {
		if looksLikeErrorOutput(content) != want {
			t.Fatalf("error detection of %q", content)
		}
	}
	args := `{"url": "https://user:pw@Example.com:8443/p?q=1#f", "path": "/tmp/x", "content": "raw payload", "urls": ["http://a.test/x?k=v", 3]}`
	summary := summarizeToolArguments(env, args)
	text, _ := pyjson.Dumps(summary)
	want := `{"argument_keys": ["content", "path", "url", "urls"], "targets": {"url": "https://example.com:8443/p", "path": "/tmp/x", "urls": ["http://a.test/x"]}}`
	if text != want {
		t.Fatalf("argument summary %s", text)
	}
	if text, _ := pyjson.Dumps(summarizeToolArguments(env, "not json")); text != `{"argument_keys": [], "targets": {}}` {
		t.Fatalf("malformed arguments %s", text)
	}
	result := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "terminal"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "line one\nline two"},
		map[string]any{"role": "tool", "tool_call_id": "zz", "content": []any{map[string]any{"text": "Error: wrapped"}}},
	}}
	tail, _ := pyjson.Dumps(extractOutputTail(env, result, 8, 600))
	if tail != `[{"tool": "terminal", "preview": "line one\nline two", "is_error": false}, {"tool": "tool", "preview": "Error: wrapped", "is_error": true}]` {
		t.Fatalf("output tail %s", tail)
	}
	if one := extractOutputTail(env, result, 1, 3); len(one) != 1 || entryText(one[0].(*pyjson.Object), "preview") != "Err" {
		t.Fatalf("bounded tail %v", one)
	}
	trace := []any{pyjson.ObjectOf("tool", "terminal", "status", "ERROR", "args_bytes", 12.7, "result_bytes", true, "input_summary", summary)}
	history, err := subagentStopToolCallHistory(env, trace)
	row, _ := pyjson.Dumps(history)
	if err != nil || row != `[{"tool_name": "terminal", "tool_input": {"argument_keys": ["content", "path", "url", "urls"], "targets": {"url": "https://example.com:8443/p", "path": "/tmp/x", "urls": ["http://a.test/x"]}}, "input_bytes": 12, "output_bytes": 0, "status": "error"}]` {
		t.Fatalf("stop history %s %v", row, err)
	}
}

// Child spend folds into the parent's session cost, marking an unset source and status
func TestRollupChildrenCostMarksUnsetSourceAndStatus(t *testing.T) {
	parent := &AIAgent{sessionEstimatedCostUSD: 0.5, sessionCostSource: "none", sessionCostStatus: "exact"}
	rollupChildrenCost(parent, 0.25)
	if parent.sessionEstimatedCostUSD != 0.75 || parent.sessionCostSource != "subagent" || parent.sessionCostStatus != "exact" {
		t.Fatalf("rollup %v %q %q", parent.sessionEstimatedCostUSD, parent.sessionCostSource, parent.sessionCostStatus)
	}
	rollupChildrenCost(parent, 0)
	if parent.sessionEstimatedCostUSD != 0.75 {
		t.Fatal("zero spend changed the cost")
	}
}

// test_delegated_output_redaction.py test_old_durable_results_are_redacted_on_read_replay_and_failure_roster
func TestOldDurableResultsAreRedactedOnReadReplayAndFailureRoster(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	record := delegationRecord(t, env)
	event := map[string]any{}
	for k, v := range record {
		event[k] = v
	}
	event["type"], event["status"], event["summary"], event["error"] = "async_delegation", "failed", delegatedSecret, delegatedSecret
	eventJSON, _ := pyjson.Dumps(event)
	resultJSON, _ := pyjson.Dumps(map[string]any{"status": "failed", "summary": delegatedSecret, "error": delegatedSecret})
	err := tools.AsyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE async_delegations SET state='failed',completed_at=?,event_json=?,result_json=?",
			float64(time.Now().UnixNano())/1e9, eventJSON, resultJSON)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := tools.GetDurableDelegation(env, "synthetic-delegation")
	if err != nil || stored == nil {
		t.Fatalf("stored %v %v", stored, err)
	}
	if text, _ := pyjson.Dumps(stored["result"]); strings.Contains(text, delegatedSecret) {
		t.Fatalf("stored result %s", text)
	}
	lastCompletionEvent()
	target := tools.DefaultProcessRegistry().CompletionQueue
	if n, err := tools.RestoreUndeliveredCompletions(env, target); n != 1 || err != nil {
		t.Fatalf("restored %d %v", n, err)
	}
	replay := lastCompletionEvent()
	if replay["restored"] != true || replay["session_key"] != record["session_key"] {
		t.Fatalf("replay %v", replay)
	}
	if text, _ := pyjson.Dumps(replay); strings.Contains(text, delegatedSecret) {
		t.Fatalf("replay %s", text)
	}
	failures, err := tools.FailedDelegationsForSession(env, "", "parent-session", 20, nil)
	if err != nil || len(failures) != 1 || failures[0]["goal"] != record["goal"] {
		t.Fatalf("failures %v %v", failures, err)
	}
	if text, _ := pyjson.Dumps(failures); strings.Contains(text, delegatedSecret) {
		t.Fatalf("failures %s", text)
	}
}

// test_delegated_output_redaction.py test_structured_summary_is_redacted_per_value_and_stays_valid_json
func TestStructuredSummaryIsRedactedPerValueAndStaysValidJSON(t *testing.T) {
	wraps := map[string]func(string) string{
		"bare":   func(s string) string { return s },
		"fenced": func(s string) string { return "```json\n" + s + "\n```" },
		"prose":  func(s string) string { return "Here is the answer:\n" + s + "\nDone." },
	}
	for name, wrap := range wraps {
		t.Run(name, func(t *testing.T) {
			key := "sk-proj-" + strings.Repeat("a", 40)
			answer, _ := pyjson.Dumps(pyjson.ObjectOf("env", "OPENAI_API_KEY="+key, "n", 2))
			entry := tools.RedactChildResult(nil, map[string]any{"summary": wrap(answer), "schema_valid": true}, map[string]any{"type": "object"}, true).(map[string]any)
			parsed, err := pyjson.Loads(tools.ExtractJSONCandidate(pyrepr.Str(entry["summary"])))
			if err != nil {
				t.Fatalf("summary no longer parses %v", err)
			}
			obj := parsed.(*pyjson.Object)
			if n, _ := obj.Get("n"); !pyrepr.Equal(n, 2) || strings.Contains(entryText(obj, "env"), key) || entry["schema_valid"] != true {
				t.Fatalf("entry %v", entry)
			}
		})
	}
}

// test_truncation_footer_sandbox_paths.py test_local_backend_footer_keeps_the_host_path
func TestLocalBackendFooterKeepsTheHostPath(t *testing.T) {
	t.Setenv("TERMINAL_ENV", "local")
	env := delegationTestEnv(t, 0, false)
	rows := make([]string, 5000)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	out, _ := tools.TruncateWithFooter(env, strings.Join(rows, "\n"), "https://example.com/doc", 3000)
	if m := delegationFooterPathRe.FindStringSubmatch(out); m == nil || !strings.HasPrefix(m[1], env.Home) {
		t.Fatalf("footer path %v", m)
	}
}
