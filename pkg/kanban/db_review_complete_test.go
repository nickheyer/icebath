package kanban

import (
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Last event of a kind on a task
func lastEventOfKind(t *testing.T, conn state.SQLiteConn, tid, kind string) *Event {
	t.Helper()
	events := eventsOfKind(t, conn, tid, kind)
	if len(events) == 0 {
		t.Fatal("no event of kind", kind)
	}
	return events[len(events)-1]
}

// Last run with an outcome on a task
func lastRunOfOutcome(t *testing.T, conn state.SQLiteConn, tid, outcome string) *Run {
	t.Helper()
	runs, err := ListRuns(context.Background(), conn, tid, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *Run
	for _, r := range runs {
		if r.Outcome != nil && *r.Outcome == outcome {
			found = r
		}
	}
	if found == nil {
		t.Fatal("no run with outcome", outcome)
	}
	return found
}

// Task handed to a claimed reviewer run
func claimedReview(t *testing.T, conn state.SQLiteConn, title string, ttlSeconds, maxRuntime *int64) (string, *Task) {
	t.Helper()
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: title, Assignee: sp("builder"), MaxRuntimeSeconds: maxRuntime})
	implementation := mustClaim(t, conn, tid, sp("builder:test"))
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("ready for independent review"), Reviewer: sp("reviewer"), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	rev, err := ClaimReviewTask(ctx, conn, tid, ttlSeconds, nil)
	if err != nil || rev == nil {
		t.Fatal("review claim", err)
	}
	return tid, rev
}

// Consecutive failures of a task
func failuresOf(t *testing.T, conn state.SQLiteConn, tid string) int64 {
	t.Helper()
	return mustTask(t, conn, tid).ConsecutiveFailures
}

func TestSameCardReviewSupportsChangesAndApprovalWithoutBlockLoop(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "Implement guarded export", Assignee: sp("builder")})
	implementation := mustClaim(t, conn, tid, sp("builder:1"))
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Reviewer: sp("reviewer"), Summary: sp("Implementation and focused tests are ready."),
		Metadata: pyjson.ObjectOf("commit", "abc123"), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	awaiting := mustTask(t, conn, tid)
	if awaiting.Status != "review" || awaiting.Assignee == nil || *awaiting.Assignee != "reviewer" || awaiting.CurrentRunID != nil {
		t.Fatal(awaiting.Status, awaiting.Assignee, awaiting.CurrentRunID)
	}
	requested := payloadOf(lastEventOfKind(t, conn, tid, "review_requested"))
	impl, _ := requested.Get("implementer")
	rev, _ := requested.Get("reviewer")
	sum, _ := requested.Get("summary")
	if impl != "builder" || rev != "reviewer" || sum != "Implementation and focused tests are ready." {
		t.Fatal(requested)
	}
	implRun := lastRunOfOutcome(t, conn, tid, "review_requested")
	if implRun.Summary == nil || *implRun.Summary != "Implementation and focused tests are ready." || !pyjson.Equal(implRun.Metadata, pyjson.ObjectOf("commit", "abc123")) {
		t.Fatal(implRun.Summary, implRun.Metadata)
	}
	rv, err := ClaimReviewTask(ctx, conn, tid, nil, sp("reviewer:1"))
	if err != nil || rv == nil {
		t.Fatal("review claim", err)
	}
	ok, who, err := RequestChanges(ctx, conn, tid, "Add a regression for the fallback branch.", rv.CurrentRunID)
	if err != nil || !ok || who != "builder" {
		t.Fatal(ok, who, err)
	}
	rework := mustTask(t, conn, tid)
	if rework.Status != "ready" || rework.Assignee == nil || *rework.Assignee != "builder" || rework.CurrentRunID != nil {
		t.Fatal(rework.Status, rework.Assignee)
	}
	changes := payloadOf(lastEventOfKind(t, conn, tid, "changes_requested"))
	reason, _ := changes.Get("reason")
	impl2, _ := changes.Get("implementer")
	rev2, _ := changes.Get("reviewer")
	if reason != "Add a regression for the fallback branch." || impl2 != "builder" || rev2 != "reviewer" {
		t.Fatal(changes)
	}
	lastRunOfOutcome(t, conn, tid, "changes_requested")
	implementation2 := mustClaim(t, conn, tid, sp("builder:2"))
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("Fallback regression added."), ExpectedRunID: implementation2.CurrentRunID}); !ok {
		t.Fatal("re-review refused")
	}
	rereview := mustTask(t, conn, tid)
	if rereview.Status != "review" || rereview.Assignee == nil || *rereview.Assignee != "reviewer" {
		t.Fatal(rereview.Status, rereview.Assignee)
	}
	review2, err := ClaimReviewTask(ctx, conn, tid, nil, sp("reviewer:2"))
	if err != nil || review2 == nil || review2.Assignee == nil || *review2.Assignee != "reviewer" {
		t.Fatal("second review claim", err)
	}
	reviewRun, err := LatestRun(ctx, conn, tid)
	if err != nil || reviewRun == nil || reviewRun.Profile == nil || *reviewRun.Profile != "reviewer" {
		t.Fatal("review run", err)
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{Summary: sp("Approved after independent verification."), ExpectedRunID: review2.CurrentRunID}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	completed := mustTask(t, conn, tid)
	if completed.Status != "done" || completed.BlockRecurrences != 0 {
		t.Fatal(completed.Status, completed.BlockRecurrences)
	}
}

func TestRereviewRequiresExplicitReviewerWhenProvenanceIsInvalid(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{{"None", ""}, {"not-json", "{not-json"}, {"empty", "{}"}} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := openPathBoard(t)
			ctx := context.Background()
			tid, rv := claimedReview(t, conn, "Malformed reviewer provenance", nil, nil)
			ok, who, err := RequestChanges(ctx, conn, tid, "Correct the implementation.", rv.CurrentRunID)
			if err != nil || !ok || who != "builder" {
				t.Fatal(ok, who, err)
			}
			if err := WriteTxn(ctx, conn, false, func() error {
				if tc.payload == "" {
					_, err := conn.ExecContext(ctx, "DELETE FROM task_events WHERE task_id = ? AND kind = 'changes_requested'", tid)
					return err
				}
				_, err := conn.ExecContext(ctx, "UPDATE task_events SET payload = ? WHERE id = (SELECT id FROM task_events WHERE task_id = ? AND kind = 'changes_requested' ORDER BY id DESC LIMIT 1)", tc.payload, tid)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			implementation := mustClaim(t, conn, tid, sp("builder:retry"))
			if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("Corrected implementation."), ExpectedRunID: implementation.CurrentRunID}); ok {
				t.Fatal("re-review accepted without reviewer")
			}
			unchanged := mustTask(t, conn, tid)
			if unchanged.Status != "running" || unchanged.Assignee == nil || *unchanged.Assignee != "builder" {
				t.Fatal(unchanged.Status, unchanged.Assignee)
			}
			if ok, _ := review(t, conn, tid, RequestReviewOptions{Reviewer: sp("reviewer"), Summary: sp("Corrected implementation."), ExpectedRunID: implementation.CurrentRunID}); !ok {
				t.Fatal("explicit reviewer refused")
			}
			restored := mustTask(t, conn, tid)
			if restored.Status != "review" || restored.Assignee == nil || *restored.Assignee != "reviewer" {
				t.Fatal(restored.Status, restored.Assignee)
			}
		})
	}
}

func TestReviewChangesReapplyParentGate(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "Upstream prerequisite", Assignee: sp("planner")})
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "Dependent implementation", Assignee: sp("builder"), Parents: []string{parent}})
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	implementation := mustClaim(t, conn, tid, sp("builder:1"))
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Reviewer: sp("reviewer"), Summary: sp("Ready for review."), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	rv, err := ClaimReviewTask(ctx, conn, tid, nil, sp("reviewer:1"))
	if err != nil || rv == nil {
		t.Fatal("review claim", err)
	}
	mustExecArgs(t, conn, "UPDATE tasks SET status = 'ready' WHERE id = ?", parent)
	ok, who, err := RequestChanges(ctx, conn, tid, "Parent contract changed; rework after it lands.", rv.CurrentRunID)
	if err != nil || !ok || who != "builder" {
		t.Fatal(ok, who, err)
	}
	if mustTask(t, conn, tid).Status != "todo" {
		t.Fatal("not re-gated")
	}
}

func TestParentReopenBlocksRequestReviewUntilParentIsDone(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "Parent", Assignee: sp("planner")})
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "Implementation with reopened parent", Assignee: sp("builder"), Parents: []string{parent}})
	implementation := mustClaim(t, conn, tid, nil)
	mustExecArgs(t, conn, "UPDATE tasks SET status = 'ready' WHERE id = ?", parent)
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("must wait"), ExpectedRunID: implementation.CurrentRunID}); ok {
		t.Fatal("review accepted under a reopened parent")
	}
	if mustTask(t, conn, tid).Status != "running" {
		t.Fatal("status")
	}
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("parent stable"), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused after parent done")
	}
}

func TestRequestChangesFailsClosedOnMalformedReviewProvenance(t *testing.T) {
	for _, bad := range []string{"{not-json", "[]"} {
		t.Run(bad, func(t *testing.T) {
			conn, _ := openPathBoard(t)
			ctx := context.Background()
			tid := mustCreate(t, conn, CreateTaskOptions{Title: "Malformed handoff", Assignee: sp("builder")})
			implementation := mustClaim(t, conn, tid, sp("builder:1"))
			if ok, _ := review(t, conn, tid, RequestReviewOptions{Reviewer: sp("reviewer"), Summary: sp("Ready."), ExpectedRunID: implementation.CurrentRunID}); !ok {
				t.Fatal("review refused")
			}
			mustExecArgs(t, conn, "UPDATE task_events SET payload = ? WHERE task_id = ? AND kind = 'review_requested'", bad, tid)
			rv, err := ClaimReviewTask(ctx, conn, tid, nil, sp("reviewer:1"))
			if err != nil || rv == nil {
				t.Fatal("review claim", err)
			}
			ok, detail, err := RequestChanges(ctx, conn, tid, "Needs changes.", rv.CurrentRunID)
			if err != nil || ok || !strings.Contains(detail, "implementer provenance") {
				t.Fatal(ok, detail, err)
			}
			task := mustTask(t, conn, tid)
			if task.Status != "running" || task.Assignee == nil || *task.Assignee != "reviewer" || task.CurrentRunID == nil || *task.CurrentRunID != *rv.CurrentRunID {
				t.Fatal(task.Status, task.Assignee, task.CurrentRunID)
			}
		})
	}
}

func TestReclaimFailsSafeOnNonObjectClaimProvenance(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid, _ := claimedReview(t, conn, "Non-object claimed payload", nil, nil)
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE task_events SET payload = '[]' WHERE task_id = ? AND kind = 'claimed' AND run_id = (SELECT current_run_id FROM tasks WHERE id = ?)", tid, tid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := ReclaimTask(ctx, conn, tid, nil, func(int, syscall.Signal) error { return nil }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "ready" {
		t.Fatal("status")
	}
}

func TestReviewRetryStillTripsTheFailureBreaker(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid, _ := claimedReview(t, conn, "Reviewer repeatedly fails", nil, nil)
	tripped, err := recordTaskFailure(ctx, conn, tid, "reviewer cannot start", RecordFailureOptions{Outcome: "spawn_failed", FailureLimit: i64ptr(1), ReleaseClaim: true, EndRun: true})
	if err != nil || !tripped {
		t.Fatal(tripped, err)
	}
	if mustTask(t, conn, tid).Status != "blocked" {
		t.Fatal("not blocked")
	}
	if retry, _ := payloadOf(lastEventOfKind(t, conn, tid, "gave_up")).Get("retry_status"); retry != "review" {
		t.Fatal(retry)
	}
	if ok, err := UnblockTask(ctx, conn, tid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("unblock left the review lane")
	}
}

func TestReviewEscalationUnblocksBackToReview(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid, rv := claimedReview(t, conn, "External review escalation", nil, nil)
	if ok, err := BlockTaskWithKind(ctx, conn, tid, sp("needs_input: maintainer decision required"), sp("needs_input"), rv.CurrentRunID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if src, _ := payloadOf(lastEventOfKind(t, conn, tid, "blocked")).Get("source_status"); src != "review" {
		t.Fatal(src)
	}
	if ok, err := UnblockTask(ctx, conn, tid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("resumed status")
	}
}

func TestReviewDependencyWaitReentersReviewAfterParentFinishes(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "Parent", Assignee: sp("planner")})
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "Review after dependency refresh", Assignee: sp("builder"), Parents: []string{parent}})
	implementation := mustClaim(t, conn, tid, nil)
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("ready"), Reviewer: sp("reviewer"), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	rv, err := ClaimReviewTask(ctx, conn, tid, nil, nil)
	if err != nil || rv == nil {
		t.Fatal("review claim", err)
	}
	if err := WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready' WHERE id = ?", parent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := BlockTaskWithKind(ctx, conn, tid, sp("dependency: parent contract is being refreshed"), sp("dependency"), rv.CurrentRunID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "todo" {
		t.Fatal("not waiting")
	}
	if ok, err := CompleteTask(ctx, conn, parent, CompleteTaskOptions{Result: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if mustTask(t, conn, tid).Status != "review" {
		t.Fatal("did not re-enter review")
	}
}

func TestGoalRunStatusIsBoundToOriginalRun(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "Goal handoff race", Assignee: sp("builder")})
	implementation := mustClaim(t, conn, tid, nil)
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("ready"), Reviewer: sp("reviewer"), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	rv, err := ClaimReviewTask(ctx, conn, tid, nil, nil)
	if err != nil || rv == nil {
		t.Fatal("review claim", err)
	}
	status := func(runID *int64) string {
		s, err := GoalRunStatus(ctx, conn, tid, runID)
		if err != nil || s == nil {
			t.Fatal("goal run status", err)
		}
		return *s
	}
	if status(implementation.CurrentRunID) != "review" {
		t.Fatal("implementation run status")
	}
	ok, who, err := RequestChanges(ctx, conn, tid, "fix it", rv.CurrentRunID)
	if err != nil || !ok || who != "builder" {
		t.Fatal(ok, who, err)
	}
	successor := mustClaim(t, conn, tid, nil)
	if status(rv.CurrentRunID) != "changes_requested" || status(successor.CurrentRunID) != "running" {
		t.Fatal("run statuses")
	}
	if ok, err := BlockTaskWithKind(ctx, conn, tid, sp("stale reviewer must not block successor"), nil, rv.CurrentRunID); err != nil || ok {
		t.Fatal("stale reviewer blocked", ok, err)
	}
	current := mustTask(t, conn, tid)
	if current.Status != "running" || current.CurrentRunID == nil || *current.CurrentRunID != *successor.CurrentRunID {
		t.Fatal(current.Status, current.CurrentRunID)
	}
}

func TestParkedReviewApprovalWithoutEvidenceStillCreatesAuditRun(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "Manual approval", Assignee: sp("reviewer")})
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("implementation handoff")}); !ok {
		t.Fatal("review refused")
	}
	if ok, err := CompleteTask(ctx, conn, tid, CompleteTaskOptions{}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	completed := lastEventOfKind(t, conn, tid, "completed")
	if completed.RunID == nil {
		t.Fatal("completed event without run")
	}
	run, err := LatestRun(ctx, conn, tid)
	if err != nil || run == nil || run.ID != *completed.RunID || run.Outcome == nil || *run.Outcome != "completed" || run.Profile == nil || *run.Profile != "reviewer" ||
		run.Summary == nil || *run.Summary != "Review approved without additional evidence." || !pyjson.Equal(run.Metadata, pyjson.ObjectOf("source_status", "review", "approval", "manual")) {
		t.Fatal(run, err)
	}
}

func TestReviewTransitionsPreserveConsecutiveFailures(t *testing.T) {
	conn, _ := openPathBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "flaky feature", Assignee: sp("builder")})
	mustExecArgs(t, conn, "UPDATE tasks SET consecutive_failures = 1 WHERE id = ?", tid)
	implementation := mustClaim(t, conn, tid, sp("builder:1"))
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("v1"), Reviewer: sp("reviewer"), ExpectedRunID: implementation.CurrentRunID}); !ok {
		t.Fatal("review refused")
	}
	if failuresOf(t, conn, tid) != 1 {
		t.Fatal("request_review changed the counter")
	}
	rv, err := ClaimReviewTask(ctx, conn, tid, nil, nil)
	if err != nil || rv == nil {
		t.Fatal("review claim", err)
	}
	if ok, who, err := RequestChanges(ctx, conn, tid, "needs fixes", rv.CurrentRunID); err != nil || !ok || who != "builder" {
		t.Fatal(ok, who, err)
	}
	if failuresOf(t, conn, tid) != 1 {
		t.Fatal("request_changes changed the counter")
	}
	retry := mustClaim(t, conn, tid, sp("builder:2"))
	if ok, _ := review(t, conn, tid, RequestReviewOptions{Summary: sp("v2"), ExpectedRunID: retry.CurrentRunID}); !ok {
		t.Fatal("re-review refused")
	}
	if failuresOf(t, conn, tid) != 1 {
		t.Fatal("re-review changed the counter")
	}
	if ok, err := ReopenReviewTask(ctx, conn, tid); err != nil || !ok || failuresOf(t, conn, tid) != 1 {
		t.Fatal("reopen", ok, err)
	}
	tripped, err := recordTaskFailure(ctx, conn, tid, "worker crashed", RecordFailureOptions{Outcome: "crashed", FailureLimit: i64ptr(2)})
	if err != nil || !tripped || failuresOf(t, conn, tid) != 2 || mustTask(t, conn, tid).Status != "blocked" {
		t.Fatal(tripped, err)
	}
	okID := mustCreate(t, conn, CreateTaskOptions{Title: "healthy", Assignee: sp("builder")})
	mustExecArgs(t, conn, "UPDATE tasks SET consecutive_failures = 1 WHERE id = ?", okID)
	if ok, err := CompleteTask(ctx, conn, okID, CompleteTaskOptions{Summary: sp("done")}); err != nil || !ok || failuresOf(t, conn, okID) != 0 {
		t.Fatal("completion did not reset", ok, err)
	}
}
