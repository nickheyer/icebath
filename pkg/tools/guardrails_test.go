package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

func TestToolCallSignatureHashesCanonicalArgs(t *testing.T) {
	argsA := map[string]any{"z": []any{map[string]any{"β": "☤", "a": 1.0}}, "a": map[string]any{"y": 2.0, "x": "secret-token-value"}}
	argsB := map[string]any{"a": map[string]any{"x": "secret-token-value", "y": 2.0}, "z": []any{map[string]any{"a": 1.0, "β": "☤"}}}
	if CanonicalToolArgs(argsA) != CanonicalToolArgs(argsB) {
		t.Fatal("canonical args differ")
	}
	sigA, sigB := ToolCallSignatureFromCall("web_search", argsA), ToolCallSignatureFromCall("web_search", argsB)
	if sigA != sigB || len(sigA.ArgsHash) != 64 {
		t.Fatalf("signatures differ: %v %v", sigA, sigB)
	}
	meta := sigA.ToMetadata()
	if meta["tool_name"] != "web_search" || meta["args_hash"] != sigA.ArgsHash {
		t.Fatalf("metadata %v", meta)
	}
	if encoded := jsonCompact(meta); strings.Contains(encoded, "secret-token-value") || strings.Contains(encoded, "☤") {
		t.Fatal("metadata exposes raw args")
	}
}

func TestGuardrailConfigParsesNestedThresholds(t *testing.T) {
	cfg := ToolCallGuardrailConfigFromMap(map[string]any{
		"warnings_enabled": false, "hard_stop_enabled": true,
		"warn_after":      map[string]any{"exact_failure": 3, "same_tool_failure": 4, "idempotent_no_progress": 5},
		"hard_stop_after": map[string]any{"exact_failure": 6, "same_tool_failure": 7, "idempotent_no_progress": 8},
	}, "")
	if cfg.WarningsEnabled || !cfg.HardStopEnabled || cfg.ExactFailureWarnAfter != 3 || cfg.SameToolFailureWarnAfter != 4 ||
		cfg.NoProgressWarnAfter != 5 || cfg.ExactFailureBlockAfter != 6 || cfg.SameToolFailureHaltAfter != 7 || cfg.NoProgressBlockAfter != 8 {
		t.Fatalf("got %+v", cfg)
	}
	for _, platform := range []string{"cli", "tui", "desktop", "acp", "subagent", "api_server"} {
		if ToolCallGuardrailConfigFromMap(nil, platform).HardStopEnabled {
			t.Fatalf("%s hard stop on", platform)
		}
	}
	for _, platform := range []string{"telegram", "cron", "discord", "kanban"} {
		if !ToolCallGuardrailConfigFromMap(nil, platform).HardStopEnabled {
			t.Fatalf("%s hard stop off", platform)
		}
	}
	cfg = ToolCallGuardrailConfigFromMap(map[string]any{"non_interactive_hard_stop_enabled": false}, "telegram")
	if cfg.HardStopEnabled || cfg.NonInteractiveHardStopEnabled {
		t.Fatal("explicit disable ignored")
	}
}

func TestRepeatedIdenticalFailedCallWarnsWithoutBlocking(t *testing.T) {
	c := NewToolCallGuardrailController(nil)
	args := map[string]any{"query": "same"}
	var decisions []ToolGuardrailDecision
	for i := 0; i < 5; i++ {
		if c.BeforeCall("web_search", args).Action != "allow" {
			t.Fatal("blocked")
		}
		decisions = append(decisions, c.AfterCall("web_search", args, strPtr(`{"error":"boom"}`), boolPtr(true)))
	}
	if decisions[0].Action != "allow" {
		t.Fatalf("first %v", decisions[0])
	}
	for _, d := range decisions[1:] {
		if d.Action != "warn" || d.Code != "repeated_exact_failure_warning" {
			t.Fatalf("got %v", d)
		}
	}
	if c.BeforeCall("web_search", args).Action != "allow" || c.HaltDecision() != nil {
		t.Fatal("default config halted")
	}
}

func TestHardStopBlocksRepeatedExactFailure(t *testing.T) {
	cfg := DefaultToolCallGuardrailConfig()
	cfg.HardStopEnabled, cfg.ExactFailureWarnAfter, cfg.ExactFailureBlockAfter, cfg.SameToolFailureHaltAfter = true, 2, 2, 99
	c := NewToolCallGuardrailController(&cfg)
	args := map[string]any{"query": "same"}
	c.BeforeCall("web_search", args)
	if d := c.AfterCall("web_search", args, strPtr(`{"error":"boom"}`), boolPtr(true)); d.Action != "allow" {
		t.Fatalf("first %v", d)
	}
	c.BeforeCall("web_search", args)
	if d := c.AfterCall("web_search", args, strPtr(`{"error":"boom"}`), boolPtr(true)); d.Action != "warn" || d.Code != "repeated_exact_failure_warning" {
		t.Fatalf("second %v", d)
	}
	blocked := c.BeforeCall("web_search", args)
	if blocked.Action != "block" || blocked.Code != "repeated_exact_failure_block" || blocked.Count != 2 {
		t.Fatalf("got %v", blocked)
	}
}

func TestSkillReadToolsIdempotentNoProgressBlock(t *testing.T) {
	cases := []struct {
		tool, result string
		args         map[string]any
	}{
		{"skill_view", `{"success":true,"name":"gui-agent-ml-operations","content":"same"}`, map[string]any{"name": "gui-agent-ml-operations"}},
		{"skills_list", `{"success":true,"skills":[{"name":"gui-agent-ml-operations"}]}`, map[string]any{"category": "mlops"}},
	}
	for _, tc := range cases {
		cfg := DefaultToolCallGuardrailConfig()
		cfg.HardStopEnabled, cfg.NoProgressWarnAfter, cfg.NoProgressBlockAfter = true, 2, 2
		c := NewToolCallGuardrailController(&cfg)
		c.BeforeCall(tc.tool, tc.args)
		if c.AfterCall(tc.tool, tc.args, strPtr(tc.result), boolPtr(false)).Action != "allow" {
			t.Fatal("first warned")
		}
		c.BeforeCall(tc.tool, tc.args)
		if w := c.AfterCall(tc.tool, tc.args, strPtr(tc.result), boolPtr(false)); w.Action != "warn" || w.Code != "idempotent_no_progress_warning" {
			t.Fatalf("%s: %v", tc.tool, w)
		}
		if b := c.BeforeCall(tc.tool, tc.args); b.Action != "block" || b.Code != "idempotent_no_progress_block" {
			t.Fatalf("%s: %v", tc.tool, b)
		}
	}
	cfg := DefaultToolCallGuardrailConfig()
	cfg.NoProgressWarnAfter, cfg.NoProgressBlockAfter = 2, 2
	c := NewToolCallGuardrailController(&cfg)
	for i := 0; i < 3; i++ {
		if c.BeforeCall("write_file", map[string]any{"path": "/tmp/x"}).Action != "allow" || c.AfterCall("write_file", map[string]any{"path": "/tmp/x"}, strPtr("ok"), boolPtr(false)).Action != "allow" {
			t.Fatal("mutating tool blocked")
		}
		if c.BeforeCall("custom_tool", map[string]any{"x": 1.0}).Action != "allow" || c.AfterCall("custom_tool", map[string]any{"x": 1.0}, strPtr("ok"), boolPtr(false)).Action != "allow" {
			t.Fatal("unknown tool blocked")
		}
	}
}

func TestIdenticalCallStreakHaltsAnyToolWhenHardStopEnabled(t *testing.T) {
	cfg := DefaultToolCallGuardrailConfig()
	cfg.HardStopEnabled, cfg.NoProgressBlockAfter = true, 5
	c := NewToolCallGuardrailController(&cfg)
	args := map[string]any{"command": "icebath config get memory.provider"}
	for i := 1; i < 5; i++ {
		c.AfterCall("terminal", args, strPtr("local\n"), boolPtr(false))
		c.ObserveCall("terminal", args, "local\n", "", false)
		if c.HaltDecision() != nil {
			t.Fatalf("halted early at %d", i)
		}
	}
	c.AfterCall("terminal", args, strPtr("local\n"), boolPtr(false))
	c.ObserveCall("terminal", args, "local\n", "", false)
	halt := c.HaltDecision()
	if halt == nil || !halt.ShouldHalt() || halt.Code != "identical_call_streak_halt" || halt.ToolName != "terminal" || halt.Count != 5 {
		t.Fatalf("got %v", halt)
	}
	soft := DefaultToolCallGuardrailConfig()
	soft.NoProgressBlockAfter = 2
	sc := NewToolCallGuardrailController(&soft)
	for i := 0; i < 6; i++ {
		sc.ObserveCall("terminal", map[string]any{"command": "ls"}, "a\nb\n", "", false)
	}
	if sc.HaltDecision() != nil {
		t.Fatal("soft config halted")
	}
	hard := DefaultToolCallGuardrailConfig()
	hard.HardStopEnabled, hard.NoProgressBlockAfter = true, 2
	hc := NewToolCallGuardrailController(&hard)
	for i := 0; i < 6; i++ {
		hc.ObserveCall("process_manage", map[string]any{"action": "poll", "session_id": "p1"}, "running", "", false)
	}
	if hc.HaltDecision() != nil {
		t.Fatal("poller halted")
	}
	for i := 0; i < 6; i++ {
		hc.ObserveCall("terminal", map[string]any{"command": "date"}, fmt.Sprintf("t%d", i), "", false)
	}
	if hc.HaltDecision() != nil {
		t.Fatal("changed result halted")
	}
}

func TestLoopCaps(t *testing.T) {
	if LoopCapConfigFromMap(map[string]any{"max_web_searches": 0}).MaxWebSearches != 0 {
		t.Fatal("zero not kept")
	}
	if LoopCapConfigFromMap(map[string]any{"max_web_searches": -5}).MaxWebSearches != defaultMaxWebSearchesPerTurn {
		t.Fatal("negative not defaulted")
	}
	if LoopCapConfigFromMap(map[string]any{"max_subagents": "nope"}).MaxSubagents != defaultMaxSubagentsPerTurn {
		t.Fatal("junk not defaulted")
	}
	cfg := DefaultToolCallGuardrailConfig()
	cfg.LoopCaps = LoopCapConfig{MaxWebSearches: 3, MaxSubagents: defaultMaxSubagentsPerTurn}
	c := NewToolCallGuardrailController(&cfg)
	for i := 0; i < 3; i++ {
		if c.BeforeCall("web_search", map[string]any{"query": fmt.Sprintf("q%d", i)}).Action != "allow" {
			t.Fatal("capped early")
		}
	}
	d := c.BeforeCall("web_search", map[string]any{"query": "q4"})
	if d.Action != "block" || d.Code != "loop_web_search_cap" || !d.ShouldHalt() {
		t.Fatalf("got %v", d)
	}
}

func hardController() *ToolCallGuardrailController {
	cfg := DefaultToolCallGuardrailConfig()
	cfg.HardStopEnabled = true
	return NewToolCallGuardrailController(&cfg)
}

var pytestArgs = map[string]any{"command": "pytest tests/test_x.py -q"}

const redRun = `{"output": "1 failed", "exit_code": 1}`

func runRed(t *testing.T, c *ToolCallGuardrailController) ToolGuardrailDecision {
	t.Helper()
	if !c.BeforeCall("terminal", pytestArgs).AllowsExecution() {
		t.Fatal("red run blocked")
	}
	return c.AfterCall("terminal", pytestArgs, strPtr(redRun), boolPtr(true))
}

func TestFixRetestLoopIsNeverHardStopped(t *testing.T) {
	c := hardController()
	for i := 0; i < 12; i++ {
		if d := runRed(t, c); d.ShouldHalt() {
			t.Fatalf("halted on red run %d", i+1)
		}
		c.AfterCall("patch", map[string]any{"path": "x.py", "old_string": "a", "new_string": fmt.Sprintf("b%d", i)}, strPtr(`{"success": true, "diff": "..."}`), boolPtr(false))
	}
	if c.HaltDecision() != nil || !c.BeforeCall("terminal", pytestArgs).AllowsExecution() {
		t.Fatal("edit loop stopped")
	}
	c = hardController()
	for i := 0; i < 5; i++ {
		runRed(t, c)
	}
	if d := c.BeforeCall("terminal", pytestArgs); d.Action != "block" || d.Code != "repeated_exact_failure_block" {
		t.Fatalf("pure replay not blocked: %v", d)
	}
	c = hardController()
	for i := 0; i < 4; i++ {
		runRed(t, c)
	}
	c.AfterCall("write_file", map[string]any{"path": "x.py", "content": "y"}, strPtr(`{"bytes_written": 1}`), boolPtr(false))
	for i := 0; i < 5; i++ {
		if !c.BeforeCall("terminal", pytestArgs).AllowsExecution() {
			t.Fatal("fresh streak blocked early")
		}
		c.AfterCall("terminal", pytestArgs, strPtr(redRun), boolPtr(true))
	}
	if c.BeforeCall("terminal", pytestArgs).Action != "block" {
		t.Fatal("second streak not blocked")
	}
}

func TestDistinctFailingCommandsWarnButNeverHalt(t *testing.T) {
	c := hardController()
	for i := 0; i < 12; i++ {
		args := map[string]any{"command": fmt.Sprintf("grep -q needle%d haystack.txt", i)}
		if d := c.AfterCall("terminal", args, strPtr(redRun), boolPtr(true)); d.ShouldHalt() {
			t.Fatalf("same tool halt on distinct command %d", i+1)
		}
	}
	if c.HaltDecision() != nil {
		t.Fatal("halted")
	}
	c2 := hardController()
	var last ToolGuardrailDecision
	for i := 0; i < 8; i++ {
		last = c2.AfterCall("send_message", map[string]any{"to": fmt.Sprintf("u%d", i)}, strPtr(`{"error": "no route"}`), boolPtr(true))
	}
	if !last.ShouldHalt() || last.Code != "same_tool_failure_halt" {
		t.Fatalf("non tolerant tool did not halt: %v", last)
	}
	c3 := hardController()
	nav := map[string]any{"url": "https://example.test/app"}
	for i := 0; i < 8; i++ {
		if !c3.BeforeCall("browser_navigate", nav).AllowsExecution() {
			t.Fatal("browser retry blocked")
		}
		c3.AfterCall("browser_navigate", nav, strPtr(`{"error": "timeout"}`), boolPtr(true))
		c3.AfterCall("browser_click", map[string]any{"selector": "#retry"}, strPtr(`{"ok": true}`), boolPtr(false))
	}
	if c3.HaltDecision() != nil {
		t.Fatal("browser retry halted")
	}
}

func TestGuardrailRefusalsAreNotFailures(t *testing.T) {
	refusal := `{"error": "BLOCKED: repeated identical read", "guardrail_refusal": true}`
	if failed, _ := ClassifyToolFailure("read_file", strPtr(refusal)); failed {
		t.Fatal("refusal counted as failure")
	}
	if failed, _ := ClassifyToolFailure("read_file", strPtr(`{"error": "ENOENT: no such file"}`)); !failed {
		t.Fatal("real error not a failure")
	}
	if failed, _ := ClassifyToolFailure("read_file", strPtr(`{"error": "x", "guardrail_refusal": "yes"}`)); !failed {
		t.Fatal("truthy prose honoured as marker")
	}
	if failed, suffix := ClassifyToolFailure("terminal", strPtr(`{"output": "", "exit_code": 2}`)); !failed || suffix != " [exit 2]" {
		t.Fatalf("terminal exit: %v %q", failed, suffix)
	}
}

func executeCodeResult(n int) string {
	b, _ := json.Marshal(map[string]any{"status": "success", "output": "", "exit_code": 0, "tool_calls_made": 0,
		"duration_seconds": 1.0 + float64(n)/100,
		"kernel":           map[string]any{"mode": "session", "reused": true, "execution_count": 100 + n, "state_reset": false},
		"stdout_truncated": false, "stdout_bytes_captured": 0})
	return string(b)
}

func TestIdenticalStreakIgnoresVolatileExecutionMetadata(t *testing.T) {
	cfg := DefaultToolCallGuardrailConfig()
	cfg.HardStopEnabled, cfg.NoProgressBlockAfter = true, 5
	c := NewToolCallGuardrailController(&cfg)
	args := map[string]any{"code": "print(1) if False else None"}
	for i := 0; i < 4; i++ {
		c.ObserveCall("execute_code", args, executeCodeResult(i), "", false)
		if c.HaltDecision() != nil {
			t.Fatalf("halted early at %d", i)
		}
	}
	c.ObserveCall("execute_code", args, executeCodeResult(4), "", false)
	if halt := c.HaltDecision(); halt == nil || halt.Code != "identical_call_streak_halt" {
		t.Fatalf("got %v", halt)
	}
	cfg2 := DefaultToolCallGuardrailConfig()
	cfg2.HardStopEnabled, cfg2.NoProgressBlockAfter = true, 3
	c2 := NewToolCallGuardrailController(&cfg2)
	for i := 0; i < 6; i++ {
		out, _ := json.Marshal(map[string]any{"status": "success", "output": fmt.Sprintf("line %d", i), "duration_seconds": 1.0, "kernel": map[string]any{"execution_count": i}})
		c2.ObserveCall("execute_code", args, string(out), "", false)
	}
	if c2.HaltDecision() != nil {
		t.Fatal("different real output counted as replay")
	}
	c3 := NewToolCallGuardrailController(&cfg2)
	for i := 0; i < 6; i++ {
		out, _ := json.Marshal(map[string]any{"status": "done", "duration_seconds": 10 + i, "stats": map[string]any{"execution_count": 100 + i}})
		c3.ObserveCall("mcp_ci_job_status", map[string]any{"job": "build-42"}, string(out), "", false)
	}
	if c3.HaltDecision() != nil {
		t.Fatal("other tool metadata treated as volatile")
	}
	desktop := ToolCallGuardrailConfigFromMap(nil, "desktop")
	c4 := NewToolCallGuardrailController(&desktop)
	var notices []string
	for i := 1; i <= 6; i++ {
		notices = append(notices, c4.ObserveCall("execute_code", map[string]any{"code": `print("...")`}, executeCodeResult(i), fmt.Sprintf("c%d", i), false).Notice)
	}
	if notices[0] != "" || notices[1] != "" {
		t.Fatalf("early notice: %v", notices)
	}
	for _, n := range notices[2:] {
		if !strings.Contains(n, "consecutive identical call to execute_code") {
			t.Fatalf("notice missing: %q", n)
		}
	}
	if c4.HaltDecision() != nil {
		t.Fatal("warn only surface halted")
	}
}

func TestRateLimitedRetryIsNotAReplayButStaysBounded(t *testing.T) {
	cases := []struct {
		result    string
		throttled bool
	}{
		{`{"error": "Slack API error: ratelimited. Retry after 1 seconds."}`, true},
		{`{"ok": false, "error": "busy", "retry_after": 30}`, true},
		{`{"error": "channel_not_found"}`, false},
	}
	for _, prior := range []int{0, 2} {
		for _, tc := range cases {
			cfg := ToolCallGuardrailConfigFromMap(nil, "webhook")
			c := NewToolCallGuardrailController(&cfg)
			tool, args := "mcp_slack_history", map[string]any{"channel": "C123"}
			for i := 0; i < prior; i++ {
				c.AfterCall(tool, args, strPtr(`{"error": "upstream timeout"}`), nil)
			}
			c.AfterCall(tool, args, strPtr(tc.result), nil)
			retry := c.AfterCall(tool, args, strPtr(tc.result), nil)
			if (retry.Code == "repeated_exact_failure_warning") == tc.throttled {
				t.Fatalf("prior=%d result=%s retry=%v", prior, tc.result, retry)
			}
			bounded := false
			for i := 0; i < 10; i++ {
				if !c.BeforeCall(tool, args).AllowsExecution() {
					bounded = true
					break
				}
				if c.AfterCall(tool, args, strPtr(tc.result), nil).Action == "halt" {
					bounded = true
					break
				}
			}
			if !bounded {
				t.Fatal("permanently failing endpoint never halted or blocked")
			}
		}
	}
}

func TestIdenticalCycleDetection(t *testing.T) {
	cfg := DefaultToolCallGuardrailConfig()
	cfg.HardStopEnabled, cfg.NoProgressBlockAfter = true, 3
	c := NewToolCallGuardrailController(&cfg)
	var lastNotice string
	for lap := 0; lap < 3; lap++ {
		c.ObserveCall("read_file", map[string]any{"path": "a"}, "A", "", false)
		obs := c.ObserveCall("search_files", map[string]any{"pattern": "b"}, "B", "", false)
		lastNotice = obs.Notice
	}
	if !strings.Contains(lastNotice, "repeated the same cycle of 2 tool calls") {
		t.Fatalf("cycle notice missing: %q", lastNotice)
	}
	if halt := c.HaltDecision(); halt == nil || halt.Code != "identical_cycle_halt" {
		t.Fatalf("cycle halt missing: %v", halt)
	}
}

func TestResultReferenceStub(t *testing.T) {
	c := NewToolCallGuardrailController(nil)
	big := strings.Repeat("x", IdenticalResultStubMinChars)
	c.RecordPersistedResult("call-1", "/tmp/spill.txt")
	first := c.ObserveCall("read_file", map[string]any{"path": "a"}, big, "call-1", false)
	if first.Stub != "" {
		t.Fatal("first call stubbed")
	}
	second := c.ObserveCall("read_file", map[string]any{"path": "a"}, big, "call-2", false)
	if !strings.Contains(second.Stub, "byte-identical to the read_file result") || !strings.Contains(second.Stub, "/tmp/spill.txt") {
		t.Fatalf("stub %q", second.Stub)
	}
}

func TestDecisionHelpers(t *testing.T) {
	d := ToolGuardrailDecision{Action: "warn", Code: "repeated_exact_failure_warning", Message: "loop", ToolName: "t", Count: 2}
	if !strings.Contains(AppendToolguardGuidance("result", d), "[Tool loop warning: repeated_exact_failure_warning; count=2; loop]") {
		t.Fatal("guidance missing")
	}
	if AppendToolguardGuidance("result", ToolGuardrailDecision{Action: "allow"}) != "result" {
		t.Fatal("allow appended")
	}
	synthetic := ToolguardSyntheticResult(ToolGuardrailDecision{Action: "block", Code: "x", Message: "m", ToolName: "t"})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(synthetic), &parsed); err != nil || parsed["error"] != "m" {
		t.Fatalf("synthetic %s", synthetic)
	}
}
