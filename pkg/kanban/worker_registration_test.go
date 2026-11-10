package kanban

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Claimed task of a worker whose dispatcher never recorded its pid, with the worker environment set
func workerEnv(t *testing.T) string {
	t.Helper()
	clearKanbanEnv(t)
	setupKanbanHome(t)
	t.Setenv("ICEBATH_PROFILE", "test-worker")
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	assignee := "test-worker"
	tid, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "worker-test", Assignee: &assignee})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	runID, err := currentRunID(ctx, conn, tid)
	if err != nil || runID == nil {
		t.Fatalf("run id %v %v", runID, err)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", tid)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*runID, 10))
	return tid
}

func expireClaim(t *testing.T, conn state.SQLiteConn, tid string) {
	t.Helper()
	mustExec(t, conn, "UPDATE tasks SET claim_expires = 1 WHERE id = '"+tid+"'")
}

func TestWorkerTheDispatcherNeverRecordedKeepsItsClaimOrNeverStarts(t *testing.T) {
	tid := workerEnv(t)
	ctx := context.Background()
	if !RegisterCurrentWorkerFromEnv(ctx) {
		t.Fatal("worker of a live run told not to start")
	}
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	if pid := mustTask(t, conn, tid).WorkerPID; pid == nil || *pid != int64(os.Getpid()) {
		t.Fatalf("worker pid %v", pid)
	}
	expireClaim(t, conn, tid)
	if released, err := ReleaseStaleClaims(ctx, conn, nil, nil); err != nil || released != 0 {
		t.Fatalf("released %d %v", released, err)
	}
	if status := mustTask(t, conn, tid).Status; status != "running" {
		t.Fatalf("status %q", status)
	}
	assignee := "test-worker"
	late, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "late orphan", Assignee: &assignee})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimTask(ctx, conn, late, nil, nil); err != nil {
		t.Fatal(err)
	}
	staleRun, err := currentRunID(ctx, conn, late)
	if err != nil || staleRun == nil {
		t.Fatalf("stale run %v %v", staleRun, err)
	}
	expireClaim(t, conn, late)
	if released, err := ReleaseStaleClaims(ctx, conn, nil, nil); err != nil || released != 1 {
		t.Fatalf("released %d %v", released, err)
	}
	if _, err := ClaimTask(ctx, conn, late, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", late)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", strconv.FormatInt(*staleRun, 10))
	if RegisterCurrentWorkerFromEnv(ctx) {
		t.Fatal("worker of a reclaimed run told to work the card")
	}
	if pid := mustTask(t, conn, late).WorkerPID; pid != nil {
		t.Fatalf("reclaimed run adopted pid %d", *pid)
	}
}

func TestReclaimLosesToAWorkerRegisteringMidSweep(t *testing.T) {
	tid := workerEnv(t)
	ctx := context.Background()
	real := terminateReclaimedWorkerFn
	t.Cleanup(func() { terminateReclaimedWorkerFn = real })
	terminateReclaimedWorkerFn = func(pid any, claimLock *string, signalFn SignalFunc, startedAt any) *pyjson.Object {
		if !RegisterCurrentWorkerFromEnv(ctx) {
			t.Error("worker refused while registering mid sweep")
		}
		return real(pid, claimLock, signalFn, startedAt)
	}
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	expireClaim(t, conn, tid)
	if released, err := ReleaseStaleClaims(ctx, conn, nil, nil); err != nil || released != 0 {
		t.Fatalf("released %d %v", released, err)
	}
	task := mustTask(t, conn, tid)
	if task.Status != "running" || task.WorkerPID == nil || *task.WorkerPID != int64(os.Getpid()) {
		t.Fatalf("task %q %v", task.Status, task.WorkerPID)
	}
}

func TestRegisterCurrentWorkerLetsUnreadableContextsRun(t *testing.T) {
	clearKanbanEnv(t)
	setupKanbanHome(t)
	ctx := context.Background()
	if !RegisterCurrentWorkerFromEnv(ctx) {
		t.Fatal("a process that is no worker refused")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "t_x")
	if !RegisterCurrentWorkerFromEnv(ctx) {
		t.Fatal("a worker with no run id refused")
	}
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "not-a-number")
	if !RegisterCurrentWorkerFromEnv(ctx) {
		t.Fatal("a worker with an unreadable run id refused")
	}
	if id, ok := workerRunID("other"); ok || id != 0 {
		t.Fatal("run id read for a task this worker is not scoped to")
	}
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "7")
	t.Setenv("ICEBATH_KANBAN_DB", "/nonexistent-dir/for/kanban/board.db")
	if !RegisterCurrentWorkerFromEnv(ctx) {
		t.Fatal("a board error refused the worker")
	}
}
