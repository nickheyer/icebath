package cron

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func jobAsRow(t *testing.T, job *Job) map[string]any {
	t.Helper()
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func wantErrorNaming(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("got %v, want an error naming %q", err, fragment)
	}
}

func isoNowUTC(offset time.Duration) string {
	return time.Now().UTC().Add(offset).Format("2006-01-02T15:04:05.000000") + "+00:00"
}

func TestCompletedOneshotTriggerIsRefusedAndDiskRecordIsUnchanged(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("done"), Schedule: "in 30m", Name: strPtr("done"), Repeat: 1})
	mustMark(t, s, job.ID, true, MarkOptions{})
	before := rawStoredRows(t, s)
	_, err := s.triggerJob(context.Background(), job.ID, "")
	wantErrorNaming(t, err, "terminal")
	if after := rawStoredRows(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("store changed\nbefore %v\nafter  %v", before, after)
	}
	if got := jobAsRow(t, mustGet(t, s, job.ID)); !reflect.DeepEqual(got, before[0]) {
		t.Fatalf("record %v want %v", got, before[0])
	}
}

func TestExhaustedRecurringJobTriggerIsRefused(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("done"), Schedule: "every 1h", Repeat: 1})
	mustMark(t, s, job.ID, true, MarkOptions{})
	_, err := s.triggerJob(context.Background(), job.ID, "")
	wantErrorNaming(t, err, "terminal")
}

func TestWedgedClaimedOneshotRemainsTriggerable(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("wedged"), Schedule: "in 30m", Repeat: 2})
	record := mustGet(t, s, job.ID)
	record.RunClaim = &Claim{At: isoNowUTC(0), By: "dead-worker"}
	record.State, record.Enabled = "scheduled", true
	record.NextRunAt = strPtr(isoNowUTC(-time.Minute))
	if err := s.SaveJobs(context.Background(), []*Job{record}, nil, false); err != nil {
		t.Fatal(err)
	}
	triggered, err := s.triggerJob(context.Background(), job.ID, "")
	if err != nil || triggered.State != "scheduled" || !triggered.Enabled {
		t.Fatalf("triggered %+v %v", triggered, err)
	}
}

func TestPausedJobRunOverrideRemainsAllowed(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("paused"), Schedule: "every 1h"})
	if _, err := s.pauseJob(context.Background(), job.ID, nil); err != nil {
		t.Fatal(err)
	}
	triggered, err := s.triggerJob(context.Background(), job.ID, "")
	if err != nil || triggered.State != "scheduled" || !triggered.Enabled {
		t.Fatalf("triggered %+v %v", triggered, err)
	}
}

func TestTerminalJobsAreNotDueOrAdvanced(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("done"), Schedule: "every 1h", Repeat: 1})
	mustMark(t, s, job.ID, true, MarkOptions{})
	before := rawStoredRows(t, s)
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("due %v", jobIDs(due))
	}
	if advanced, err := s.advanceNextRun(context.Background(), job.ID); err != nil || advanced {
		t.Fatalf("advanced %v %v", advanced, err)
	}
	if after := rawStoredRows(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("store changed\nbefore %v\nafter  %v", before, after)
	}
}

func TestUpdateCannotReactivateTerminalRecord(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("done"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, job.ID, true, MarkOptions{})
	_, err := s.updateJob(context.Background(), job.ID, map[string]any{"enabled": true})
	wantErrorNaming(t, err, "terminal")
	_, err = s.updateJob(context.Background(), job.ID, map[string]any{"schedule": "every 1h"})
	wantErrorNaming(t, err, "terminal")
}

func TestRearmCompletedOneshotRestoresScheduleAndPreservesHistory(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("done"), Schedule: "in 30m", Repeat: 3})
	mustMark(t, s, job.ID, true, MarkOptions{})
	finished := mustGet(t, s, job.ID)
	rearmed, err := s.rearmOneshot(context.Background(), job.ID, isoNowUTC(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rearmed.Schedule == nil || rearmed.Schedule.Kind != "once" || rearmed.Repeat == nil || rearmed.Repeat.Times == nil || *rearmed.Repeat.Times != 3 || rearmed.Repeat.Completed != 0 {
		t.Fatalf("schedule %+v repeat %+v", rearmed.Schedule, rearmed.Repeat)
	}
	if rearmed.State != "scheduled" || !rearmed.Enabled || rearmed.NextRunAt == nil || *rearmed.NextRunAt != rearmed.Schedule.RunAt {
		t.Fatalf("state %q enabled %v next %v run_at %q", rearmed.State, rearmed.Enabled, rearmed.NextRunAt, rearmed.Schedule.RunAt)
	}
	if deref(rearmed.LastRunAt) != deref(finished.LastRunAt) || finished.LastRunAt == nil || deref(rearmed.LastStatus) != deref(finished.LastStatus) {
		t.Fatalf("history %v %v want %v %v", deref(rearmed.LastRunAt), deref(rearmed.LastStatus), deref(finished.LastRunAt), deref(finished.LastStatus))
	}
}

func TestRearmRefusesRecurringAndLiveClaim(t *testing.T) {
	s, _ := newTestStore(t)
	recurring := mustCreate(t, s, CreateJobOptions{Prompt: prompt("recurring"), Schedule: "every 1h"})
	future := isoNowUTC(5 * time.Minute)
	_, err := s.rearmOneshot(context.Background(), recurring.ID, future)
	wantErrorNaming(t, err, "one-shot")
	oneshot := mustCreate(t, s, CreateJobOptions{Prompt: prompt("claimed"), Schedule: "in 30m"})
	record := mustGet(t, s, oneshot.ID)
	record.RunClaim = &Claim{At: isoNowUTC(0), By: "live"}
	if err := s.SaveJobs(context.Background(), []*Job{record}, nil, false); err != nil {
		t.Fatal(err)
	}
	_, err = s.rearmOneshot(context.Background(), oneshot.ID, future)
	wantErrorNaming(t, err, "claim")
}

// Leaves a recurring job as mark_job_run does when the next occurrence cannot be computed
func forceErrorState(t *testing.T, s *Store, jobID string) {
	t.Helper()
	prev := computeNextRunFn
	computeNextRunFn = func(*Store, *Schedule, *string, *string) (*string, error) { return nil, nil }
	defer func() { computeNextRunFn = prev }()
	mustMark(t, s, jobID, true, MarkOptions{})
}

func stuckRecurring(t *testing.T) (*Store, *Job) {
	t.Helper()
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("recurring"), Schedule: "every 5m"})
	forceErrorState(t, s, job.ID)
	return s, job
}

func TestRecurringJobStuckInErrorStateIsRecoverableDueScanSelfHealsNextRunAt(t *testing.T) {
	s, job := stuckRecurring(t)
	if stuck := mustGet(t, s, job.ID); stuck.State != "error" || !stuck.Enabled || stuck.NextRunAt != nil {
		t.Fatalf("stuck %q %v %v", stuck.State, stuck.Enabled, stuck.NextRunAt)
	}
	mustDue(t, s)
	if healed := mustGet(t, s, job.ID); healed.NextRunAt == nil {
		t.Fatal("due-scan self-heal never reached an error-state recurring job")
	}
}

func TestRecurringJobStuckInErrorStateIsRecoverableResumeJobRecoversErrorState(t *testing.T) {
	s, job := stuckRecurring(t)
	resumed, err := s.resumeJob(context.Background(), job.ID)
	if err != nil || resumed == nil || resumed.State != "scheduled" || !resumed.Enabled || resumed.NextRunAt == nil {
		t.Fatalf("resumed %+v %v", resumed, err)
	}
}

func TestRecurringJobStuckInErrorStateIsRecoverableClaimJobForFireRecoversErrorState(t *testing.T) {
	s, job := stuckRecurring(t)
	claimed, ok, err := s.claimJobForFire(context.Background(), job.ID, FireClaimOptions{})
	if err != nil || !ok || claimed == nil || claimed.FireClaim == nil {
		t.Fatalf("claim_job_for_fire must not refuse an error-state recurring job, got %+v %v %v", claimed, ok, err)
	}
}

func TestRecurringJobStuckInErrorStateIsRecoverableAdvanceNextRunsRecoversErrorState(t *testing.T) {
	s, job := stuckRecurring(t)
	if advanced, err := s.advanceNextRuns(context.Background(), []string{job.ID}); err != nil || advanced != 1 {
		t.Fatalf("advanced %d %v", advanced, err)
	}
}

func TestRecurringJobStuckInErrorStateIsRecoverablePauseJobNowWorksOnErrorState(t *testing.T) {
	s, job := stuckRecurring(t)
	paused, err := s.pauseJob(context.Background(), job.ID, nil)
	if err != nil || paused == nil || paused.State != "paused" || paused.Enabled {
		t.Fatalf("paused %+v %v", paused, err)
	}
}

func TestRecurringJobStuckInErrorStateIsRecoverableCompletedOneshotStillBlockedOnEveryPath(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("done"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, job.ID, true, MarkOptions{})
	if state := mustGet(t, s, job.ID).State; state != "completed" {
		t.Fatalf("state %q", state)
	}
	_, err := s.resumeJob(context.Background(), job.ID)
	wantErrorNaming(t, err, "terminal")
	if claimed, ok, err := s.claimJobForFire(context.Background(), job.ID, FireClaimOptions{}); err != nil || ok || claimed != nil {
		t.Fatalf("claimed %+v %v %v", claimed, ok, err)
	}
	if advanced, err := s.advanceNextRuns(context.Background(), []string{job.ID}); err != nil || advanced != 0 {
		t.Fatalf("advanced %d %v", advanced, err)
	}
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("due %v", jobIDs(due))
	}
}
