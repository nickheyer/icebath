package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/tools"
)

// test_delegate_kanban_isolation.py test_build_child_agent_strips_kanban_toolset_even_when_parent_is_worker
func TestBuildChildAgentStripsKanbanToolsetEvenWhenParentIsWorker(t *testing.T) {
	parent := batchValidationParent(t)
	parent.enabledToolsets = []string{"terminal", "kanban"}
	parent.validToolNames = map[string]bool{"terminal": true, "kanban_complete": true, "kanban_comment": true}
	parent.model, parent.provider, parent.baseURL = "test-model", "test-provider", "http://example.invalid"
	parent.apiMode, parent.platform, parent.sessionID = "chat_completions", "cli", "parent-session"
	var captured AIAgentOptions
	prev := newChildAIAgentFn
	t.Cleanup(func() { newChildAIAgentFn = prev })
	newChildAIAgentFn = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
		captured = o
		return &AIAgent{validToolNames: map[string]bool{"terminal": true}, sessionID: "child-session", sessionDB: o.SessionDB, env: parent.env}, nil
	}
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	child, err := buildChildAgent(ctx, childBuildSpec{TaskIndex: 0, Goal: "review only", Context: nil, Toolsets: nil, Model: nil,
		MaxIterations: 3, TaskCount: 1, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if len(child.validToolNames) != 1 || !child.validToolNames["terminal"] {
		t.Fatal(child.validToolNames)
	}
	if slices.Contains(captured.EnabledToolsets, "kanban") || !slices.Contains(captured.DisabledToolsets, "kanban") {
		t.Fatalf("enabled %v disabled %v", captured.EnabledToolsets, captured.DisabledToolsets)
	}
}

// test_delegate_kanban_isolation.py test_child_attempting_default_complete_does_not_finish_parent_or_delete_workspace
func TestChildAttemptingDefaultCompleteDoesNotFinishParentOrDeleteWorkspace(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	workspace := filepath.Join(tmp, "parent-workspace")
	for _, dir := range []string{h, workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ICEBATH_KANBAN_HOME", "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD", "ICEBATH_DELEGATED_CHILD_CONTEXT"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_PROFILE", "parent-worker")
	t.Setenv("ICEBATH_KANBAN_WORKSPACE", workspace)
	t.Setenv("ICEBATH_KANBAN_ATTACHMENTS_ROOT", filepath.Join(tmp, "attachments"))
	ctx := context.Background()
	if _, err := kanban.InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn, err := kanban.Connect(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assignee, kind := "parent-worker", "scratch"
	tid, err := kanban.CreateTask(ctx, conn, kanban.CreateTaskOptions{Title: "parent", Assignee: &assignee, WorkspaceKind: &kind, WorkspacePath: &workspace})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := kanban.ClaimTask(ctx, conn, tid, nil, nil)
	conn.Close()
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", claim.ID)
	env := delegationTestEnv(t, 0, false)
	t.Setenv("ICEBATH_HOME", h)
	parent := &AIAgent{sessionID: "parent", env: env, currentTaskID: tid}
	child := &AIAgent{model: "test-model", env: env, delegateDepth: 1, subagentID: "sa-test", platform: "subagent", quietMode: true, suppressStatusOutput: true}
	prev := conversationLoopRun
	t.Cleanup(func() { conversationLoopRun = prev })
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a != child {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		raw := `{"summary": "wrong child completion"}`
		attempted := tools.NewDispatcher(tools.Default).RegistryDispatch(ctx, &tools.Call{Name: "kanban_complete",
			Args: map[string]any{"summary": "wrong child completion"}, RawArgs: raw})
		return map[string]any{"final_response": attempted, "completed": true, "api_calls": 0, "messages": []any{}}, nil
	}
	result := runSingleChild(ctx, 0, "try to complete parent", child, parent, "", nil, nil)
	conn, err = kanban.Connect(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	task, err := kanban.GetTask(ctx, conn, tid)
	if err != nil || task == nil {
		t.Fatal(task, err)
	}
	run, err := kanban.LatestRun(ctx, conn, tid)
	if err != nil || run == nil {
		t.Fatal(run, err)
	}
	if status := entryText(result, "status"); status != "completed" {
		t.Fatal(status, result)
	}
	if summary := entryText(result, "summary"); !strings.Contains(summary, "delegate_task child") {
		t.Fatal(summary)
	}
	if task.Status != "running" || run.Status != "running" {
		t.Fatal(task.Status, run.Status)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatal(err)
	}
}
