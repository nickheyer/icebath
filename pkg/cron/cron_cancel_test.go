package cron

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Claims and starts one attempt of the job in the store ledger
func claimRunning(t *testing.T, s *Store, jobID string) map[string]any {
	t.Helper()
	l := openExecutionsLedger(s)
	record := mustLedger(t)(l.Create(context.Background(), jobID, "test", nil))
	mustLedger(t)(l.MarkRunning(context.Background(), recText(record, "id")))
	return record
}

func mustRequestCancel(t *testing.T, s *Store, executionID string, reason *string) {
	t.Helper()
	if _, err := s.requestCancel(executionID, reason); err != nil {
		t.Fatal(err)
	}
}

func cancelOutcome(t *testing.T, s *Store, jobID, reason string) map[string]any {
	t.Helper()
	res, err := cancelRun(s, jobID, reason)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, k := range res.Keys {
		out[k] = res.Vals[k]
	}
	return out
}

func TestMarkerDrivesTheRunsCancelView(t *testing.T) {
	s, _ := newTestStore(t)
	request := s.newCancelRequest("exec-a")
	if request.IsSet() {
		t.Fatal("unrequested cancel is set")
	}
	mustRequestCancel(t, s, "exec-a", strPtr("operator asked"))
	if !request.IsSet() || !s.cancelRequested("exec-a") {
		t.Fatal("requested cancel not seen")
	}
	if got := request.Reason(); got != "operator asked" {
		t.Fatalf("reason %q", got)
	}
	s.clearCancel("exec-a")
	if request.IsSet() {
		t.Fatal("cleared cancel still set")
	}
}

func TestAMarkerOnlyEverReachesItsOwnExecution(t *testing.T) {
	s, _ := newTestStore(t)
	finished, upcoming := s.newCancelRequest("exec-old"), s.newCancelRequest("exec-new")
	mustRequestCancel(t, s, "exec-old", nil)
	if !finished.IsSet() || upcoming.IsSet() {
		t.Fatalf("finished %v upcoming %v", finished.IsSet(), upcoming.IsSet())
	}
}

func TestCancelRunTargetsTheLiveExecution(t *testing.T) {
	s, _ := newTestStore(t)
	record := claimRunning(t, s, "job-live")
	result := cancelOutcome(t, s, "job-live", "wedged")
	if result["success"] != true || result["cancelled"] != true || result["execution_id"] != record["id"] {
		t.Fatalf("result %v", result)
	}
	if !s.newCancelRequest(recText(record, "id")).IsSet() {
		t.Fatal("the live execution got no marker")
	}
}

func TestCancelReachesALiveRunBehindARefusedTrigger(t *testing.T) {
	s, _ := newTestStore(t)
	l := openExecutionsLedger(s)
	ctx := context.Background()
	live := claimRunning(t, s, "job-busy")
	refused := mustLedger(t)(l.Create(ctx, "job-busy", "test", nil))
	mustLedger(t)(l.Finish(ctx, recText(refused, "id"), false, strPtr("Fire claim was not acquired"), nil, false))
	editExecution(t, l, recText(refused, "id"), "claimed_at", "2999-01-01T00:00:00+00:00")
	result := cancelOutcome(t, s, "job-busy", "")
	if result["cancelled"] != true || result["execution_id"] != live["id"] {
		t.Fatalf("result %v", result)
	}
}

func TestCancelIsANoopWhenNothingIsInFlight(t *testing.T) {
	s, _ := newTestStore(t)
	l := openExecutionsLedger(s)
	ctx := context.Background()
	claimRunning(t, s, "job-done")
	latest := mustLedger(t)(l.Latest(ctx, "job-done"))
	mustLedger(t)(l.Finish(ctx, recText(latest, "id"), true, nil, nil, false))
	result := cancelOutcome(t, s, "job-done", "")
	if result["success"] != true || result["cancelled"] != false {
		t.Fatalf("result %v", result)
	}
	if v, ok := result["execution_id"]; !ok || v != nil {
		t.Fatalf("execution id %v", v)
	}
}

func TestOperatorCancelIsNotTreatedAsClaimLoss(t *testing.T) {
	s, _ := newTestStore(t)
	job := &Job{ID: "job-fence", FireClaim: &Claim{By: "someone-else"}}
	mustRequestCancel(t, s, "exec-fenced", nil)
	fence := s.newFireOwnership(job, nil, nil, s.newCancelRequest("exec-fenced"))
	if !fence.cancelEvent.IsSet() {
		t.Fatal("the run did not stop")
	}
	if got := cancelReason(fence.cancelEvent); got != "cancelled on request (icebath cron cancel)" {
		t.Fatalf("reason %q", got)
	}
	if fence.transportCancelled() || fence.claimLost != nil {
		t.Fatal("an operator cancel read as a lost fire claim")
	}
}

func TestASecondRequestKeepsTheReasonTheRunAlreadyReported(t *testing.T) {
	s, _ := newTestStore(t)
	mustRequestCancel(t, s, "exec-reason", strPtr("operator asked"))
	mustRequestCancel(t, s, "exec-reason", strPtr("second ask"))
	mustRequestCancel(t, s, "exec-reason", nil)
	if got := s.newCancelRequest("exec-reason").Reason(); got != "operator asked" {
		t.Fatalf("reason %q", got)
	}
}

func TestASecondRequestStillPrunesStaleMarkers(t *testing.T) {
	s, _ := newTestStore(t)
	var pruned []*float64
	patchChronos(t, &pruneCancelMarkers, func(_ *Store, now *float64) { pruned = append(pruned, now) })
	mustRequestCancel(t, s, "exec-pruned", strPtr("first"))
	mustRequestCancel(t, s, "exec-pruned", strPtr("second"))
	if len(pruned) != 2 {
		t.Fatalf("pruned %d times", len(pruned))
	}
}

func TestACancelLandsOnAFilesystemWithoutHardLinks(t *testing.T) {
	s, _ := newTestStore(t)
	patchChronos(t, &cancelLink, func(string, string) error {
		return &os.LinkError{Op: "link", Err: syscall.EPERM}
	})
	mustRequestCancel(t, s, "exec-exfat", strPtr("first ask"))
	mustRequestCancel(t, s, "exec-exfat", strPtr("second ask"))
	request := s.newCancelRequest("exec-exfat")
	if !request.IsSet() || request.Reason() != "first ask" {
		t.Fatalf("set %v reason %q", request.IsSet(), request.Reason())
	}
}

func TestStoppingALadderRerunDoesNotSpendAnotherRepeat(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	ctx := context.Background()
	job, err := s.createJob(CreateJobOptions{Prompt: strPtr("ping"), Schedule: "every 1h", Deliver: strPtr("local"), Repeat: 2})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	stored[0].Repeat.Completed = 1
	if err := s.SaveJobs(stored, nil, false); err != nil {
		t.Fatal(err)
	}
	current, err := s.getJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	instant := deref(current.NextRunAt)
	rerun := *current
	rerun.ScheduledInstant = &instant
	var expr *string
	if job.Schedule != nil && job.Schedule.Expr != "" {
		expr = strPtr(job.Schedule.Expr)
	}
	rerun.UnreachableRetry = &RetryState{At: instant, Expr: expr}
	fire := recText(mustLedger(t)(openExecutionsLedger(s).Create(ctx, job.ID, "test", nil)), "id")
	mustRequestCancel(t, s, fire, nil)
	if err := s.finishCancelledRun(ctx, &rerun, fire, nil); err != nil {
		t.Fatal(err)
	}
	after, err := s.getJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Repeat == nil || after.Repeat.Completed != 1 || !after.Enabled {
		t.Fatalf("after %+v %+v", after.Repeat, after.Enabled)
	}
}

func TestOverlappingCancelRequestsKeepTheFirstReason(t *testing.T) {
	s, _ := newTestStore(t)
	real := cancelMarkerExists
	patchChronos(t, &cancelMarkerExists, func(path string) (bool, error) {
		if filepath.Base(path) == "exec-race.cancel" {
			return false, nil
		}
		return real(path)
	})
	mustRequestCancel(t, s, "exec-race", strPtr("first ask"))
	mustRequestCancel(t, s, "exec-race", strPtr("second ask"))
	if got := s.newCancelRequest("exec-race").Reason(); got != "first ask" {
		t.Fatalf("reason %q", got)
	}
}

func TestAMissedRowDoesNotHideALiveRun(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	ctx := context.Background()
	job, err := s.createJob(CreateJobOptions{Prompt: strPtr("long"), Schedule: "every 1h", Deliver: strPtr("local")})
	if err != nil {
		t.Fatal(err)
	}
	ledger := openExecutionsLedger(s)
	live := recText(mustLedger(t)(ledger.Create(ctx, job.ID, "test", nil)), "id")
	mustLedger(t)(ledger.MarkRunning(ctx, live))
	mustLedger(t)(ledger.RecordMissed(ctx, job.ID, 3, nil, "slept"))
	inflight := mustLedger(t)(ledger.LiveInflight(ctx, job.ID))
	if inflight == nil || recText(inflight, "id") != live {
		t.Fatalf("live run hidden behind the missed row, got %v want %s", inflight, live)
	}
}
