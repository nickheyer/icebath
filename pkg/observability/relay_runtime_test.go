package observability

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestTerminalModelErrorRetainsTheFailedRoute(t *testing.T) {
	fake, ctx := directRuntime(t)
	withProviderNames(t, "anthropic")
	base := map[string]any{"session_id": "s1", "task_id": "t1", "api_request_id": "r1", "provider": "anthropic", "model": "claude-sonnet"}
	invokeHook(ctx, "pre_api_request", base)
	invokeHook(ctx, "api_request_error", base, map[string]any{"retryable": false, "reason": "auth", "error": map[string]any{"message": "sensitive-error"}})
	if ends := fake.ops("llm.call_end"); len(ends) != 0 {
		t.Fatalf("call closed early: %v", ends)
	}
	rt := liveRuntime(ctx)
	session := rt.session(base)
	if session == nil || session.modelCalls.Len() != 1 {
		t.Fatalf("session %v", session)
	}
	if call := session.modelCalls.Values()[0]; !maps.Equal(call.fields, map[string]string{"model": "claude-sonnet", "provider": "anthropic"}) {
		t.Fatalf("fields %v", call.fields)
	}
	finalizeSession(ctx, "s1")
	ends := fake.ops("llm.call_end")
	if len(ends) != 1 {
		t.Fatalf("ends %v", ends)
	}
	want := map[string]any{"call_role": "primary", "error_class": "auth", "model": "claude-sonnet", "outcome": "failed", "provider": "anthropic", "ttft_bucket": "unknown"}
	if !maps.Equal(ends[0].payload.(map[string]any), want) {
		t.Fatalf("end payload %v", ends[0].payload)
	}
}

func TestSameRequestIDIsIsolatedBetweenTasks(t *testing.T) {
	fake, ctx := directRuntime(t)
	common := map[string]any{"session_id": "s1", "api_request_id": "shared-request", "platform": "cli", "provider": "anthropic", "model": "claude-sonnet"}
	for _, task := range []string{"t1", "t2"} {
		invokeHook(ctx, "pre_llm_call", common, map[string]any{"task_id": task})
		invokeHook(ctx, "pre_api_request", common, map[string]any{"task_id": task})
	}
	invokeHook(ctx, "post_api_request", common)
	if ends := fake.ops("llm.call_end"); len(ends) != 0 {
		t.Fatalf("ambiguous request closed a call: %v", ends)
	}
	for _, task := range []string{"t2", "t1"} {
		invokeHook(ctx, "post_api_request", common, map[string]any{"task_id": task})
		invokeHook(ctx, "on_session_end", common, map[string]any{"task_id": task}, successEnd)
	}
	finalizeSession(ctx, "s1")
	if ends := fake.ops("llm.call_end"); len(ends) != 2 {
		t.Fatalf("model ends %d", len(ends))
	}
	taskEnds := fake.taskEnds()
	if len(taskEnds) != 2 {
		t.Fatalf("task ends %v", taskEnds)
	}
	for _, fields := range taskEnds {
		if fields["model_call_count_bucket"] != "1" || fields["retry_count_bucket"] != "0" {
			t.Fatalf("task end %v", fields)
		}
	}
}

func TestReusedToolCallIDIsCountedForEachProviderRequest(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "pre_llm_call", base)
	for _, request := range []string{"request-1", "request-2"} {
		call := mergeEvent(base, map[string]any{"api_request_id": request, "tool_call_id": "provider-reused-id", "tool_name": "terminal"})
		invokeHook(ctx, "pre_tool_call", call, map[string]any{"args": map[string]any{"command": "private"}})
		invokeHook(ctx, "post_tool_call", call, map[string]any{"result": map[string]any{"output": "private"}, "status": "ok"})
	}
	invokeHook(ctx, "on_session_end", base, successEnd)
	finalizeSession(ctx, "s1")
	if ends := fake.ops("tool.call_end"); len(ends) != 2 {
		t.Fatalf("tool ends %d", len(ends))
	}
	taskEnds := fake.taskEnds()
	if len(taskEnds) != 1 || taskEnds[0]["tool_call_count_bucket"] != "2" {
		t.Fatalf("task ends %v", taskEnds)
	}
}

// Event without one key
func without(m map[string]any, key string) map[string]any {
	out := maps.Clone(m)
	delete(out, key)
	return out
}

func TestNonretryableProviderErrorCanRecoverWithinOneLogicalCall(t *testing.T) {
	fake, ctx := directRuntime(t)
	withProviderNames(t, "anthropic", "openai-api")
	base := map[string]any{"session_id": "s1", "task_id": "t1", "api_request_id": "r1", "provider": "anthropic", "model": "claude-sonnet", "retry_count": 0}
	invokeHook(ctx, "pre_api_request", base)
	invokeHook(ctx, "api_request_error", base, map[string]any{"retryable": false, "reason": "model_not_found"})
	fallback := mergeEvent(base, map[string]any{"provider": "openai-api", "model": "gpt-5"})
	invokeHook(ctx, "pre_api_request", fallback)
	invokeHook(ctx, "post_api_request", fallback)
	finalizeSession(ctx, "s1")
	ends, starts := fake.ops("llm.call_end"), fake.ops("llm.call")
	if len(ends) != 1 || len(starts) != 1 || starts[0].kwargs["model_name"] != "unknown" {
		t.Fatalf("starts %v ends %v", starts, ends)
	}
	want := map[string]any{"call_role": "primary", "error_class": "model_not_found", "model": "gpt-5", "outcome": "success", "provider": "openai-api", "ttft_bucket": "unknown"}
	if !maps.Equal(ends[0].payload.(map[string]any), want) {
		t.Fatalf("end %v", ends[0].payload)
	}
}

func TestPartialTerminalContextReusesThePendingToolSpan(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "api_request_id": "request-1", "platform": "cli", "tool_call_id": "tool-1", "tool_name": "terminal"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "pre_tool_call", base)
	invokeHook(ctx, "post_tool_call", without(base, "api_request_id"), map[string]any{"result": map[string]any{"output": "private"}, "status": "ok"})
	invokeHook(ctx, "on_session_end", base, successEnd)
	finalizeSession(ctx, "s1")
	ends := fake.ops("tool.call_end")
	if len(ends) != 1 || ends[0].payload.(map[string]any)["outcome"] != "success" {
		t.Fatalf("tool ends %v", ends)
	}
	if taskEnds := fake.taskEnds(); len(taskEnds) != 1 || taskEnds[0]["tool_call_count_bucket"] != "1" {
		t.Fatalf("task ends %v", taskEnds)
	}
}

func TestPartialTerminalVariantsDoNotDoubleCountACompletedCall(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "api_request_id": "request-1", "platform": "cli", "tool_call_id": "tool-1", "tool_name": "terminal"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "pre_tool_call", base)
	for _, omitted := range []string{"api_request_id", "turn_id"} {
		invokeHook(ctx, "post_tool_call", without(base, omitted), map[string]any{"result": map[string]any{"output": "private"}, "status": "ok"})
	}
	invokeHook(ctx, "on_session_end", base, successEnd)
	finalizeSession(ctx, "s1")
	if ends := fake.ops("tool.call_end"); len(ends) != 1 {
		t.Fatalf("tool ends %v", ends)
	}
	if taskEnds := fake.taskEnds(); len(taskEnds) != 1 || taskEnds[0]["tool_call_count_bucket"] != "1" {
		t.Fatalf("task ends %v", taskEnds)
	}
}

func TestAmbiguousPartialTerminalDoesNotCreateAPhantomToolSpan(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli", "tool_call_id": "provider-reused-id", "tool_name": "terminal"}
	invokeHook(ctx, "pre_llm_call", base)
	for _, request := range []string{"request-1", "request-2"} {
		invokeHook(ctx, "pre_tool_call", base, map[string]any{"api_request_id": request})
	}
	invokeHook(ctx, "post_tool_call", base, map[string]any{"result": map[string]any{"output": "ambiguous-private-result"}, "status": "ok"})
	invokeHook(ctx, "on_session_end", base, map[string]any{"completed": false, "failed": true, "interrupted": false, "turn_exit_reason": "system_aborted"})
	finalizeSession(ctx, "s1")
	ends := fake.ops("tool.call_end")
	if len(ends) != 2 {
		t.Fatalf("tool ends %v", ends)
	}
	for _, e := range ends {
		if e.payload.(map[string]any)["outcome"] != "failed" {
			t.Fatalf("outcome %v", e.payload)
		}
	}
	if taskEnds := fake.taskEnds(); len(taskEnds) != 1 || taskEnds[0]["tool_call_count_bucket"] != "2" {
		t.Fatalf("task ends %v", taskEnds)
	}
}

func TestReusedTaskIDStartsANewRunForEachTurn(t *testing.T) {
	fake, ctx := directRuntime(t)
	for _, turn := range []string{"turn-1", "turn-2"} {
		base := map[string]any{"session_id": "reused-session", "task_id": "reused-session", "turn_id": turn, "platform": "api"}
		invokeHook(ctx, "pre_llm_call", base)
		invokeHook(ctx, "post_tool_call", base, map[string]any{"api_request_id": "request-" + turn, "tool_call_id": "tool-" + turn, "tool_name": "read_file", "result": map[string]any{"output": "private"}, "status": "ok"})
		invokeHook(ctx, "on_session_end", base, successEnd)
	}
	finalizeSession(ctx, "reused-session")
	starts := 0
	for _, e := range fake.ops("scope.push") {
		if e.name == TaskScope {
			starts++
		}
	}
	taskEnds := fake.taskEnds()
	if starts != 2 || len(taskEnds) != 2 || len(fake.ops("tool.call_end")) != 2 {
		t.Fatalf("starts %d ends %v tools %d", starts, taskEnds, len(fake.ops("tool.call_end")))
	}
	for _, fields := range taskEnds {
		if fields["tool_call_count_bucket"] != "1" {
			t.Fatalf("task end %v", fields)
		}
	}
}

func TestLateToolResultDoesNotAttachToReusedTaskID(t *testing.T) {
	fake, ctx := directRuntime(t)
	first := map[string]any{"session_id": "reused-session", "task_id": "reused-session", "turn_id": "turn-1", "platform": "api"}
	invokeHook(ctx, "pre_llm_call", first)
	invokeHook(ctx, "pre_tool_call", first, map[string]any{"api_request_id": "request-1", "tool_call_id": "tool-1", "tool_name": "terminal"})
	invokeHook(ctx, "on_session_end", first, map[string]any{"completed": false, "failed": true, "interrupted": false, "turn_exit_reason": "timed_out"})
	second := mergeEvent(first, map[string]any{"turn_id": "turn-2"})
	rt := liveRuntime(ctx)
	if _, task, err := rt.startTask(ctx, nil, map[string]any{"session_id": "reused-session", "task_id": "reused-session", "platform": "api"}); task == nil || err != nil {
		t.Fatalf("second run not started %v", err)
	}
	invokeHook(ctx, "post_tool_call", first, map[string]any{"api_request_id": "request-1", "tool_call_id": "tool-1", "tool_name": "terminal", "result": map[string]any{"output": "late-private-result"}, "status": "ok"})
	invokeHook(ctx, "pre_llm_call", second)
	invokeHook(ctx, "post_tool_call", second, map[string]any{"api_request_id": "request-2", "tool_call_id": "tool-2", "tool_name": "read_file", "result": map[string]any{"output": "current-private-result"}, "status": "ok"})
	invokeHook(ctx, "on_session_end", second, successEnd)
	finalizeSession(ctx, "reused-session")
	ends := fake.ops("tool.call_end")
	var outcomes, categories []any
	for _, e := range ends {
		outcomes = append(outcomes, e.payload.(map[string]any)["outcome"])
		categories = append(categories, e.payload.(map[string]any)["tool_category"])
	}
	if len(ends) != 2 || outcomes[0] != "timed_out" || outcomes[1] != "success" || categories[0] != "terminal" || categories[1] != "file" {
		t.Fatalf("outcomes %v categories %v", outcomes, categories)
	}
	taskEnds := fake.taskEnds()
	if len(taskEnds) != 2 || taskEnds[0]["tool_call_count_bucket"] != "1" || taskEnds[1]["tool_call_count_bucket"] != "1" {
		t.Fatalf("task ends %v", taskEnds)
	}
}

func TestPendingToolIsClosedAndCountedWhenTaskIsInterrupted(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "on_session_start", base)
	invokeHook(ctx, "pre_tool_call", base, map[string]any{"tool_call_id": "tool-1", "tool_name": "terminal", "args": map[string]any{"command": "must-not-pass"}})
	invokeHook(ctx, "on_session_end", base, map[string]any{"completed": false, "failed": false, "interrupted": true, "turn_exit_reason": "interrupted_by_user"})
	invokeHook(ctx, "post_tool_call", base, map[string]any{"tool_call_id": "tool-1", "tool_name": "terminal", "result": map[string]any{"output": "late-result-must-not-pass"}, "status": "ok"})
	finalizeSession(ctx, "s1")
	ends := fake.ops("tool.call_end")
	if len(ends) != 1 {
		t.Fatalf("tool ends %v", ends)
	}
	got := ends[0].payload.(map[string]any)
	want := map[string]any{"approval_outcome": "not_required", "error_class": "interrupted", "latency_bucket": got["latency_bucket"], "outcome": "cancelled", "retry_count_bucket": "unknown", "tool_category": "terminal", "tool_name": "terminal"}
	if !maps.Equal(got, want) {
		t.Fatalf("tool end %v", got)
	}
	if taskEnds := fake.taskEnds(); len(taskEnds) != 1 || taskEnds[0]["tool_call_count_bucket"] != "1" {
		t.Fatalf("task ends %v", taskEnds)
	}
	starts := 0
	for _, e := range fake.ops("scope.push") {
		if e.name == TaskScope {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("task starts %d", starts)
	}
}

func TestPendingToolUsesTheOuterTaskTimeoutOutcome(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "api"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "pre_tool_call", base, map[string]any{"tool_call_id": "tool-1", "tool_name": "web_search"})
	FinishTaskRun(ctx, "s1", "t1", "api", nil, fmt.Errorf("private timeout detail: %w", context.DeadlineExceeded))
	finalizeSession(ctx, "s1")
	ends := fake.ops("tool.call_end")
	if len(ends) != 1 || ends[0].payload.(map[string]any)["outcome"] != "timed_out" {
		t.Fatalf("tool ends %v", ends)
	}
	taskEnds := fake.taskEnds()
	if len(taskEnds) != 1 || taskEnds[0]["outcome"] != "timed_out" || taskEnds[0]["tool_call_count_bucket"] != "1" {
		t.Fatalf("task ends %v", taskEnds)
	}
	if strings.Contains(fmt.Sprint(fake.events), "private timeout detail") {
		t.Fatal("timeout detail leaked into Relay events")
	}
}

func TestLateModelStartDoesNotCreateAnOrphanAfterTaskCompletion(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "on_session_end", base, successEnd)
	invokeHook(ctx, "pre_api_request", base, map[string]any{"api_request_id": "late-request", "provider": "nvidia", "model": "nvidia/nemotron-3-super-120b-a12b"})
	finalizeSession(ctx, "s1")
	if len(fake.ops("llm.call")) != 0 || len(fake.ops("llm.call_end")) != 0 {
		t.Fatalf("orphan model call %v", fake.ops("llm.call"))
	}
}

// Approval marks the runtime emitted
func approvalMarks(f *obsRelayFake) []map[string]any {
	var out []map[string]any
	for _, e := range f.ops("scope.event") {
		if e.name == ToolApprovalMark {
			out = append(out, e.kwargs["data"].(map[string]any))
		}
	}
	return out
}

func TestApprovalWithoutToolContextIsCountedAsUnattributed(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "on_session_start", base)
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "post_approval_response", map[string]any{"turn_id": "turn-1", "choice": "deny", "command": "must-not-pass"})
	invokeHook(ctx, "on_session_end", base, map[string]any{"completed": false, "failed": true, "interrupted": false, "turn_exit_reason": "approval_denied"})
	finalizeSession(ctx, "s1")
	if marks := approvalMarks(fake); len(marks) != 1 || !maps.Equal(marks[0], map[string]any{"attribution": "unattributed", "outcome": "denied"}) {
		t.Fatalf("approval marks %v", marks)
	}
}

func TestApprovalWithUnmatchedToolIDIsCountedAsUnattributed(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "post_approval_response", base, map[string]any{"tool_call_id": "spoofed-tool-call", "choice": "deny", "command": "must-not-pass"})
	invokeHook(ctx, "on_session_end", base, map[string]any{"completed": false, "failed": true, "interrupted": false, "turn_exit_reason": "approval_denied"})
	finalizeSession(ctx, "s1")
	if marks := approvalMarks(fake); len(marks) != 1 || !maps.Equal(marks[0], map[string]any{"attribution": "unattributed", "outcome": "denied"}) {
		t.Fatalf("approval marks %v", marks)
	}
}

// Skill marks of one name the runtime emitted
func skillMarks(f *obsRelayFake, name string) []fakeEvent {
	var out []fakeEvent
	for _, e := range f.ops("scope.event") {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}

func TestSkillLifecycleWithOnlyTaskIDUsesUniqueTaskScope(t *testing.T) {
	fake, ctx := directRuntime(t)
	rt := getRuntime(ctx, false, nil)
	_, task, err := rt.startTask(ctx, nil, map[string]any{"session_id": "session-1", "task_id": "task-1", "platform": "cli"})
	if task == nil || err != nil {
		t.Fatalf("task not started %v", err)
	}
	invokeHook(ctx, "on_skill_lifecycle", map[string]any{"action": "created", "skill_name": "private-skill-name", "provenance": "agent_created", "task_id": "task-1"})
	marks := skillMarks(fake, SkillLifecycleMark)
	if len(marks) != 1 || marks[0].kwargs["handle"] != task.handle {
		t.Fatalf("marks %v", marks)
	}
}

func TestSkillTaskOnlyCorrelationDoesNotGuessAcrossSessions(t *testing.T) {
	fake, ctx := directRuntime(t)
	rt := getRuntime(ctx, false, nil)
	for _, sid := range []string{"session-1", "session-2"} {
		if _, task, _ := rt.startTask(ctx, nil, map[string]any{"session_id": sid, "task_id": "shared-task", "platform": "cli"}); task == nil {
			t.Fatalf("task for %s not started", sid)
		}
	}
	invokeHook(ctx, "on_skill_lifecycle", map[string]any{"action": "created", "skill_name": "private-skill-name", "provenance": "agent_created", "task_id": "shared-task"})
	marks := skillMarks(fake, SkillLifecycleMark)
	if len(marks) != 1 {
		t.Fatalf("marks %v", marks)
	}
	if _, ok := marks[0].kwargs["handle"]; ok {
		t.Fatalf("mark guessed a task scope %v", marks[0].kwargs)
	}
}

func TestLateSkillLifecycleIsNotReemittedAtTheRoot(t *testing.T) {
	fake, ctx := directRuntime(t)
	base := map[string]any{"session_id": "session-1", "task_id": "task-1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "on_session_end", base, successEnd)
	invokeHook(ctx, "on_skill_lifecycle", base, map[string]any{"action": "loaded", "skill_name": "private-skill-name", "provenance": "local", "use_count": 2, "reused": true, "reuse_after_patch": false})
	if marks := skillMarks(fake, SkillLoadMark); len(marks) != 0 {
		t.Fatalf("late skill load reemitted %v", marks)
	}
}

func TestSkillLifecycleDoesNotFallbackAcrossAnExplicitSession(t *testing.T) {
	fake, ctx := directRuntime(t)
	rt := getRuntime(ctx, false, nil)
	if _, task, _ := rt.startTask(ctx, nil, map[string]any{"session_id": "session-1", "task_id": "task-1", "platform": "cli"}); task == nil {
		t.Fatal("task not started")
	}
	invokeHook(ctx, "on_skill_lifecycle", map[string]any{"action": "created", "skill_name": "private-skill-name", "provenance": "local", "session_id": "wrong-session", "task_id": "task-1"})
	if marks := skillMarks(fake, SkillLifecycleMark); len(marks) != 0 {
		t.Fatalf("fell back across sessions %v", marks)
	}
}

func TestTaskRetryCountSurvivesProviderFallbackOrdinalReset(t *testing.T) {
	fake, ctx := directRuntime(t)
	withProviderNames(t, "nvidia", "openai")
	base := map[string]any{"session_id": "s1", "task_id": "t1", "api_request_id": "r1", "platform": "cli", "provider": "nvidia", "model": "nvidia/nemotron-3-super-120b-a12b"}
	invokeHook(ctx, "pre_llm_call", base)
	for ordinal := range 2 {
		invokeHook(ctx, "pre_api_request", base, map[string]any{"retry_count": ordinal})
		invokeHook(ctx, "api_request_error", base, map[string]any{"retry_count": ordinal, "retryable": true})
	}
	fallback := mergeEvent(base, map[string]any{"provider": "openai", "model": "gpt-5", "retry_count": 0})
	invokeHook(ctx, "pre_api_request", fallback)
	invokeHook(ctx, "post_api_request", fallback)
	invokeHook(ctx, "on_session_end", base, successEnd)
	finalizeSession(ctx, "s1")
	ends := fake.ops("llm.call_end")
	if len(ends) != 1 {
		t.Fatalf("model ends %v", ends)
	}
	end := ends[0].payload.(map[string]any)
	if end["model"] != "gpt-5" || end["outcome"] != "success" || end["provider"] != "openai" {
		t.Fatalf("model end %v", end)
	}
	if taskEnds := fake.taskEnds(); len(taskEnds) != 1 || taskEnds[0]["retry_count_bucket"] != "2" {
		t.Fatalf("task ends %v", taskEnds)
	}
}
