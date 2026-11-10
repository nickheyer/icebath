package kanban

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Status, block kind, recurrences and current run of a task row
func taskRow(t *testing.T, conn state.SQLiteConn, tid string) *Row {
	t.Helper()
	row, err := QueryOne(context.Background(), conn, "SELECT status, block_kind, block_recurrences, current_run_id FROM tasks WHERE id = ?", tid)
	if err != nil || row == nil {
		t.Fatal("task row", err)
	}
	return row
}

// Newest run row of a task with status, outcome and summary
func lastRunRow(t *testing.T, conn state.SQLiteConn, tid string) *Row {
	t.Helper()
	row, err := QueryOne(context.Background(), conn, "SELECT status, outcome, summary, profile, step_key FROM task_runs WHERE task_id = ? ORDER BY id DESC LIMIT 1", tid)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// Claims a task or fails the test
func mustClaim(t *testing.T, conn state.SQLiteConn, tid string, claimer *string) *Task {
	t.Helper()
	claimed, err := ClaimTask(context.Background(), conn, tid, nil, claimer)
	if err != nil || claimed == nil {
		t.Fatal("claim", tid, err)
	}
	return claimed
}

// Requests review or fails the test, returning ok and reason
func review(t *testing.T, conn state.SQLiteConn, tid string, o RequestReviewOptions) (bool, string) {
	t.Helper()
	ok, reason, err := RequestReview(context.Background(), conn, tid, o)
	if err != nil {
		t.Fatal(err)
	}
	return ok, reason
}

func TestRequestReviewTransitionsRunningToReview(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "impl a feature", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	runID := mustTask(t, conn, tid).CurrentRunID
	if runID == nil {
		t.Fatal("no run")
	}
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("Implementation complete\nfull details below"), Reviewer: sp("reviewer"), ExpectedRunID: runID}); !ok {
		t.Fatal("review refused")
	}
	row := taskRow(t, conn, tid)
	if row.Get("status") != "review" || row.Get("current_run_id") != nil || cellInt(row.Get("block_recurrences")) != 0 || row.Get("block_kind") != nil {
		t.Fatal(row.Vals)
	}
	run := lastRunRow(t, conn, tid)
	if run.Get("outcome") != "review_requested" || run.Get("status") != "review" {
		t.Fatal(run.Vals)
	}
	rr := eventsOfKind(t, conn, tid, "review_requested")
	if len(rr) != 1 {
		t.Fatal("review_requested events", len(rr))
	}
	p := payloadOf(rr[0])
	impl, _ := p.Get("implementer")
	rev, _ := p.Get("reviewer")
	sum, _ := p.Get("summary")
	if impl != "worker" || rev != "reviewer" || sum != "Implementation complete" {
		t.Fatal(p)
	}
	if len(eventsOfKind(t, conn, tid, "blocked")) != 0 || len(eventsOfKind(t, conn, tid, "block_loop_detected")) != 0 {
		t.Fatal("review recorded as a block")
	}
}

func TestRepeatedReviewRequestsNeverTriage(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "cycle me", Assignee: sp("worker")})
	for i := 0; i < 4; i++ {
		task := mustTask(t, conn, tid)
		if task.Status == "ready" {
			mustClaim(t, conn, tid, nil)
		} else {
			if task.Status != "review" {
				t.Fatal("status", task.Status)
			}
			if claimed, err := ClaimReviewTask(ctx, conn, tid, nil, nil); err != nil || claimed == nil {
				t.Fatal("review claim", err)
			}
		}
		if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("pass complete"), ExpectedRunID: mustTask(t, conn, tid).CurrentRunID}); !ok {
			t.Fatal("review refused")
		}
		row := taskRow(t, conn, tid)
		if row.Get("status") != "review" || cellInt(row.Get("block_recurrences")) != 0 {
			t.Fatal("must never leave the review lane", row.Vals)
		}
	}
	if taskRow(t, conn, tid).Get("status") != "review" || len(eventsOfKind(t, conn, tid, "block_loop_detected")) != 0 || len(eventsOfKind(t, conn, tid, "review_requested")) != 4 {
		t.Fatal("cycle")
	}
}

func TestRequestReviewExpectedRunIdMismatchIsNoop(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "stale worker", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	realRun := mustTask(t, conn, tid).CurrentRunID
	stale := *realRun + 999
	if ok, _ := review(t, conn, tid, RequestReviewOptions{ExpectedRunID: &stale}); ok {
		t.Fatal("stale run moved the task")
	}
	row := taskRow(t, conn, tid)
	if row.Get("status") != "running" || cellInt(row.Get("current_run_id")) != *realRun || len(eventsOfKind(t, conn, tid, "review_requested")) != 0 {
		t.Fatal(row.Vals)
	}
}

func TestRequestReviewUnknownTaskReturnsFalse(t *testing.T) {
	conn := openTestBoard(t)
	if ok, _ := review(t, conn, "t_deadbeefcafe", RequestReviewOptions{}); ok {
		t.Fatal("unknown task reviewed")
	}
}

func TestRequestReviewRefusesToClearLiveClaimWithoutOwnership(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "live claim", Assignee: sp("worker")})
	claimed := mustClaim(t, conn, tid, nil)
	if err := setWorkerPid(ctx, conn, tid, os.Getpid(), nil, nil); err != nil {
		t.Fatal(err)
	}
	ok, reason := review(t, conn, tid, RequestReviewOptions{})
	if ok || !strings.Contains(reason, "live claim") {
		t.Fatal(ok, reason)
	}
	row, err := QueryOne(ctx, conn, "SELECT status, claim_lock, current_run_id FROM tasks WHERE id = ?", tid)
	if err != nil || row.Get("status") != "running" || row.Get("claim_lock") == nil {
		t.Fatal("live claim touched", err, row.Vals)
	}
	if ok, _ := review(t, conn, tid, RequestReviewOptions{}); ok {
		t.Fatal("second claimless review succeeded")
	}
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("done"), ExpectedRunID: claimed.CurrentRunID}); !ok || mustTask(t, conn, tid).Status != "review" {
		t.Fatal("owner review refused")
	}
	tid2 := mustCreate(t, conn, CreateTaskOptions{Title: "forced", Assignee: sp("worker")})
	mustClaim(t, conn, tid2, nil)
	if ok, _ := review(t, conn, tid2, RequestReviewOptions{Summary: sp("override"), Force: true}); !ok || mustTask(t, conn, tid2).Status != "review" {
		t.Fatal("forced review refused")
	}
}

func TestRequestReviewMalformedProvenanceGetsDistinctReason(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "provenance", Assignee: sp("builder")})
	claimed := mustClaim(t, conn, tid, nil)
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("v1"), Reviewer: sp("reviewer"), ExpectedRunID: claimed.CurrentRunID}); !ok {
		t.Fatal("v1 review refused")
	}
	reviewRun, err := ClaimReviewTask(ctx, conn, tid, nil, nil)
	if err != nil || reviewRun == nil {
		t.Fatal("review claim", err)
	}
	ok, impl, err := RequestChanges(ctx, conn, tid, "fix", reviewRun.CurrentRunID)
	if err != nil || !ok || impl != "builder" {
		t.Fatal(ok, impl, err)
	}
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE task_events SET payload = '{\"reviewer\": 42}' WHERE task_id = ? AND kind = 'changes_requested'", tid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retry := mustClaim(t, conn, tid, sp("builder:retry"))
	ok, reason := review(t, conn, tid, RequestReviewOptions{Summary: sp("v2"), ExpectedRunID: retry.CurrentRunID})
	if ok || !strings.Contains(reason, "provenance") {
		t.Fatal(ok, reason)
	}
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("v2"), Reviewer: sp("reviewer"), ExpectedRunID: retry.CurrentRunID}); !ok {
		t.Fatal("explicit reviewer refused")
	}
}

func TestRequestReviewWhitespaceOnlySummaryDoesNotCrash(t *testing.T) {
	for _, blank := range []string{"   ", "\n", "\t\n  "} {
		t.Run(blank, func(t *testing.T) {
			conn := openTestBoard(t)
			tid := mustCreate(t, conn, CreateTaskOptions{Title: "blank summary", Assignee: sp("worker")})
			mustClaim(t, conn, tid, nil)
			if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp(blank), ExpectedRunID: mustTask(t, conn, tid).CurrentRunID}); !ok {
				t.Fatal("review refused")
			}
			if mustTask(t, conn, tid).Status != "review" {
				t.Fatal("status")
			}
			rr := eventsOfKind(t, conn, tid, "review_requested")
			if sum, _ := payloadOf(rr[0]).Get("summary"); len(rr) != 1 || sum != nil {
				t.Fatal(rr)
			}
		})
	}
}

func TestCompleteTaskClosesReviewToDone(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "approve me", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	review(t, conn, tid, RequestReviewOptions{Summary: sp("ready"), ExpectedRunID: mustTask(t, conn, tid).CurrentRunID})
	if task := mustTask(t, conn, tid); task.Status != "review" || task.CurrentRunID != nil {
		t.Fatal(task.Status, task.CurrentRunID)
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Summary: sp("LGTM — merged"), Result: sp("approved")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "done" || len(eventsOfKind(t, conn, tid, "completed")) == 0 {
		t.Fatal("not done")
	}
}

func TestReopenReviewTaskReturnsToReady(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "reopen me", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	review(t, conn, tid, RequestReviewOptions{Summary: sp("v1"), Reviewer: sp("reviewer"), ExpectedRunID: mustTask(t, conn, tid).CurrentRunID})
	reviewing := mustTask(t, conn, tid)
	if reviewing.Status != "review" || reviewing.Assignee == nil || *reviewing.Assignee != "reviewer" {
		t.Fatal(reviewing.Status, reviewing.Assignee)
	}
	if ok, err := ReopenReviewTask(ctx, conn, tid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	row := taskRow(t, conn, tid)
	reopened := mustTask(t, conn, tid)
	if row.Get("status") != "ready" || reopened.Assignee == nil || *reopened.Assignee != "worker" || row.Get("current_run_id") != nil || cellInt(row.Get("block_recurrences")) != 0 {
		t.Fatal(row.Vals, reopened.Assignee)
	}
	if len(eventsOfKind(t, conn, tid, "review_reopened")) == 0 {
		t.Fatal("no review_reopened event")
	}
	if ok, err := ReopenReviewTask(ctx, conn, tid); err != nil || ok {
		t.Fatal("second reopen", ok, err)
	}
}

func TestReviewCycleEndToEnd(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "cycle", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	review(t, conn, tid, RequestReviewOptions{Summary: sp("v1"), ExpectedRunID: mustTask(t, conn, tid).CurrentRunID})
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("v1")
	}
	if ok, err := ReopenReviewTask(ctx, conn, tid); err != nil || !ok || mustTask(t, conn, tid).Status != "ready" {
		t.Fatal("reopen", ok, err)
	}
	mustClaim(t, conn, tid, nil)
	review(t, conn, tid, RequestReviewOptions{Summary: sp("v2"), ExpectedRunID: mustTask(t, conn, tid).CurrentRunID})
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("v2")
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Summary: sp("approved")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	row := taskRow(t, conn, tid)
	if row.Get("status") != "done" || cellInt(row.Get("block_recurrences")) != 0 || len(eventsOfKind(t, conn, tid, "block_loop_detected")) != 0 {
		t.Fatal(row.Vals)
	}
}

func TestRequestReviewOnUnclaimedReadySynthesizesRun(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "ready then review", Assignee: sp("worker")})
	if task := mustTask(t, conn, tid); task.Status != "ready" || task.CurrentRunID != nil {
		t.Fatal("precondition")
	}
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("done without a claim")}); !ok || mustTask(t, conn, tid).Status != "review" {
		t.Fatal("review refused")
	}
	run := lastRunRow(t, conn, tid)
	if run == nil || run.Get("outcome") != "review_requested" || run.Get("summary") != "done without a claim" {
		t.Fatal(run)
	}
	evs := eventsOfKind(t, conn, tid, "review_requested")
	if sum, _ := payloadOf(evs[0]).Get("summary"); len(evs) != 1 || sum != "done without a claim" {
		t.Fatal(evs)
	}
}

func TestReviewerReassignsForAutonomousDispatch(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "route reviewer", Assignee: sp("worker")})
	claimed := mustClaim(t, conn, tid, nil)
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("v1"), Reviewer: sp("lead-reviewer"), ExpectedRunID: claimed.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	if a := mustTask(t, conn, tid).Assignee; a == nil || *a != "lead-reviewer" {
		t.Fatal("assignee", a)
	}
	ev := payloadOf(eventsOfKind(t, conn, tid, "review_requested")[0])
	rev, _ := ev.Get("reviewer")
	impl, _ := ev.Get("implementer")
	if rev != "lead-reviewer" || impl != "worker" {
		t.Fatal(ev)
	}
}

func TestReviewHandoffWithoutLiveRunAttributesRunToImplementer(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "handoff attribution", Assignee: sp("worker")})
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("ready"), Reviewer: sp("lead-reviewer")}); !ok {
		t.Fatal("review refused")
	}
	task := mustTask(t, conn, tid)
	if task.Assignee == nil || *task.Assignee != "lead-reviewer" {
		t.Fatal("assignee")
	}
	run := lastRunRow(t, conn, tid)
	if run.Get("outcome") != "review_requested" || run.Get("profile") != "worker" || !pyjson.Equal(run.Get("step_key"), optStrValue(task.CurrentStepKey)) {
		t.Fatal(run.Vals)
	}
	if impl, _ := payloadOf(eventsOfKind(t, conn, tid, "review_requested")[0]).Get("implementer"); impl != "worker" {
		t.Fatal(impl)
	}
}

func TestReviewHandoffOfCardAssignedToItsReviewerRecordsNoImplementer(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "already applied", Assignee: sp("reviewer-a")})
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("review this"), Reviewer: sp("reviewer-a")}); !ok {
		t.Fatal("review refused")
	}
	ev := payloadOf(eventsOfKind(t, conn, tid, "review_requested")[0])
	rev, _ := ev.Get("reviewer")
	impl, _ := ev.Get("implementer")
	if rev != "reviewer-a" || impl != nil {
		t.Fatal(ev)
	}
	run := lastRunRow(t, conn, tid)
	if run.Get("outcome") != "review_requested" || run.Get("profile") != nil {
		t.Fatal(run.Vals)
	}
	if claimed, err := ClaimReviewTask(ctx, conn, tid, nil, sp("reviewer-a")); err != nil || claimed == nil {
		t.Fatal("review claim", err)
	}
	ok, reason, err := RequestChanges(ctx, conn, tid, "found 3 issues", nil)
	if err != nil || ok || !strings.Contains(reason, "implementer provenance") {
		t.Fatal(ok, reason, err)
	}
}

func TestSynthesizedRunForUnassignedCardKeepsNullProfile(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "unassigned handoff"})
	if ok, err := BlockTaskWithKind(context.Background(), conn, tid, sp("waiting on upstream"), nil, nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	run := lastRunRow(t, conn, tid)
	if run.Get("outcome") != "blocked" || run.Get("profile") != nil {
		t.Fatal(run.Vals)
	}
}

func TestClaimTaskRecoversFromInvariantLeak(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "invariant test", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	leaked, err := LatestRun(ctx, conn, tid)
	if err != nil || leaked == nil {
		t.Fatal(err)
	}
	mustExecArgs(t, conn, "UPDATE tasks SET status = 'ready', claim_lock = NULL, claim_expires = NULL WHERE id = ?", tid)
	if run, err := GetRun(ctx, conn, leaked.ID); err != nil || run.EndedAt != nil {
		t.Fatal("leaked run closed early", err)
	}
	mustClaim(t, conn, tid, nil)
	run, err := GetRun(ctx, conn, leaked.ID)
	if err != nil || run.EndedAt == nil || run.Outcome == nil || *run.Outcome != "reclaimed" {
		t.Fatal("leak not recovered", err)
	}
	newRun, err := LatestRun(ctx, conn, tid)
	if err != nil || newRun.ID == leaked.ID || newRun.EndedAt != nil {
		t.Fatal("new run", err)
	}
}

func TestUnblockInvariantRecovery(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "unblock invariant", Assignee: sp("worker")})
	mustClaim(t, conn, tid, nil)
	leaked, _ := LatestRun(ctx, conn, tid)
	mustExecArgs(t, conn, "UPDATE tasks SET status = 'blocked' WHERE id = ?", tid)
	if task := mustTask(t, conn, tid); task.CurrentRunID == nil || *task.CurrentRunID != leaked.ID {
		t.Fatal("precondition")
	}
	if run, _ := GetRun(ctx, conn, leaked.ID); run.EndedAt != nil {
		t.Fatal("precondition run")
	}
	if ok, err := UnblockTask(ctx, conn, tid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	task := mustTask(t, conn, tid)
	if task.Status != "ready" || task.CurrentRunID != nil {
		t.Fatal(task.Status, task.CurrentRunID)
	}
	run, _ := GetRun(ctx, conn, leaked.ID)
	if run.Outcome == nil || *run.Outcome != "reclaimed" || run.EndedAt == nil {
		t.Fatal("leak not closed")
	}
}

func TestCompleteCanRetryAfterPhantomRejection(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	parentA := mustCreate(t, conn, CreateTaskOptions{Title: "retry-empty", Assignee: sp("alice")})
	mustClaim(t, conn, parentA, nil)
	parentB := mustCreate(t, conn, CreateTaskOptions{Title: "retry-corrected", Assignee: sp("alice")})
	mustClaim(t, conn, parentB, nil)
	real := mustCreate(t, conn, CreateTaskOptions{Title: "real-child", Assignee: sp("x"), CreatedBy: sp("alice")})
	expectPhantom := func(tid string, cards []string) {
		t.Helper()
		_, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Summary: sp("oops"), CreatedCards: cards})
		var halluc *HallucinatedCardsError
		if !errors.As(err, &halluc) {
			t.Fatal("expected HallucinatedCardsError", err)
		}
		if mustTask(t, conn, tid).Status != "running" {
			t.Fatal("status moved")
		}
	}
	expectPhantom(parentA, []string{"t_phantomdeadbeef"})
	if ok, err := CompleteTask(ctx, conn, parentA, CompleteTaskOptions{Summary: sp("retry without claims"), CreatedCards: []string{}}); err != nil || !ok || mustTask(t, conn, parentA).Status != "done" {
		t.Fatal(ok, err)
	}
	expectPhantom(parentB, []string{real, "t_anotherphantom"})
	if ok, err := CompleteTask(ctx, conn, parentB, CompleteTaskOptions{Summary: sp("retry with corrected list"), CreatedCards: []string{real}}); err != nil || !ok || mustTask(t, conn, parentB).Status != "done" {
		t.Fatal(ok, err)
	}
	for _, parent := range []string{parentA, parentB} {
		kinds := eventKinds(t, conn, parent)
		if countKind(kinds, "completion_blocked_hallucination") != 1 || countKind(kinds, "completed") != 1 {
			t.Fatal(kinds)
		}
	}
}

func TestNotifySubStartsCaughtUpOnActiveTask(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "old task", Assignee: sp("w")})
	if _, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Result: sp("done long ago")}); err != nil {
		t.Fatal(err)
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "123"})
	if cellInt(subField(subsOf(t, conn, tid)[0], "last_event_id")) <= 0 {
		t.Fatal("cursor must snap to MAX(task_events.id) at subscription time")
	}
	_, events, err := UnseenEventsForSub(ctx, conn, tid, "telegram", "123", nil, []string{"completed", "blocked", "gave_up", "crashed", "timed_out"})
	if err != nil || len(events) != 0 {
		t.Fatal("historical events must not replay to a new sub", len(events), err)
	}
}
