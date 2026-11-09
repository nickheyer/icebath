package agent

import (
	"context"
	"math"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The _delegate_single helper, one task whose scripted child returns childResult
func delegateSingleStatus(t *testing.T, childResult map[string]any) *pyjson.Object {
	t.Helper()
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 0, 0, childResult)
	return delegateFirstResult(t, parent, "Test child status")
}

func statusTriple(t *testing.T, entry *pyjson.Object) (string, string, bool) {
	t.Helper()
	truncated, _ := entry.Get("truncated")
	return entryText(entry, "status"), entryText(entry, "exit_reason"), truncated == true
}

func TestDelegateFailedChildStatusErrorWithoutFailedFlagMarksFailed(t *testing.T) {
	entry := delegateSingleStatus(t, map[string]any{"final_response": "connection reset while streaming", "completed": false, "interrupted": false,
		"error": "connection reset", "api_calls": 2, "messages": []any{}})
	if status, exit, truncated := statusTriple(t, entry); status != "failed" || exit != "error" || truncated {
		t.Fatalf("entry %v", entry)
	}
}

func TestDelegateFailedChildStatusEmptyErrorWithSummaryIsCompleted(t *testing.T) {
	entry := delegateSingleStatus(t, map[string]any{"final_response": "work produced", "completed": true, "interrupted": false,
		"error": "", "api_calls": 2, "messages": []any{}})
	if status, exit, truncated := statusTriple(t, entry); status != "completed" || exit != "completed" || truncated {
		t.Fatalf("entry %v", entry)
	}
}

func TestDelegateFailedChildStatusGenuineTruncationStaysCompletedMaxIterations(t *testing.T) {
	entry := delegateSingleStatus(t, map[string]any{"final_response": "made partial progress before the budget ran out", "completed": false,
		"interrupted": false, "api_calls": 10, "messages": []any{}})
	if status, exit, truncated := statusTriple(t, entry); status != "completed" || exit != "max_iterations" || !truncated {
		t.Fatalf("entry %v", entry)
	}
}

func TestDelegateFailedChildStatusInterruptedUnchanged(t *testing.T) {
	entry := delegateSingleStatus(t, map[string]any{"final_response": "some partial output", "completed": false, "interrupted": true,
		"api_calls": 2, "messages": []any{}})
	if status, exit, truncated := statusTriple(t, entry); status != "interrupted" || exit != "interrupted" || truncated {
		t.Fatalf("entry %v", entry)
	}
}

// Parent with the cost counters the footer reads, the _make_parent_with_cost_counters helper
func costCounterParent(t *testing.T, startingCost float64) *AIAgent {
	t.Helper()
	parent := mockDelegateParent(t, 0)
	parent.sessionEstimatedCostUSD, parent.sessionCostStatus, parent.sessionCostSource = startingCost, "unknown", "none"
	return parent
}

func TestSubagentCostRollupSingleChildCostFoldedIntoParent(t *testing.T) {
	parent := costCounterParent(t, 0.10)
	scriptedCostChild(t, parent.env, "claude-sonnet-4-6", 1000, 200, 0.42, map[string]any{
		"final_response": "done", "completed": true, "interrupted": false, "api_calls": 2, "messages": []any{}})
	entry := delegateFirstResult(t, parent, "do stuff")
	if math.Abs(parent.sessionEstimatedCostUSD-0.52) > 1e-6 || entryHas(entry, "_child_cost_usd") || entryHas(entry, "_child_role") {
		t.Fatalf("parent cost %v entry %v", parent.sessionEstimatedCostUSD, entry)
	}
}

func TestSubagentCostRollupBatchChildrenCostsSumIntoParent(t *testing.T) {
	parent := costCounterParent(t, 0.0)
	entries := []*pyjson.Object{
		pyjson.ObjectOf("task_index", 0, "status", "completed", "summary", "A", "api_calls", 2, "duration_seconds", 1.0, "_child_role", "leaf", "_child_cost_usd", 0.15),
		pyjson.ObjectOf("task_index", 1, "status", "completed", "summary", "B", "api_calls", 2, "duration_seconds", 1.0, "_child_role", "leaf", "_child_cost_usd", 0.27),
		pyjson.ObjectOf("task_index", 2, "status", "failed", "summary", "", "error", "boom", "api_calls", 0, "duration_seconds", 0.1, "_child_role", "leaf", "_child_cost_usd", 0.03),
	}
	scriptChildEntries(t, entries...)
	out, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, map[string]any{"tasks": batchGoals("Investigate module A", "Investigate module B", "Investigate module C")})
	payload := dispatchPayload(t, out, err)
	if math.Abs(parent.sessionEstimatedCostUSD-0.45) > 1e-6 || parent.sessionCostSource != "subagent" || parent.sessionCostStatus != "estimated" {
		t.Fatalf("parent cost %v source %q status %q", parent.sessionEstimatedCostUSD, parent.sessionCostSource, parent.sessionCostStatus)
	}
	for _, raw := range batchResults(t, payload) {
		entry := raw.(*pyjson.Object)
		if entryHas(entry, "_child_cost_usd") || entryHas(entry, "_child_role") {
			t.Fatalf("entry %v", entry)
		}
	}
}
