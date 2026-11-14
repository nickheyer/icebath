package kanban

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// CLI invocation over a namespace with captured stdout and stderr
func cliFor(ctx context.Context, ns map[string]any) (*kanbanCLI, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &kanbanCLI{ctx: ctx, ns: ns, w: &out, errw: &errOut}, &out, &errOut
}

// Runs fn on a board connection, failing the test on any error
func mustConn(t *testing.T, ctx context.Context, fn func(conn state.SQLiteConn)) {
	t.Helper()
	if err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error { fn(conn); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCliGateTransportFailureFailsOpenButGenuineVerdictRejects(t *testing.T) {
	c, _, _ := cliFor(context.Background(), map[string]any{})
	patchGoalJudge(t, true, func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		return goals.JudgeResult{Verdict: "continue", Reason: "judge error: BadRequestError", TransportFailed: true}
	})
	if verdict, reason := c.goalModeHandoffRejection(goalTask("task-1"), "evidence"); verdict != "done" || reason != nil {
		t.Fatal(verdict, reason)
	}
	judgeGoalFn = func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		return goals.JudgeResult{Verdict: "continue", Reason: "goal not met yet"}
	}
	if verdict, reason := c.goalModeHandoffRejection(goalTask("task-1"), "evidence"); verdict != "continue" || reason == nil || *reason != "goal not met yet" {
		t.Fatal(verdict, reason)
	}
}

func TestCliGateBindsPerTaskAffinityScope(t *testing.T) {
	var seen []string
	patchGoalJudge(t, true, func(ctx context.Context, _, _ string, _ goals.JudgeOptions) goals.JudgeResult {
		seen = append(seen, provider.PortalAffinityScope(ctx))
		return goals.JudgeResult{Verdict: "done"}
	})
	c, _, _ := cliFor(context.Background(), map[string]any{})
	if verdict, reason := c.goalModeHandoffRejection(goalTask("task-9"), "ev"); verdict != "done" || reason != nil {
		t.Fatal(verdict, reason)
	}
	if len(seen) != 1 || seen[0] != "kanban:task-9" {
		t.Fatal(seen)
	}
	if scope := provider.PortalAffinityScope(context.Background()); scope != "" {
		t.Fatal(scope)
	}
	bound, _, _ := cliFor(provider.WithPortalAffinityScope(context.Background(), "outer-conversation"), map[string]any{})
	bound.goalModeHandoffRejection(goalTask("task-9"), "ev")
	if seen[len(seen)-1] != "outer-conversation" {
		t.Fatal(seen)
	}
}

func TestWorkerLinkPreservesForeignChildRules(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	var worker, parent, readyChild, runningChild string
	var workerRunID int64
	mustConn(t, ctx, func(conn state.SQLiteConn) {
		create := func(title string) string {
			id, err := CreateTask(ctx, conn, CreateTaskOptions{Title: title})
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		worker = create("worker")
		if task, err := ClaimTask(ctx, conn, worker, nil, strp("worker")); err != nil || task == nil {
			t.Fatal(task, err)
		}
		task, err := GetTask(ctx, conn, worker)
		if err != nil {
			t.Fatal(err)
		}
		workerRunID = *task.CurrentRunID
		parent, readyChild, runningChild = create("unfinished parent"), create("foreign ready child"), create("foreign running child")
		if task, err := ClaimTask(ctx, conn, runningChild, nil, strp("other")); err != nil || task == nil {
			t.Fatal(task, err)
		}
	})
	t.Setenv("ICEBATH_KANBAN_TASK", worker)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(workerRunID, 10))
	link := func(child string) (int, error) {
		c, _, _ := cliFor(ctx, map[string]any{"parent_id": parent, "child_id": child})
		return c.cmdLink()
	}
	if rc, err := link(readyChild); rc != 0 || err != nil {
		t.Fatal(rc, err)
	}
	var ve *ValueError
	if _, err := link(runningChild); !errors.As(err, &ve) || !strings.Contains(err.Error(), "child is already running") {
		t.Fatal(err)
	}
	if rc, err := link(worker); rc != 0 || err != nil {
		t.Fatal(rc, err)
	}
	mustConn(t, ctx, func(conn state.SQLiteConn) {
		for child, want := range map[string]int{readyChild: 1, runningChild: 0, worker: 1} {
			ids, err := ParentIDs(ctx, conn, child)
			if err != nil || len(ids) != want || (want == 1 && ids[0] != parent) {
				t.Fatal(child, ids, err)
			}
		}
	})
}
