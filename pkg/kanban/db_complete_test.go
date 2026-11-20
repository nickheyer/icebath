package kanban

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Initialized default board connection in a sandboxed kanban home
func openTestBoard(t *testing.T) state.SQLiteConn {
	t.Helper()
	setupKanbanHome(t)
	if _, err := InitDB(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Decoded payload object of an event, empty when none
func payloadOf(e *Event) *pyjson.Object {
	if o, ok := e.Payload.(*pyjson.Object); ok && o != nil {
		return o
	}
	return pyjson.NewObject()
}

// Event kinds of a task in id order
func eventKinds(t *testing.T, conn state.SQLiteConn, id string) []string {
	t.Helper()
	events, err := ListEvents(context.Background(), conn, id)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(events))
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// Count of a kind in a list of kinds
func countKind(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// Claims a task as this host and fingerprints the current process as its worker when asked
func claimedRunningTask(t *testing.T, conn state.SQLiteConn, liveWorker bool) (string, int64) {
	t.Helper()
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "live", Assignee: sp("coder")})
	if claimed, err := ClaimTask(ctx, conn, tid, nil, sp(claimerID())); err != nil || claimed == nil {
		t.Fatal("claim", err)
	}
	if liveWorker {
		if err := setWorkerPid(ctx, conn, tid, os.Getpid(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	runID, err := currentRunID(ctx, conn, tid)
	if err != nil || runID == nil {
		t.Fatal("run id", err)
	}
	return tid, *runID
}

// Status of a task read straight from the table
func statusOf(t *testing.T, conn state.SQLiteConn, id string) string {
	t.Helper()
	row, err := QueryOne(context.Background(), conn, "SELECT status FROM tasks WHERE id = ?", id)
	if err != nil || row == nil {
		t.Fatal("status", id, err)
	}
	return cellStr(row.Get("status"))
}

// Run row of one id
func runRow(t *testing.T, conn state.SQLiteConn, runID int64) *Row {
	t.Helper()
	row, err := QueryOne(context.Background(), conn, "SELECT ended_at, outcome FROM task_runs WHERE id = ?", runID)
	if err != nil || row == nil {
		t.Fatal("run row", err)
	}
	return row
}

func TestClaimlessCompleteRefusesLiveRunUntilForced(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid, runID := claimedRunningTask(t, conn, true)
	_, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Result: sp("someone else says done")})
	var live *LiveClaimError
	if !errors.As(err, &live) {
		t.Fatal("expected LiveClaimError", err)
	}
	if runRow(t, conn, runID).Get("ended_at") != nil || statusOf(t, conn, tid) != "running" {
		t.Fatal("refused completion moved state")
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Result: sp("worker done"), ExpectedRunID: &runID}); err != nil || !ok {
		t.Fatal("owner completion", ok, err)
	}
	tid2, run2 := claimedRunningTask(t, conn, true)
	if ok, err := CompleteTask(ctx, conn, tid2, CompleteTaskOptions{Result: sp("operator override"), Force: true}); err != nil || !ok {
		t.Fatal("forced completion", ok, err)
	}
	row := runRow(t, conn, run2)
	if row.Get("ended_at") == nil || row.Get("outcome") != "completed" {
		t.Fatal("forced completion left the run open", row.Vals)
	}
}

func TestClaimlessCompleteOfClaimWithoutLiveWorkerUnchanged(t *testing.T) {
	conn := openTestBoard(t)
	tid, runID := claimedRunningTask(t, conn, false)
	if ok, err := CompleteTask(context.Background(), conn, tid, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if runRow(t, conn, runID).Get("ended_at") == nil {
		t.Fatal("run still open")
	}
}

func TestClaimlessCompleteOfUnclaimedCardUnchanged(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "admin", Assignee: sp("coder")})
	if ok, err := CompleteTask(context.Background(), conn, tid, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if statusOf(t, conn, tid) != "done" {
		t.Fatal("not done")
	}
}

func TestRequestReviewSharesTheLiveWorkerFence(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid, _ := claimedRunningTask(t, conn, false)
	if ok, _, err := RequestReview(ctx, conn, tid, RequestReviewOptions{Summary: sp("handoff")}); err != nil || !ok {
		t.Fatal("review without live worker", ok, err)
	}
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("not in review")
	}
	tid2, run2 := claimedRunningTask(t, conn, true)
	ok, reason, err := RequestReview(ctx, conn, tid2, RequestReviewOptions{Summary: sp("steal")})
	if err != nil || ok || !strings.Contains(reason, "live claim") {
		t.Fatal("live claim not fenced", ok, reason, err)
	}
	if ok, _, err := RequestReview(ctx, conn, tid2, RequestReviewOptions{Summary: sp("own"), ExpectedRunID: &run2}); err != nil || !ok {
		t.Fatal("owner review", ok, err)
	}
}

// Claimed running task owned by this host
func readyTask(t *testing.T, conn state.SQLiteConn) string {
	t.Helper()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "demo", Assignee: sp("coder")})
	if claimed, err := ClaimTask(context.Background(), conn, tid, nil, sp(claimerID())); err != nil || claimed == nil {
		t.Fatal("claim", err)
	}
	return tid
}

// Asserts a completion fails with EmptyCompletionError
func expectEmptyCompletion(t *testing.T, conn state.SQLiteConn, tid string, o CompleteTaskOptions) {
	t.Helper()
	_, err := CompleteTask(context.Background(), conn, tid, o)
	var empty *EmptyCompletionError
	if !errors.As(err, &empty) {
		t.Fatal("expected EmptyCompletionError", err)
	}
}

func TestCompleteWithoutEvidenceRaises(t *testing.T) {
	conn := openTestBoard(t)
	tid := readyTask(t, conn)
	expectEmptyCompletion(t, conn, tid, CompleteTaskOptions{})
	row, err := QueryOne(context.Background(), conn, "SELECT status, result FROM tasks WHERE id = ?", tid)
	if err != nil || row.Get("status") != "running" || truthy(row.Get("result")) {
		t.Fatal("state moved", err, row.Vals)
	}
	kinds := eventKinds(t, conn, tid)
	if countKind(kinds, "completion_blocked_empty_result") == 0 || countKind(kinds, "completed") != 0 {
		t.Fatal(kinds)
	}
}

func TestWhitespaceResultRaises(t *testing.T) {
	conn := openTestBoard(t)
	tid := readyTask(t, conn)
	expectEmptyCompletion(t, conn, tid, CompleteTaskOptions{Result: sp("   ")})
	if statusOf(t, conn, tid) != "running" {
		t.Fatal("status moved")
	}
}

func TestWhitespaceStoredResultDoesNotCount(t *testing.T) {
	conn := openTestBoard(t)
	tid := readyTask(t, conn)
	if _, err := conn.ExecContext(context.Background(), "UPDATE tasks SET result = ? WHERE id = ?", "  ", tid); err != nil {
		t.Fatal(err)
	}
	expectEmptyCompletion(t, conn, tid, CompleteTaskOptions{})
	if ok, err := CompleteTask(context.Background(), conn, tid, CompleteTaskOptions{Result: sp("shipped the gate")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if statusOf(t, conn, tid) != "done" {
		t.Fatal("not done")
	}
}

func TestReviewApprovalExempt(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "review-me", Assignee: sp("coder")})
	if claimed, err := ClaimTask(ctx, conn, tid, nil, sp(claimerID())); err != nil || claimed == nil {
		t.Fatal("claim", err)
	}
	if ok, _, err := RequestReview(ctx, conn, tid, RequestReviewOptions{Summary: sp("please look")}); err != nil || !ok {
		t.Fatal("review", ok, err)
	}
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("not review")
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{}); err != nil || !ok {
		t.Fatal("approval", ok, err)
	}
	if mustTask(t, conn, tid).Status != "done" {
		t.Fatal("not done")
	}
}

func TestRejectThenSucceed(t *testing.T) {
	conn := openTestBoard(t)
	tid := readyTask(t, conn)
	expectEmptyCompletion(t, conn, tid, CompleteTaskOptions{Summary: sp("   ")})
	if ok, err := CompleteTask(context.Background(), conn, tid, CompleteTaskOptions{Summary: sp("fixed empty complete")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if statusOf(t, conn, tid) != "done" {
		t.Fatal("not done")
	}
}

// Replaces the pid liveness seam for the test duration
func stubPidAlive(t *testing.T, alive bool) {
	t.Helper()
	prev := pidAlive
	pidAlive = func(any) bool { return alive }
	t.Cleanup(func() { pidAlive = prev })
}

// Claims a task, fingerprints this process and backdates the run so enforce_max_runtime books a timeout
func timeOutOnce(t *testing.T, conn state.SQLiteConn, tid string) {
	t.Helper()
	ctx := context.Background()
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := setWorkerPid(ctx, conn, tid, os.Getpid(), nil, nil); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Unix() - 30
	err := WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET started_at = ? WHERE id = ?", started, tid); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "UPDATE task_runs SET started_at = ? WHERE id = (SELECT current_run_id FROM tasks WHERE id = ?)", started, tid)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	timedOut, err := EnforceMaxRuntime(ctx, conn, func(int, syscall.Signal) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range timedOut {
		if id == tid {
			return
		}
	}
	t.Fatal("task not timed out", timedOut)
}

func TestTypedBlockClassifiesUntypedBreakerBlock(t *testing.T) {
	stubPidAlive(t, false)
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "ceiling", Assignee: sp("worker"), MaxRuntimeSeconds: i64ptr(1)})
	timeOutOnce(t, conn, tid)
	if mustTask(t, conn, tid).Status != "ready" {
		t.Fatal("first timeout did not requeue")
	}
	timeOutOnce(t, conn, tid)
	parked := mustTask(t, conn, tid)
	if parked.Status != "blocked" || parked.BlockKind != nil || parked.BlockRecurrences != 0 {
		t.Fatal("breaker did not park untyped", parked.Status, parked.BlockKind, parked.BlockRecurrences)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, tid, sp("needs a human decision"), sp("needs_input"), nil); err != nil || !ok {
		t.Fatal("classify", ok, err)
	}
	after := mustTask(t, conn, tid)
	if after.Status != "blocked" || after.BlockKind == nil || *after.BlockKind != "needs_input" || after.BlockRecurrences != 1 {
		t.Fatal("classified", after.Status, after.BlockKind, after.BlockRecurrences)
	}
	if after.ConsecutiveFailures != parked.ConsecutiveFailures || !strPtrEq(after.LastFailureError, parked.LastFailureError) || after.CurrentRunID != nil {
		t.Fatal("evidence changed")
	}
	kinds := eventKinds(t, conn, tid)
	if countKind(kinds, "blocked") != 1 || countKind(kinds, "gave_up") != 1 || countKind(kinds, "timed_out") != 2 {
		t.Fatal(kinds)
	}
	runs, err := ListRuns(ctx, conn, tid, true, nil, nil)
	if err != nil || len(runs) != 2 {
		t.Fatal("runs", len(runs), err)
	}
}

func TestTypedBlockStillRefusesTypedOrLiveBlockedCards(t *testing.T) {
	stubPidAlive(t, false)
	conn := openTestBoard(t)
	ctx := context.Background()
	parked := mustCreate(t, conn, CreateTaskOptions{Title: "parked", Assignee: sp("worker"), MaxRuntimeSeconds: i64ptr(1)})
	timeOutOnce(t, conn, parked)
	timeOutOnce(t, conn, parked)
	if mustTask(t, conn, parked).Status != "blocked" {
		t.Fatal("not parked")
	}
	row, err := QueryOne(ctx, conn, "SELECT MAX(id) AS id FROM task_runs WHERE task_id = ?", parked)
	if err != nil || row.Get("id") == nil {
		t.Fatal("stale run id", err)
	}
	stale := cellInt(row.Get("id"))
	if ok, err := BlockTaskWithKind(ctx, conn, parked, sp("mine"), sp("needs_input"), &stale); err != nil || ok {
		t.Fatal("stale owner classified", ok, err)
	}
	if mustTask(t, conn, parked).BlockKind != nil {
		t.Fatal("kind attached by stale owner")
	}
	typed := mustCreate(t, conn, CreateTaskOptions{Title: "typed once", Assignee: sp("worker")})
	if _, err := ClaimTask(ctx, conn, typed, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, typed, sp("decision"), sp("needs_input"), nil); err != nil || !ok {
		t.Fatal("typed block", ok, err)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, typed, sp("again"), sp("capability"), nil); err != nil || ok {
		t.Fatal("re-typed", ok, err)
	}
	if k := mustTask(t, conn, typed).BlockKind; k == nil || *k != "needs_input" {
		t.Fatal("kind changed")
	}
	live := mustCreate(t, conn, CreateTaskOptions{Title: "live run", Assignee: sp("worker")})
	if _, err := ClaimTask(ctx, conn, live, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'blocked' WHERE id = ?", live)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, live, sp("race"), sp("needs_input"), nil); err != nil || ok {
		t.Fatal("live run classified", ok, err)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, live, sp("no kind"), nil, nil); err != nil || ok {
		t.Fatal("kind-less call accepted", ok, err)
	}
	if mustTask(t, conn, live).BlockKind != nil {
		t.Fatal("kind attached")
	}
}

// Child in todo under parents that may already be done
func stuckTodo(t *testing.T, conn state.SQLiteConn, parentsDone bool, nParents int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	var parents []string
	for i := 0; i < nParents; i++ {
		parents = append(parents, mustCreate(t, conn, CreateTaskOptions{Title: "parent" + string(rune('0'+i)), Assignee: sp("setup")}))
	}
	child := mustCreate(t, conn, CreateTaskOptions{Title: "child", Parents: parents, Assignee: sp("setup")})
	if mustTask(t, conn, child).Status != "todo" {
		t.Fatal("child not todo")
	}
	if parentsDone {
		for _, pid := range parents {
			if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status='done' WHERE id=?", pid); err != nil {
				t.Fatal(err)
			}
		}
	}
	return child, parents
}

func TestPromoteStuckTodoSucceeds(t *testing.T) {
	conn := openTestBoard(t)
	child, _ := stuckTodo(t, conn, true, 1)
	ok, reason, err := PromoteTask(context.Background(), conn, child, "tester", nil, false)
	if err != nil || !ok || reason != "" {
		t.Fatal(ok, reason, err)
	}
	if mustTask(t, conn, child).Status != "ready" {
		t.Fatal("not ready")
	}
}

func TestPromoteRefusesUndoneParentAndNamesTheRealRemedy(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	child, parents := stuckTodo(t, conn, false, 1)
	ok, reason, err := PromoteTask(ctx, conn, child, "tester", sp("recovery"), false)
	if err != nil || ok || !strings.Contains(reason, parents[0]) {
		t.Fatal(ok, reason, err)
	}
	if mustTask(t, conn, child).Status != "todo" {
		t.Fatal("status moved")
	}
	if claimed, err := ClaimTask(ctx, conn, child, nil, nil); err != nil || claimed != nil {
		t.Fatal("gate bypassed", claimed, err)
	}
}

// Triage task with optional body and assignee
func createTriage(t *testing.T, conn state.SQLiteConn, title string) string {
	t.Helper()
	return mustCreate(t, conn, CreateTaskOptions{Title: title, Triage: true})
}

func TestSpecifyPromotesTriageToTodo(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := createTriage(t, conn, "rough idea")
	if mustTask(t, conn, tid).Status != "triage" {
		t.Fatal("not triage")
	}
	ok, err := SpecifyTriageTask(ctx, conn, tid, sp("Refined: rough idea"), sp("**Goal**\nDo the thing."), nil, sp("specifier-bot"))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	task := mustTask(t, conn, tid)
	if task.Status != "ready" || task.Title != "Refined: rough idea" || task.Body == nil || !strings.Contains(*task.Body, "**Goal**") {
		t.Fatal(task.Status, task.Title, task.Body)
	}
}

func TestSpecifyRejectsBlankTitle(t *testing.T) {
	conn := openTestBoard(t)
	tid := createTriage(t, conn, "rough")
	_, err := SpecifyTriageTask(context.Background(), conn, tid, sp("   "), sp("ok"), nil, nil)
	var ve *ValueError
	if !errors.As(err, &ve) {
		t.Fatal("expected ValueError", err)
	}
}

func TestSpecifyRecordsAuditCommentOnlyWhenAuthorGiven(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid1 := createTriage(t, conn, "a")
	if _, err := SpecifyTriageTask(ctx, conn, tid1, sp("A-spec"), sp("b"), nil, sp("ace")); err != nil {
		t.Fatal(err)
	}
	comments1, err := ListComments(ctx, conn, tid1)
	if err != nil || len(comments1) != 1 || !strings.Contains(comments1[0].Body, "Specified") || comments1[0].Author != "ace" {
		t.Fatal("audit comment", err, comments1)
	}
	tid2 := createTriage(t, conn, "b")
	if _, err := SpecifyTriageTask(ctx, conn, tid2, sp("B-spec"), sp("b"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if comments2, err := ListComments(ctx, conn, tid2); err != nil || len(comments2) != 0 {
		t.Fatal("silent specify commented", err, comments2)
	}
}

func TestBlockHookRunsAfterCommitWithWriteLockFree(t *testing.T) {
	for _, tc := range []struct{ kind, status, event string }{
		{"dependency", "todo", "dependency_wait"},
		{"needs_input", "blocked", "blocked"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			conn := openTestBoard(t)
			ctx := context.Background()
			dbPath := mustDBPath(t, nil)
			tid := mustCreate(t, conn, CreateTaskOptions{Title: "child", Assignee: sp("worker")})
			var seen [][3]string
			captureHook(t, "kanban_task_blocked", func(context.Context, map[string]any) (any, error) {
				other, err := state.ConnectSQLite(dbPath, state.SQLiteConfig{Timeout: 0})
				if err != nil {
					t.Error(err)
					return nil, nil
				}
				defer other.Close()
				row, _ := QueryOne(ctx, other, "SELECT status FROM tasks WHERE id = ?", tid)
				last, _ := QueryOne(ctx, other, "SELECT kind FROM task_events WHERE task_id = ? ORDER BY id DESC LIMIT 1", tid)
				lock := "free"
				if _, err := other.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
					lock = state.ErrText(err)
				} else {
					other.ExecContext(ctx, "ROLLBACK")
				}
				seen = append(seen, [3]string{cellStr(row.Get("status")), cellStr(last.Get("kind")), lock})
				return nil, nil
			})
			parent := mustCreate(t, conn, CreateTaskOptions{Title: "parent"})
			if _, err := LinkTasks(ctx, conn, parent, tid, nil); err != nil {
				t.Fatal(err)
			}
			if err := WriteTxn(ctx, conn, false, func() error {
				_, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready' WHERE id = ?", tid)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if ok, err := BlockTaskWithKind(ctx, conn, tid, sp("waiting"), sp(tc.kind), nil); err != nil || !ok {
				t.Fatal("block", ok, err)
			}
			if len(seen) != 1 || seen[0] != [3]string{tc.status, tc.event, "free"} {
				t.Fatal("hook observation", seen)
			}
		})
	}
}

func TestMisbehavingHookDoesNotBreakTransition(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	captureHook(t, "kanban_task_completed", func(context.Context, map[string]any) (any, error) {
		return nil, errors.New("plugin exploded")
	})
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "t", Assignee: sp("worker")})
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Summary: sp("ok")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "done" {
		t.Fatal("not done")
	}
}

// Task linked under parents and driven to running so block_task can act
func runningTask(t *testing.T, conn state.SQLiteConn, title string, parents ...string) string {
	t.Helper()
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: title, Assignee: sp("worker")})
	for _, p := range parents {
		if _, err := LinkTasks(ctx, conn, p, tid, nil); err != nil {
			t.Fatal(err)
		}
	}
	makeRunningAgain(t, conn, tid)
	return tid
}

// Forces a task to ready and claims it as worker
func makeRunningAgain(t *testing.T, conn state.SQLiteConn, tid string) {
	t.Helper()
	ctx := context.Background()
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET status='ready' WHERE id=?", tid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimTask(ctx, conn, tid, nil, sp("worker")); err != nil || claimed == nil {
		t.Fatal("claim", err)
	}
}

func TestBlockLoopDetectedEventEmitted(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := runningTask(t, conn, "t")
	if _, err := BlockTaskWithKind(ctx, conn, tid, sp("x"), sp("capability"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := UnblockTask(ctx, conn, tid); err != nil {
		t.Fatal(err)
	}
	makeRunningAgain(t, conn, tid)
	if _, err := BlockTaskWithKind(ctx, conn, tid, sp("x"), sp("capability"), nil); err != nil {
		t.Fatal(err)
	}
	events := eventsOfKind(t, conn, tid, "block_loop_detected")
	if len(events) == 0 {
		t.Fatal("expected a block_loop_detected event")
	}
	p := payloadOf(events[len(events)-1])
	rec, _ := p.Get("recurrences")
	kind, _ := p.Get("kind")
	if !pyjson.Equal(rec, int64(2)) || kind != "capability" {
		t.Fatal(rec, kind)
	}
}

func TestDependencyThenParentDonePromotes(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "parent", Assignee: sp("worker")})
	child := runningTask(t, conn, "child")
	if _, err := LinkTasks(ctx, conn, parent, child, mustTask(t, conn, child).CurrentRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := BlockTaskWithKind(ctx, conn, child, sp("wait"), sp("dependency"), nil); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, conn, child).Status != "todo" {
		t.Fatal("dependency block did not park in todo")
	}
	makeRunningAgain(t, conn, parent)
	if _, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil {
		t.Fatal(err)
	}
	if _, err := RecomputeReady(ctx, conn, nil); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, conn, child).Status != "ready" {
		t.Fatal("child not promoted")
	}
}

func TestWorkerBlockIsNotAutoPromotedByRecomputeReady(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "needs human review"})
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, tid, sp("review-required: please verify ACL change"), nil, mustTask(t, conn, tid).CurrentRunID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "blocked" {
		t.Fatal("not blocked")
	}
	for i := 0; i < 5; i++ {
		promoted, err := RecomputeReady(ctx, conn, nil)
		if err != nil || promoted != 0 || mustTask(t, conn, tid).Status != "blocked" {
			t.Fatal("worker-blocked task must not auto-promote", promoted, err)
		}
	}
}

func TestProtocolViolationLoopIsBroken(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "loop reproducer"})
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := BlockTaskWithKind(ctx, conn, tid, sp("review-required: human eyes please"), nil, mustTask(t, conn, tid).CurrentRunID); err != nil {
		t.Fatal(err)
	}
	if promoted, err := RecomputeReady(ctx, conn, nil); err != nil || promoted != 0 || mustTask(t, conn, tid).Status != "blocked" {
		t.Fatal("first tick promoted", promoted, err)
	}
	now := time.Now().Unix()
	mustExecArgs(t, conn, "INSERT INTO task_events (task_id, kind, payload, created_at) VALUES (?, 'protocol_violation', NULL, ?)", tid, now)
	mustExecArgs(t, conn, "INSERT INTO task_events (task_id, kind, payload, created_at) VALUES (?, 'gave_up', NULL, ?)", tid, now+1)
	for i := 0; i < 3; i++ {
		if promoted, err := RecomputeReady(ctx, conn, nil); err != nil || promoted != 0 || mustTask(t, conn, tid).Status != "blocked" {
			t.Fatal("later tick promoted", promoted, err)
		}
	}
}

// Executes one parameterized statement or fails the test
func mustExecArgs(t *testing.T, conn state.SQLiteConn, query string, args ...any) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestCreatedWithInitialStatusBlockedIsNotPromotedByRecomputeReady(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "parent task"})
	child := mustCreate(t, conn, CreateTaskOptions{Title: "gated child task", Parents: []string{parent}, InitialStatus: "blocked"})
	if mustTask(t, conn, child).Status != "blocked" {
		t.Fatal("child not blocked")
	}
	if _, err := ClaimTask(ctx, conn, parent, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, conn, parent).Status != "done" {
		t.Fatal("parent not done")
	}
	if promoted, err := RecomputeReady(ctx, conn, nil); err != nil || promoted != 0 || mustTask(t, conn, child).Status != "blocked" {
		t.Fatal("blocked child promoted", promoted, err)
	}
}
