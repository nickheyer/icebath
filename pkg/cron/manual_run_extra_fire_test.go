package cron

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Store at 2026-10-28 09:00 -07:00 whose clock the returned pointer moves
func manualRunClock(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	zone := time.FixedZone("", -7*3600)
	cur := time.Date(2026, 10, 28, 9, 0, 0, 0, zone)
	return mutableClockStore(t, &cur, zone), &cur
}

func claimFire(t *testing.T, s *Store, id string, o FireClaimOptions) *Job {
	t.Helper()
	claimed, ok, err := s.claimJobForFire(context.Background(), id, o)
	if err != nil || !ok || claimed == nil {
		t.Fatalf("fire was not claimed: %v %v", ok, err)
	}
	return claimed
}

func mustClaimDispatch(t *testing.T, s *Store, id string) {
	t.Helper()
	if ok, err := s.claimDispatch(id); err != nil || !ok {
		t.Fatalf("claimDispatch = %v %v", ok, err)
	}
}

func markOwned(t *testing.T, s *Store, id string, success bool, owner string, o MarkOptions) {
	t.Helper()
	o.ExpectedFireOwner = &owner
	if ok, err := s.markJobRun(context.Background(), id, success, o); err != nil || !ok {
		t.Fatalf("markJobRun = %v %v", ok, err)
	}
}

func fire(t *testing.T, s *Store, id string, o FireClaimOptions) {
	t.Helper()
	claimed := claimFire(t, s, id, o)
	mustClaimDispatch(t, s, id)
	markOwned(t, s, id, true, claimed.FireClaim.By, MarkOptions{})
}

func mustTrigger(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.triggerJob(id, ""); err != nil {
		t.Fatal(err)
	}
}

func dueList(t *testing.T, s *Store) []string {
	t.Helper()
	return jobIDs(mustDue(t, s))
}

func repeatOf(j *Job) [2]any {
	if j.Repeat == nil {
		return [2]any{nil, 0}
	}
	if j.Repeat.Times == nil {
		return [2]any{nil, j.Repeat.Completed}
	}
	return [2]any{*j.Repeat.Times, j.Repeat.Completed}
}

func reminder(t *testing.T, s *Store, schedule string) *Job {
	return mustCreate(t, s, CreateJobOptions{Prompt: prompt("remind me"), Schedule: schedule, Name: prompt("reminder")})
}

func TestManualRunKeepsTheScheduledOneshotAndItStillFires(t *testing.T) {
	s, cur := manualRunClock(t)
	job := reminder(t, s, "in 30m")
	slot := *job.NextRunAt
	fire(t, s, job.ID, FireClaimOptions{Manual: true})
	after := mustGet(t, s, job.ID)
	if after.State != "scheduled" || !after.Enabled || deref(after.NextRunAt) != slot {
		t.Fatalf("after manual run = %s %v %v", after.State, after.Enabled, deref(after.NextRunAt))
	}
	if repeatOf(after) != [2]any{1, 0} || deref(after.LastStatus) != "ok" {
		t.Fatalf("repeat %v status %v", repeatOf(after), deref(after.LastStatus))
	}
	*cur = cur.Add(30*time.Minute + 5*time.Second)
	if due := dueList(t, s); !reflect.DeepEqual(due, []string{job.ID}) {
		t.Fatalf("due = %v", due)
	}
	fire(t, s, job.ID, FireClaimOptions{})
	done := mustGet(t, s, job.ID)
	if done.State != "completed" || done.NextRunAt != nil || repeatOf(done) != [2]any{1, 1} {
		t.Fatalf("done = %s %v %v", done.State, done.NextRunAt, repeatOf(done))
	}
}

func TestGatewayForwardedTriggerRestoresTheSlotAtClaimTime(t *testing.T) {
	s, _ := manualRunClock(t)
	job := reminder(t, s, "in 30m")
	slot := *job.NextRunAt
	mustTrigger(t, s, job.ID)
	if due := dueList(t, s); !reflect.DeepEqual(due, []string{job.ID}) {
		t.Fatalf("due = %v", due)
	}
	claimed := claimFire(t, s, job.ID, FireClaimOptions{})
	if deref(mustGet(t, s, job.ID).NextRunAt) != slot {
		t.Fatal("slot not durable before the run starts")
	}
	mustClaimDispatch(t, s, job.ID)
	markOwned(t, s, job.ID, true, claimed.FireClaim.By, MarkOptions{})
	after := mustGet(t, s, job.ID)
	if after.State != "scheduled" || deref(after.NextRunAt) != slot || after.Repeat.Completed != 0 {
		t.Fatalf("after = %s %v %v", after.State, deref(after.NextRunAt), repeatOf(after))
	}
	if after.hasKey(restoreKey) || after.ManualRunRestoreAt != nil {
		t.Fatal("manual_run_restore_at survived the claim")
	}
}

func TestSecondTriggerBeforeTheClaimKeepsTheOriginalSlot(t *testing.T) {
	s, cur := manualRunClock(t)
	job := reminder(t, s, "in 30m")
	slot := *job.NextRunAt
	mustTrigger(t, s, job.ID)
	*cur = cur.Add(20 * time.Second)
	mustTrigger(t, s, job.ID)
	fire(t, s, job.ID, FireClaimOptions{})
	after := mustGet(t, s, job.ID)
	if after.State != "scheduled" || deref(after.NextRunAt) != slot || repeatOf(after) != [2]any{1, 0} {
		t.Fatalf("after = %s %v %v", after.State, deref(after.NextRunAt), repeatOf(after))
	}
}

func TestForwardedRecurringRunSurvivesTheTickAdvance(t *testing.T) {
	s, cur := manualRunClock(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("digest"), Schedule: "every 60m", Name: prompt("digest"), Repeat: 1})
	slot := *job.NextRunAt
	*cur = cur.Add(20 * time.Minute)
	mustTrigger(t, s, job.ID)
	due := dueList(t, s)
	if !reflect.DeepEqual(due, []string{job.ID}) {
		t.Fatalf("due = %v", due)
	}
	if _, err := s.advanceNextRuns(due); err != nil {
		t.Fatal(err)
	}
	fire(t, s, job.ID, FireClaimOptions{})
	after := mustGet(t, s, job.ID)
	if after.State != "scheduled" || deref(after.NextRunAt) != slot || repeatOf(after) != [2]any{1, 0} {
		t.Fatalf("after = %s %v %v", after.State, deref(after.NextRunAt), repeatOf(after))
	}
}

func TestManualRunDoesNotSpendARecurringRepeatBudgetOrShiftTheSchedule(t *testing.T) {
	s, cur := manualRunClock(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("digest"), Schedule: "every 60m", Name: prompt("digest"), Repeat: 2})
	slot := *job.NextRunAt
	*cur = cur.Add(20 * time.Minute)
	fire(t, s, job.ID, FireClaimOptions{Manual: true})
	after := mustGet(t, s, job.ID)
	if repeatOf(after) != [2]any{2, 0} || deref(after.NextRunAt) != slot {
		t.Fatalf("after = %v %v", repeatOf(after), deref(after.NextRunAt))
	}
}

func TestCrashMidManualRunLeavesTheScheduledOneshotDue(t *testing.T) {
	s, cur := manualRunClock(t)
	job := reminder(t, s, "in 30m")
	claimFire(t, s, job.ID, FireClaimOptions{Manual: true})
	mustClaimDispatch(t, s, job.ID)
	stale := FormatISO(cur.Add(-2 * time.Hour))
	editJobs(t, s, func(j *Job) {
		j.FireClaim.By = "dead-host:0"
		j.FireClaim.At = stale
	})
	*cur = cur.Add(30*time.Minute + 5*time.Second)
	if due := dueList(t, s); !reflect.DeepEqual(due, []string{job.ID}) {
		t.Fatalf("due = %v", due)
	}
	if mustGet(t, s, job.ID).Repeat.Completed != 0 {
		t.Fatal("crashed manual run spent the one shot")
	}
}

func TestRunAtOrAfterTheDueSlotIsTheScheduledFire(t *testing.T) {
	s, cur := manualRunClock(t)
	job := reminder(t, s, "in 2m")
	*cur = cur.Add(2*time.Minute + 10*time.Second)
	fire(t, s, job.ID, FireClaimOptions{Manual: true})
	done := mustGet(t, s, job.ID)
	if done.State != "completed" || done.Repeat.Completed != 1 {
		t.Fatalf("done = %s %v", done.State, repeatOf(done))
	}
}

func TestTriggerLandingDuringAManualRunSurvivesItsCompletion(t *testing.T) {
	s, cur := manualRunClock(t)
	job := reminder(t, s, "in 30m")
	slot := *job.NextRunAt
	claimed := claimFire(t, s, job.ID, FireClaimOptions{Manual: true})
	mustClaimDispatch(t, s, job.ID)
	*cur = cur.Add(10 * time.Second)
	mustTrigger(t, s, job.ID)
	*cur = cur.Add(10 * time.Second)
	if ok, err := s.heartbeatFireClaim(job.ID, claimed.FireClaim.By); err != nil || !ok {
		t.Fatalf("heartbeat = %v %v", ok, err)
	}
	markOwned(t, s, job.ID, true, claimed.FireClaim.By, MarkOptions{})
	if due := dueList(t, s); !reflect.DeepEqual(due, []string{job.ID}) {
		t.Fatalf("queued run-now not due: %v", due)
	}
	fire(t, s, job.ID, FireClaimOptions{})
	after := mustGet(t, s, job.ID)
	if after.State != "scheduled" || deref(after.NextRunAt) != slot || repeatOf(after) != [2]any{1, 0} {
		t.Fatalf("after = %s %v %v", after.State, deref(after.NextRunAt), repeatOf(after))
	}
}

func TestQuotaHoldOnAManualRunDelaysAQueuedRunNowWithoutSpendingTheSchedule(t *testing.T) {
	s, cur := manualRunClock(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("digest"), Schedule: "every 60m", Name: prompt("digest"), Repeat: 3})
	slot := *job.NextRunAt
	*cur = cur.Add(20 * time.Minute)
	claimed := claimFire(t, s, job.ID, FireClaimOptions{Manual: true})
	mustClaimDispatch(t, s, job.ID)
	*cur = cur.Add(10 * time.Second)
	mustTrigger(t, s, job.ID)
	*cur = cur.Add(10 * time.Second)
	markOwned(t, s, job.ID, false, claimed.FireClaim.By, MarkOptions{Error: prompt("quota"), QuotaHoldSeconds: 600})
	held := mustGet(t, s, job.ID)
	if deref(held.NextRunAt) <= FormatISO(*cur) {
		t.Fatalf("not parked through the window: %v", deref(held.NextRunAt))
	}
	if due := dueList(t, s); len(due) != 0 {
		t.Fatalf("due inside the window: %v", due)
	}
	*cur = nextRunTime(t, held).Add(5 * time.Second)
	due := dueList(t, s)
	if !reflect.DeepEqual(due, []string{job.ID}) {
		t.Fatalf("delayed run-now not due: %v", due)
	}
	if _, err := s.advanceNextRuns(due); err != nil {
		t.Fatal(err)
	}
	fire(t, s, job.ID, FireClaimOptions{})
	after := mustGet(t, s, job.ID)
	if deref(after.NextRunAt) != slot || repeatOf(after) != [2]any{3, 0} {
		t.Fatalf("after = %v %v", deref(after.NextRunAt), repeatOf(after))
	}
}
