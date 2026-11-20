package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
)

const (
	batchGoodA = "Refactor the login handler to use the new session helper"
	batchGoodB = "Write regression tests for the session expiry watcher"
)

// Parent of test_delegate_batch_validation.py _make_mock_parent over a temp home
func batchValidationParent(t *testing.T) *AIAgent {
	t.Helper()
	env := delegationTestEnv(t, 0, false)
	return &AIAgent{sessionID: "parent-session", env: env, baseURL: "https://openrouter.ai/api/v1", apiKey: "test-key",
		provider: "openrouter", apiMode: "chat_completions", model: "anthropic/claude-sonnet-4", platform: "cli"}
}

// Replaces the single child run with completed entries, returning the call counter
func stubCompletedChildRuns(t *testing.T, summaries ...string) *int32 {
	t.Helper()
	prev := runSingleChildFn
	var calls int32
	runSingleChildFn = func(_ context.Context, i int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		atomic.AddInt32(&calls, 1)
		summary := "ok"
		if i < len(summaries) {
			summary = summaries[i]
		}
		return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", summary, "api_calls", 1, "duration_seconds", 1.0, "_child_role", nil)
	}
	t.Cleanup(func() { runSingleChildFn = prev })
	return &calls
}

func batchGoals(goals ...string) []any {
	tasks := make([]any, len(goals))
	for i, g := range goals {
		tasks[i] = map[string]any{"goal": g}
	}
	return tasks
}

func callBatch(t *testing.T, parent *AIAgent, tasks []any) *pyjson.Object {
	t.Helper()
	out, err := delegateTask(context.Background(), parent, map[string]any{"tasks": tasks})
	return dispatchPayload(t, out, err)
}

func batchResults(t *testing.T, payload *pyjson.Object) []any {
	t.Helper()
	raw, _ := payload.Get("results")
	results, _ := raw.([]any)
	return results
}

func TestBatchDuplicateGoalsAllowedExactDuplicateGoalsAccepted(t *testing.T) {
	stubCompletedChildRuns(t)
	payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, batchGoodA))
	if entryHas(payload, "error") || len(batchResults(t, payload)) != 2 {
		t.Fatalf("payload %v", payload)
	}
}

func TestBatchDuplicateGoalsAllowedCaseWhitespaceVariantDuplicatesAccepted(t *testing.T) {
	stubCompletedChildRuns(t)
	payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, "  "+strings.ToUpper(batchGoodA)+"  "))
	if entryHas(payload, "error") || len(batchResults(t, payload)) != 2 {
		t.Fatalf("payload %v", payload)
	}
}

func TestBatchPlaceholderGoalsBareTodoRejectedCaseInsensitive(t *testing.T) {
	parent := batchValidationParent(t)
	for _, todo := range []string{"TODO", "todo", "ToDo"} {
		if payload := callBatch(t, parent, batchGoals(batchGoodA, todo)); !entryHas(payload, "error") {
			t.Fatalf("%q accepted %v", todo, payload)
		}
	}
}

func TestBatchPlaceholderGoalsTaskNPlaceholderRejected(t *testing.T) {
	payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, "Task 123456789"))
	if !strings.Contains(strings.ToLower(entryText(payload, "error")), "placeholder") {
		t.Fatalf("payload %v", payload)
	}
}

func TestBatchPlaceholderGoalsUnexpandedAngleTemplateMarkerRejected(t *testing.T) {
	payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, "Implement <feature_name> end to end"))
	if !strings.Contains(strings.ToLower(entryText(payload, "error")), "template") {
		t.Fatalf("payload %v", payload)
	}
}

func TestBatchPlaceholderGoalsUnexpandedBraceTemplateMarkerRejected(t *testing.T) {
	payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, "Summarize {file_path} for the report"))
	if !strings.Contains(strings.ToLower(entryText(payload, "error")), "template") {
		t.Fatalf("payload %v", payload)
	}
}

func TestBatchPlaceholderGoalsCodeShapedBracketsNotRejected(t *testing.T) {
	stubCompletedChildRuns(t)
	parent := batchValidationParent(t)
	for _, goal := range []string{
		"Refactor the parser to return Vec<T> instead of raw pointers",
		"Fix the Result<String> error propagation in the config loader",
		"Render the sidebar inside a <div> wrapper with flex layout",
		`Update the fixture to emit {"key": 1} for the happy path`,
		"Add a glob rule matching src/{a,b}/*.py to the lint config",
		"Rewrite the loop so {i} interpolates via f-strings correctly",
	} {
		if payload := callBatch(t, parent, batchGoals(batchGoodA, goal)); entryHas(payload, "error") {
			t.Fatalf("%q rejected %v", goal, payload)
		}
	}
}

func TestBatchPlaceholderGoalsMultiwordPlaceholderShapesStillRejected(t *testing.T) {
	parent := batchValidationParent(t)
	for _, goal := range []string{
		"Deploy the service to <target environment> when ready",
		"Backfill rows for {customer id} in the billing table",
		"Ship <FEATURE-NAME> behind the beta flag",
	} {
		payload := callBatch(t, parent, batchGoals(batchGoodA, goal))
		if !strings.Contains(strings.ToLower(entryText(payload, "error")), "template") {
			t.Fatalf("%q payload %v", goal, payload)
		}
	}
}

func TestBatchPlaceholderGoalsTooShortGoalRejected(t *testing.T) {
	if payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, "fix bug")); !entryHas(payload, "error") {
		t.Fatalf("payload %v", payload)
	}
}

func TestSingleTaskBatchOneTaskBatchIsValidSingleTaskShape(t *testing.T) {
	stubCompletedChildRuns(t, "done")
	if payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA)); entryHas(payload, "error") {
		t.Fatalf("payload %v", payload)
	}
}

func TestValidBatchStillRunsTwoDistinctGoalsPassValidation(t *testing.T) {
	stubCompletedChildRuns(t, "A done", "B done")
	payload := callBatch(t, batchValidationParent(t), batchGoals(batchGoodA, batchGoodB))
	if entryHas(payload, "error") || len(batchResults(t, payload)) != 2 {
		t.Fatalf("payload %v", payload)
	}
}

func TestValidBatchStillRunsSingleGoalFormUnaffectedByBatchChecks(t *testing.T) {
	stubCompletedChildRuns(t)
	out, err := delegateTask(context.Background(), batchValidationParent(t), map[string]any{"goal": "test"})
	if payload := dispatchPayload(t, out, err); entryHas(payload, "error") {
		t.Fatalf("payload %v", payload)
	}
}

// Memory provider counting shutdowns, the observable side of a closed child
type shutdownCounter struct {
	delegationRecorder
	shutdowns int32
}

func (c *shutdownCounter) Shutdown(context.Context) error {
	atomic.AddInt32(&c.shutdowns, 1)
	return nil
}

func TestOwnerRefusalMidBatchSiblingsBuiltBeforeARefusalReleaseTheirReservations(t *testing.T) {
	parent := batchValidationParent(t)
	counter := &shutdownCounter{}
	built := &AIAgent{sessionID: "built-child", env: parent.env, memoryManager: memory.NewMemoryManager(context.Background(), nil, nil)}
	if err := built.memoryManager.AddProvider(context.Background(), counter); err != nil {
		t.Fatal(err)
	}
	builds := 0
	prevBuild := buildChildPreservingParentToolsFn
	buildChildPreservingParentToolsFn = func(context.Context, childBuildSpec) (*AIAgent, error) {
		builds++
		if builds == 1 {
			return built, nil
		}
		return nil, &delegateValueError{msg: "the gateway owner refused another subagent"}
	}
	t.Cleanup(func() { buildChildPreservingParentToolsFn = prevBuild })
	runs := stubCompletedChildRuns(t)
	out, err := delegateTask(context.Background(), parent, map[string]any{"tasks": batchGoals(batchGoodA, batchGoodB)})
	payload := dispatchPayload(t, out, err)
	if !strings.Contains(entryText(payload, "error"), "refused another subagent") {
		t.Fatalf("payload %v", payload)
	}
	if atomic.LoadInt32(runs) != 0 {
		t.Fatalf("children ran %d times after the refusal", atomic.LoadInt32(runs))
	}
	if !built.memoryProviderShutdown || atomic.LoadInt32(&counter.shutdowns) != 1 {
		t.Fatalf("built child closed %v shutdowns %d", built.memoryProviderShutdown, counter.shutdowns)
	}
}
