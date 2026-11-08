package observability

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

type replyAgentFake struct {
	metricsAgentFake
}

func (a *replyAgentFake) HasContentAfterThinkBlock(text string) bool {
	return strings.TrimSpace(strings.ReplaceAll(text, "<think>x</think>", "")) != ""
}

func (a *replyAgentFake) ExtractReasoning(message map[string]any) string {
	s, _ := message["reasoning"].(string)
	return s
}

// A user turn agent on openrouter with an assistant tool write origin
func harnessAgent(origin string) *replyAgentFake {
	if origin == "" {
		origin = "assistant_tool"
	}
	return &replyAgentFake{metricsAgentFake{provider: "openrouter", model: "anthropic/claude-sonnet", memoryOrigin: origin}}
}

// Tool env bound to the runtime profile with shared metrics on and metric events routed to the sink
func harnessToolEnv(ctx context.Context) *tools.Env {
	env := &tools.Env{Home: home.Dir(ctx), Config: config.Tree{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true}}}}
	env.Notify = func(ev tools.Event) { RecordToolMetricEvent(ctx, env, ev) }
	return env
}

func runTool(t *testing.T, ctx context.Context, env *tools.Env, name string, args map[string]any) string {
	t.Helper()
	def := tools.Default.Get(name)
	if def == nil {
		t.Fatalf("tool %s not registered", name)
	}
	return def.Handler(ctx, &tools.Call{Name: name, Args: args, TaskID: "t", Env: env})
}

func TestPatchAndWriteFileCountOutcomeAndTheStrategyThatLanded(t *testing.T) {
	_, ctx := directRuntime(t)
	savedKeys, savedSecrets := tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv
	t.Cleanup(func() { tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv = savedKeys, savedSecrets })
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return nil }
	env := harnessToolEnv(ctx)
	target := filepath.Join(t.TempDir(), "work", "a.py")
	runTool(t, ctx, env, "write_file", map[string]any{"path": target, "content": "def f():\n    return 1\n\nx = 1\nx = 1\n"})
	runTool(t, ctx, env, "patch", map[string]any{"path": target, "old_string": "return 1", "new_string": "return 2"})
	runTool(t, ctx, env, "patch", map[string]any{"path": target, "old_string": "def f():\n  return 2", "new_string": "def f():\n    return 3"})
	runTool(t, ctx, env, "patch", map[string]any{"path": target, "old_string": "nothing like this", "new_string": "y"})
	runTool(t, ctx, env, "patch", map[string]any{"path": target, "old_string": "x = 1", "new_string": "x = 2"})
	runTool(t, ctx, env, "patch", map[string]any{"mode": "patch", "patch": "*** Begin Patch\n*** Update File: " + target + "\n@@\n-def f():\n+def g():\n*** End Patch\n"})
	body, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(body), "return 3") || !strings.Contains(string(body), "def g") {
		t.Fatalf("%q %v", body, err)
	}
	var fuzzy, rest []string
	for _, row := range effRows(t, ctx, FileEditMetric) {
		if strings.Contains(row, "match_strategy=exact") || strings.Contains(row, "match_strategy=none") {
			rest = append(rest, row)
		} else {
			fuzzy = append(fuzzy, row)
		}
	}
	if len(fuzzy) != 1 || !strings.Contains(fuzzy[0], "mode=replace,outcome=applied,tool=patch|1") {
		t.Fatalf("fuzzy %v", fuzzy)
	}
	want := rowSet("match_strategy=none,mode=replace,outcome=ambiguous,tool=patch|1",
		"match_strategy=exact,mode=replace,outcome=applied,tool=patch|1",
		"match_strategy=none,mode=replace,outcome=no_match,tool=patch|1",
		"match_strategy=exact,mode=v4a,outcome=applied,tool=patch|1",
		"match_strategy=none,mode=whole_file,outcome=applied,tool=write_file|1")
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("%v", rest)
	}
}

func TestMatcherCallsOutsideAnEditToolCallAreNotCounted(t *testing.T) {
	_, ctx := directRuntime(t)
	tools.FuzzyFindAndReplace("a = 1\n", "a = 1", "a = 2", false, nil)
	NoteEditMatch(ctx, "exact", "")
	if rows := effRows(t, ctx, FileEditMetric); len(rows) != 0 {
		t.Fatalf("%v", rows)
	}
}

func TestV4aPatchReportsItsLoosestHunkStrategy(t *testing.T) {
	fields := FileEditFields("patch", "patch", `{"success": true}`, &EditProbe{Strategies: []string{"exact", "line_trimmed", "exact"}})
	if !reflect.DeepEqual(fields, map[string]string{"match_strategy": "line_trimmed", "mode": "v4a", "outcome": "applied", "tool": "patch"}) {
		t.Fatalf("%v", fields)
	}
	if got := FileEditFields("patch", "patch", `{"success": true, "no_change": true}`, &EditProbe{Misses: []string{"no_match"}}); got["outcome"] != "already_applied" {
		t.Fatalf("%v", got)
	}
}

func TestCommandKindComesFromTheFirstProgramWord(t *testing.T) {
	for _, c := range []struct {
		command any
		kind    string
	}{{"git status", "git"}, {"FOO=1 sudo -E pip install x", "package_manager"}, {"/usr/bin/python3.12 -c 1", "python"},
		{"pytest -q tests/", "test_runner"}, {"  cd /tmp && make", "shell_builtin"}, {"npx tsc", "node"},
		{"docker ps", "container"}, {"curl -s http://x", "network"}, {"ls -la", "file_ops"}, {"bash -lc 'x'", "shell"},
		{"cargo build", "build"}, {"my-private-tool --secret", "other"}, {"", "other"}, {nil, "other"}} {
		if got := CommandKind(c.command); got != c.kind || !TerminalCommandKinds.Has(got) {
			t.Errorf("%v: %s want %s", c.command, got, c.kind)
		}
	}
}

func TestTerminalOutcomeTrustsIcebathFlagsNotExit124(t *testing.T) {
	for _, c := range []struct {
		result map[string]any
		want   string
	}{{map[string]any{"returncode": 124}, "nonzero"}, {map[string]any{"returncode": 124, "icebath_timed_out": true}, "timeout"},
		{map[string]any{"returncode": 130, "icebath_interrupted": true}, "killed"}, {map[string]any{"returncode": 137}, "killed"},
		{map[string]any{"returncode": -9}, "killed"}, {map[string]any{"returncode": 0}, "ok"}, {map[string]any{"output": "x"}, ""},
		{map[string]any{"returncode": true}, ""}} {
		if got, _ := TerminalOutcome(c.result); got != c.want {
			t.Errorf("%v: %q want %q", c.result, got, c.want)
		}
	}
}

func TestLoopGuardsCountOncePerTurnPerSignalAndDetector(t *testing.T) {
	_, ctx := directRuntime(t)
	agent := harnessAgent("")
	for range 3 {
		RecordGuardrailDecision(ctx, agent, "warn", "same_tool_failure_warning")
	}
	RecordGuardrailDecision(ctx, agent, "halt", "same_tool_failure_halt")
	RecordGuardrailDecision(ctx, agent, "warn", "identical_call_streak")
	RecordGuardrailDecision(ctx, agent, "block", "loop_web_search_cap")
	RecordGuardrailDecision(ctx, agent, "allow", "allow")
	FinishTurn(ctx, agent, "max_iterations_reached(90/90)", "", false, false)
	ResetHarnessTurn(agent)
	RecordGuardrailDecision(ctx, agent, "warn", "same_tool_failure_warning")
	RecordGuardrailDecision(ctx, harnessAgent("background_review"), "halt", "identical_cycle_halt")
	route := "model=anthropic/claude-sonnet,provider=openrouter,"
	want := rowSet("detector=iteration_budget,"+route+"signal=iteration_cap|1",
		"detector=same_tool_failure,"+route+"signal=loop_detected|1",
		"detector=web_search_cap,"+route+"signal=loop_detected|1",
		"detector=identical_call_streak,"+route+"signal=repeated_tool_call|1",
		"detector=same_tool_failure,"+route+"signal=repeated_tool_call|2")
	if got := effRows(t, ctx, LoopGuardMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestRealGuardrailHaltReachesTheMetric(t *testing.T) {
	_, ctx := directRuntime(t)
	agent := harnessAgent("")
	cfg := tools.DefaultToolCallGuardrailConfig()
	cfg.HardStopEnabled = true
	controller := tools.NewToolCallGuardrailController(&cfg)
	var halt *tools.ToolGuardrailDecision
	for range 8 {
		args := map[string]any{"command": "false"}
		if before := controller.BeforeCall("terminal", args); before.ShouldHalt() {
			halt = &before
			RecordGuardrailDecision(ctx, agent, before.Action, before.Code)
			break
		}
		result, failed := `{"exit_code": 1, "error": "boom"}`, true
		after := controller.AfterCall("terminal", args, &result, &failed)
		RecordGuardrailWarnings(ctx, agent, &after, "")
		if after.ShouldHalt() {
			halt = &after
			RecordGuardrailDecision(ctx, agent, after.Action, after.Code)
			break
		}
	}
	if halt == nil {
		t.Fatal("no halt decision")
	}
	rows := strings.Join(effRows(t, ctx, LoopGuardMetric), "\n")
	if !strings.Contains(rows, "detector="+guardDetectors[halt.Code]+",") || !strings.Contains(rows, "signal=loop_detected") ||
		!strings.Contains(rows, "signal=repeated_tool_call") {
		t.Fatalf("%s", rows)
	}
}

// Observes one round of tool outcomes then resolves it
func harnessRound(ctx context.Context, agent MetricsAgent, outcomes ...toolOutcome) {
	for _, o := range outcomes {
		ObserveToolOutcome(ctx, agent, o.name, o.failed)
	}
	FinishToolRound(ctx, agent)
}

func TestEachFailedCallResolvesAgainstTheModelsNextCall(t *testing.T) {
	_, ctx := directRuntime(t)
	agent := harnessAgent("")
	harnessRound(ctx, agent, toolOutcome{"terminal", true}, toolOutcome{"read_file", false})
	harnessRound(ctx, agent, toolOutcome{"read_file", false}, toolOutcome{"terminal", false})
	harnessRound(ctx, agent, toolOutcome{"patch", true}, toolOutcome{"mcp_acme_secret", true})
	harnessRound(ctx, agent, toolOutcome{"search_files", true})
	FinishTurn(ctx, agent, "text_response(stop)", "done", false, false)
	ResetHarnessTurn(agent)
	harnessRound(ctx, agent, toolOutcome{"web_search", true})
	FinishTurn(ctx, agent, "guardrail_halt", "stopped", false, true)
	route := "model=anthropic/claude-sonnet,"
	want := rowSet(route+"next_outcome=success,next_tool=same,provider=openrouter,tool=terminal|1",
		route+"next_outcome=error,next_tool=different,provider=openrouter,tool=patch|1",
		route+"next_outcome=error,next_tool=different,provider=openrouter,tool=mcp|1",
		route+"next_outcome=no_tool_call,next_tool=none,provider=openrouter,tool=search_files|1",
		route+"next_outcome=gave_up,next_tool=none,provider=openrouter,tool=web_search|1")
	got := effRows(t, ctx, ToolRecoveryMetric)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	if strings.Contains(strings.Join(got, ""), "acme") {
		t.Fatal("mcp server name leaked")
	}
}

func TestEveryPrimaryReplyCountsOnceWithItsIssue(t *testing.T) {
	_, ctx := directRuntime(t)
	agent := harnessAgent("")
	reply := func(finishReason string, message map[string]any) {
		response := new(int)
		RecordReplyFinish(ctx, agent, response, finishReason)
		RecordReplyContent(ctx, agent, response, message)
	}
	reply("stop", map[string]any{"content": "hello"})
	reply("tool_calls", map[string]any{"tool_calls": []any{map[string]any{}}})
	reply("stop", map[string]any{"content": "   "})
	reply("stop", map[string]any{"content": "<think>x</think>", "reasoning": "x"})
	reply("length", map[string]any{"content": "half an ans"})
	RecordReplyFinish(ctx, agent, new(int), "content_filter")
	RecordReplyContent(ctx, harnessAgent("background_review"), new(int), map[string]any{"content": ""})
	route := "model=anthropic/claude-sonnet,provider=openrouter"
	want := rowSet("issue=empty,"+route+"|1", "issue=none,"+route+"|2", "issue=reasoning_only,"+route+"|1",
		"issue=refusal,"+route+"|1", "issue=truncated_length,"+route+"|1")
	if got := effRows(t, ctx, ModelReplyIssueMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestEverythingIsANoOpWhileDisabled(t *testing.T) {
	_, ctx := directRuntime(t)
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := harnessAgent("")
	RecordReplyFinish(ctx, agent, new(int), "length")
	harnessRound(ctx, agent, toolOutcome{"terminal", true})
	RecordGuardrailDecision(ctx, agent, "halt", "identical_cycle_halt")
	if HasHarnessTurn(agent) {
		t.Fatal("turn state created while disabled")
	}
	if got := RecordFileEdit(ctx, nil, "patch", "replace", func(context.Context) string { return `{"success": true}` }); got != `{"success": true}` {
		t.Fatal(got)
	}
}

func TestReplyFinishLatchMatchesTheSameMapResponse(t *testing.T) {
	_, ctx := directRuntime(t)
	agent := harnessAgent("")
	first := map[string]any{"choices": []any{}}
	RecordReplyFinish(ctx, agent, first, "length")
	RecordReplyContent(ctx, agent, first, map[string]any{"content": "half"})
	RecordReplyContent(ctx, agent, map[string]any{"choices": []any{}}, map[string]any{"content": "hello"})
	route := "model=anthropic/claude-sonnet,provider=openrouter"
	want := rowSet("issue=none,"+route+"|1", "issue=truncated_length,"+route+"|1")
	if got := effRows(t, ctx, ModelReplyIssueMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}
