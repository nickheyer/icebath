package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Naive local wall clock text offset from now, the shape datetime.now().isoformat() writes
func naiveAgo(d time.Duration) string {
	return time.Now().Add(-d).Format("2006-01-02T15:04:05.000000")
}

func mustGet(t *testing.T, s *Store, id string) *Job {
	t.Helper()
	job, err := s.getJob(context.Background(), id)
	if err != nil || job == nil {
		t.Fatalf("getJob(%s) = %v, %v", id, job, err)
	}
	return job
}

func mustMark(t *testing.T, s *Store, id string, success bool, o MarkOptions) {
	t.Helper()
	if _, err := s.markJobRun(context.Background(), id, success, o); err != nil {
		t.Fatalf("markJobRun(%s): %v", id, err)
	}
}

func mustDue(t *testing.T, s *Store) []*Job {
	t.Helper()
	due, err := s.getDueJobs(context.Background())
	if err != nil {
		t.Fatalf("getDueJobs: %v", err)
	}
	return due
}

// Rewrites every stored record through fn and saves the result
func editJobs(t *testing.T, s *Store, fn func(j *Job)) {
	t.Helper()
	jobs, err := s.LoadJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		fn(j)
	}
	if err := s.SaveJobs(context.Background(), jobs, nil, false); err != nil {
		t.Fatal(err)
	}
}

func seedJobs(t *testing.T, s *Store, jobs ...map[string]any) {
	t.Helper()
	list := make([]any, len(jobs))
	for i, j := range jobs {
		list[i] = j
	}
	writeJobsFile(t, s, map[string]any{"jobs": list})
}

func inFuture(t *testing.T, s *Store, text *string) bool {
	t.Helper()
	if text == nil {
		return false
	}
	at, ok := s.clk.parseAware(*text, nil)
	return ok && at.After(s.clk.now())
}

func TestMarkJobRunIncrementsCompleted(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Test"), Schedule: "every 1h"})
	mustMark(t, s, job.ID, true, MarkOptions{})
	if got := mustGet(t, s, job.ID); got.Repeat.Completed != 1 || deref(got.LastStatus) != "ok" {
		t.Fatalf("completed %d status %v", got.Repeat.Completed, deref(got.LastStatus))
	}
}

func TestMarkJobRunRepeatLimitRetainsCompletedRecord(t *testing.T) {
	s, _ := newTestStore(t)
	once := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Once"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, once.ID, true, MarkOptions{})
	got, _ := s.getJob(context.Background(), once.ID)
	if got == nil {
		t.Fatal("completed one-shot was deleted from jobs.json")
	}
	if got.State != "completed" || got.Enabled || got.NextRunAt != nil || deref(got.LastStatus) != "ok" {
		t.Fatalf("completed one shot %+v", got)
	}
}

func TestMarkJobRunRepeatLimitRetainsDeliveryError(t *testing.T) {
	s, _ := newTestStore(t)
	failed := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Once"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, failed.ID, true, MarkOptions{DeliveryError: prompt("platform 'telegram' not configured")})
	got := mustGet(t, s, failed.ID)
	if got.State != "completed" || deref(got.LastDeliveryError) != "platform 'telegram' not configured" || deref(got.LastStatus) != "delivery_failed" {
		t.Fatalf("delivery error lost %+v", got)
	}
}

func TestMarkJobRunCompletedOneshotVisibleInList(t *testing.T) {
	s, _ := newTestStore(t)
	listed := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Once"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, listed.ID, true, MarkOptions{DeliveryError: prompt("send failed: 502")})
	all, _ := s.listJobs(context.Background(), true)
	var seen *Job
	for _, j := range all {
		if j.ID == listed.ID {
			seen = j
		}
	}
	if seen == nil || seen.State != "completed" || deref(seen.LastDeliveryError) != "send failed: 502" || deref(seen.LastStatus) != "delivery_failed" {
		t.Fatalf("completed one shot missing from list %+v", seen)
	}
	enabled, _ := s.listJobs(context.Background(), false)
	if containsString(jobIDs(enabled), listed.ID) {
		t.Fatal("completed one shot listed as enabled")
	}
}

func TestMarkJobRunCompletedOneshotNotDue(t *testing.T) {
	s, _ := newTestStore(t)
	once := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Once"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, once.ID, true, MarkOptions{})
	if containsString(jobIDs(mustDue(t, s)), once.ID) {
		t.Fatal("completed one shot is due")
	}
}

func TestMarkJobRunErrorStatus(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Fail"), Schedule: "every 1h"})
	mustMark(t, s, job.ID, false, MarkOptions{Error: prompt("timeout")})
	if got := mustGet(t, s, job.ID); deref(got.LastStatus) != "error" || deref(got.LastError) != "timeout" {
		t.Fatalf("error status %+v", got)
	}
}

func TestMarkJobRunDeliveryErrorTrackedSeparately(t *testing.T) {
	s, _ := newTestStore(t)
	rep := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Report"), Schedule: "every 1h"})
	mustMark(t, s, rep.ID, true, MarkOptions{DeliveryError: prompt("send failed: 502")})
	got := mustGet(t, s, rep.ID)
	if deref(got.LastStatus) != "delivery_failed" || got.LastError != nil || deref(got.LastDeliveryError) != "send failed: 502" || got.FailureStreak != 0 {
		t.Fatalf("delivery error not tracked separately %+v", got)
	}
}

func TestMarkJobRunSuccessWithoutDeliveryErrorStaysOk(t *testing.T) {
	s, _ := newTestStore(t)
	ok := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Report"), Schedule: "every 1h"})
	mustMark(t, s, ok.ID, true, MarkOptions{})
	if got := mustGet(t, s, ok.ID); deref(got.LastStatus) != "ok" {
		t.Fatalf("successful run status %s", deref(got.LastStatus))
	}
	mustMark(t, s, ok.ID, true, MarkOptions{DeliveryError: prompt("")})
	if got := mustGet(t, s, ok.ID); deref(got.LastStatus) != "ok" {
		t.Fatalf("empty delivery error flipped status to %s", deref(got.LastStatus))
	}
}

func TestMarkJobRunAgentFailureStillErrorWithDeliveryError(t *testing.T) {
	s, _ := newTestStore(t)
	both := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Report"), Schedule: "every 1h"})
	mustMark(t, s, both.ID, false, MarkOptions{Error: prompt("timeout"), DeliveryError: prompt("send failed: 502")})
	if got := mustGet(t, s, both.ID); deref(got.LastStatus) != "error" || deref(got.LastError) != "timeout" || got.FailureStreak != 1 {
		t.Fatalf("agent failure with delivery error %+v", got)
	}
}

func TestMarkJobRunExplicitStatusOverrideWinsOverDeliveryFailed(t *testing.T) {
	s, _ := newTestStore(t)
	override := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Report"), Schedule: "every 1h"})
	mustMark(t, s, override.ID, true, MarkOptions{DeliveryError: prompt("send failed: 502"), Status: prompt("blocked_config")})
	if got := mustGet(t, s, override.ID); deref(got.LastStatus) != "blocked_config" || deref(got.LastDeliveryError) != "send failed: 502" {
		t.Fatalf("explicit status lost %+v", got)
	}
}

func TestMarkJobRunFailureStreakIncrementsAndResets(t *testing.T) {
	s, _ := newTestStore(t)
	flaky := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Flaky"), Schedule: "every 1h"})
	if mustGet(t, s, flaky.ID).FailureStreak != 0 {
		t.Fatal("new job has a streak")
	}
	mustMark(t, s, flaky.ID, false, MarkOptions{Error: prompt("timeout")})
	mustMark(t, s, flaky.ID, false, MarkOptions{Error: prompt("timeout")})
	if mustGet(t, s, flaky.ID).FailureStreak != 2 {
		t.Fatal("streak not incremented")
	}
	mustMark(t, s, flaky.ID, true, MarkOptions{})
	if mustGet(t, s, flaky.ID).FailureStreak != 0 {
		t.Fatal("streak not reset")
	}
}

func TestMarkJobRunFailureStreakIgnoresDeliveryErrors(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Report"), Schedule: "every 1h"})
	mustMark(t, s, job.ID, false, MarkOptions{Error: prompt("timeout")})
	mustMark(t, s, job.ID, true, MarkOptions{DeliveryError: prompt("send failed: 502")})
	if mustGet(t, s, job.ID).FailureStreak != 0 {
		t.Fatal("delivery error kept the agent failure streak")
	}
}

func TestMarkJobRunFailureStreakBackcompatMissingField(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Old"), Schedule: "every 1h"})
	raw, _ := os.ReadFile(s.jobsFile)
	stripped := regexp.MustCompile(`\s*"failure_streak": 0,`).ReplaceAllString(string(raw), "")
	if stripped == string(raw) {
		t.Fatalf("failure_streak not on disk: %s", raw)
	}
	os.WriteFile(s.jobsFile, []byte(stripped), 0o600)
	mustMark(t, s, job.ID, false, MarkOptions{Error: prompt("boom")})
	if mustGet(t, s, job.ID).FailureStreak != 1 {
		t.Fatal("streak from a record without the field")
	}
}

// The cron evaluator is linked in, so a schedule whose next run cannot be computed is the croniter missing window
func TestMarkJobRunRecurringCronNotDisabledWhenCroniterMissing(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Recurring"), Schedule: "0 7,15,23 * * *"})
	if job.Kind() != "cron" {
		t.Fatalf("kind %s", job.Kind())
	}
	editJobs(t, s, func(j *Job) { j.Schedule.Expr = "" })
	mustMark(t, s, job.ID, true, MarkOptions{})
	got, _ := s.getJob(context.Background(), job.ID)
	if got == nil {
		t.Fatal("recurring cron job was deleted")
	}
	if !got.Enabled || got.State != "error" || got.State == "completed" || got.NextRunAt != nil || deref(got.LastError) == "" ||
		!strings.Contains(strings.ToLower(deref(got.LastError)), "croniter") {
		t.Fatalf("recurring job retired instead of error %+v", got)
	}
}

func TestMarkJobRunTransientCroniterImportErrorNotLatched(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Recurring"), Schedule: "0 7,15,23 * * *"})
	if job.Kind() != "cron" {
		t.Fatalf("kind %s", job.Kind())
	}
	expr := job.Schedule.Expr
	editJobs(t, s, func(j *Job) { j.Schedule.Expr = "" })
	if next, err := s.computeNextRun(mustGet(t, s, job.ID).Schedule, nil, nil); err != nil || next != nil {
		t.Fatalf("next run during the failure window = %v %v", next, err)
	}
	mustMark(t, s, job.ID, true, MarkOptions{})
	if got := mustGet(t, s, job.ID); got.State != "error" || got.NextRunAt != nil {
		t.Fatalf("failure window outcome %+v", got)
	}
	editJobs(t, s, func(j *Job) { j.Schedule.Expr = expr })
	if _, err := ParseCron(expr); err != nil {
		t.Fatalf("the cron evaluator stayed unavailable: %v", err)
	}
	if next, err := s.computeNextRun(mustGet(t, s, job.ID).Schedule, nil, nil); err != nil || next == nil {
		t.Fatalf("next run once the evaluator answers = %v %v", next, err)
	}
	mustDue(t, s)
	if got := mustGet(t, s, job.ID); got.State != "scheduled" {
		t.Fatalf("due scan did not re arm %+v", got)
	}
}

func TestAdvanceNextRunAdvancesIntervalJob(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Recurring check"), Schedule: "every 1h"})
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(naiveAgo(5 * time.Minute)) })
	if ok, err := s.advanceNextRun(context.Background(), job.ID); !ok || err != nil {
		t.Fatalf("advance = %v, %v", ok, err)
	}
	if !inFuture(t, s, mustGet(t, s, job.ID).NextRunAt) {
		t.Fatal("next_run_at should be in the future after advance")
	}
}

func TestAdvanceNextRunSkipsOneshotJob(t *testing.T) {
	s, _ := newTestStore(t)
	once := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Run once"), Schedule: "in 30m"})
	before := mustGet(t, s, once.ID).NextRunAt
	if ok, err := s.advanceNextRun(context.Background(), once.ID); ok || err != nil {
		t.Fatalf("one shot advanced %v %v", ok, err)
	}
	if after := mustGet(t, s, once.ID).NextRunAt; deref(after) != deref(before) {
		t.Fatalf("one-shot next_run_at should be unchanged %s -> %s", deref(before), deref(after))
	}
}

func TestAdvanceNextRunCrashSafetyScenario(t *testing.T) {
	s, _ := newTestStore(t)
	crash := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Crash test"), Schedule: "every 1h"})
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(naiveAgo(5 * time.Minute)) })
	if due := mustDue(t, s); len(due) != 1 {
		t.Fatalf("due before %d", len(due))
	}
	s.advanceNextRun(context.Background(), crash.ID)
	if _, ok, err := s.claimJobForFire(context.Background(), crash.ID, FireClaimOptions{}); !ok || err != nil {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("Job should not be due after advance + claim: %v", jobIDs(due))
	}
}

// Recurring job pushed late by the given amount with an optional recent last run, returning the due ids
func lateRecurring(t *testing.T, schedule string, late, lastRun time.Duration) (*Store, *Job, []string) {
	t.Helper()
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Due"), Schedule: schedule})
	editJobs(t, s, func(j *Job) {
		j.NextRunAt = strPtr(naiveAgo(late))
		if lastRun > 0 {
			j.LastRunAt = strPtr(FormatISO(s.clk.now().Add(-lastRun)))
		}
	})
	return s, job, jobIDs(mustDue(t, s))
}

func TestGetDueJobsPastDueWithinWindowReturned(t *testing.T) {
	_, job, due := lateRecurring(t, "every 1h", 10*time.Minute, 0)
	if len(due) != 1 || due[0] != job.ID {
		t.Fatalf("due %v", due)
	}
}

func TestGetDueJobsStalePastDueRunsOnceAndFastForwards(t *testing.T) {
	s, job, due := lateRecurring(t, "every 1h", 35*time.Minute, 0)
	if len(due) != 1 || due[0] != job.ID {
		t.Fatalf("due %v", due)
	}
	if !inFuture(t, s, mustGet(t, s, job.ID).NextRunAt) {
		t.Fatal("stale job not fast forwarded")
	}
}

func TestGetDueJobsLongExecutionDoesNotPerpetuallyDefer(t *testing.T) {
	s, job, due := lateRecurring(t, "every 5m", 11*time.Minute, time.Minute)
	if len(due) != 1 || due[0] != job.ID {
		t.Fatalf("long-execution job was skipped (perpetual-defer bug): %v", due)
	}
	if !inFuture(t, s, mustGet(t, s, job.ID).NextRunAt) {
		t.Fatal("next_run_at not fast forwarded")
	}
}

func TestGetDueJobsIdlessJobDoesNotCrashOrBlockSiblingJobs(t *testing.T) {
	s, _ := newTestStore(t)
	healthy := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Healthy"), Schedule: "every 1h"})
	jobs, _ := s.LoadJobs(context.Background())
	jobs[0].NextRunAt = strPtr(naiveAgo(35 * time.Minute))
	seed := []map[string]any{jobToMap(t, jobs[0]), {"name": "idless-job", "schedule": map[string]any{"kind": "cron", "expr": "0 4 * * *"},
		"enabled": true, "no_agent": true, "next_run_at": nil}}
	seedJobs(t, s, seed...)
	if !containsString(jobIDs(mustDue(t, s)), healthy.ID) {
		t.Fatal("healthy sibling starved by an id less record")
	}
}

func jobToMap(t *testing.T, j *Job) map[string]any {
	t.Helper()
	b, err := j.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBrokenRecentOneshotWithoutNextRunIsRecovered(t *testing.T) {
	now := time.Date(2026, 3, 18, 4, 22, 30, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	runAt := "2026-03-18T04:22:00+00:00"
	seedJobs(t, s, map[string]any{"id": "oneshot-recover", "name": "Recover me", "prompt": "Word of the day",
		"schedule":         map[string]any{"kind": "once", "run_at": runAt, "display": "once at 2026-03-18 04:22"},
		"schedule_display": "once at 2026-03-18 04:22", "repeat": map[string]any{"times": 1, "completed": 0},
		"enabled": true, "state": "scheduled", "paused_at": nil, "paused_reason": nil,
		"created_at": "2026-03-18T04:21:00+00:00", "next_run_at": nil, "last_run_at": nil,
		"last_status": nil, "last_error": nil, "deliver": "local", "origin": nil})
	if due := mustDue(t, s); len(due) != 1 || due[0].ID != "oneshot-recover" {
		t.Fatalf("due %v", jobIDs(due))
	}
	got := mustGet(t, s, "oneshot-recover")
	if deref(got.NextRunAt) != runAt || got.RunClaim == nil || got.RunClaim.At != FormatISO(now) {
		t.Fatalf("recovered next %s claim %+v want at %s", deref(got.NextRunAt), got.RunClaim, FormatISO(now))
	}
}

func TestOneshotNotRedispatchedWhileRunning(t *testing.T) {
	t0 := time.Now().UTC()
	at := t0
	s := newClockStore(t, &at, nil)
	s.clk.nowFn = func() time.Time { return at }
	runAt := FormatISO(t0.Add(-5 * time.Second))
	seedJobs(t, s, map[string]any{"id": "long-oneshot", "name": "R", "prompt": "2.5min research",
		"schedule": map[string]any{"kind": "once", "run_at": runAt}, "next_run_at": runAt, "enabled": true, "state": "scheduled"})
	if due := mustDue(t, s); len(due) != 1 || due[0].ID != "long-oneshot" {
		t.Fatalf("first tick %v", jobIDs(due))
	}
	if mustGet(t, s, "long-oneshot").RunClaim == nil {
		t.Fatal("no run claim stamped")
	}
	for _, gap := range []int{28, 61, 130} {
		at = t0.Add(time.Duration(gap) * time.Second)
		if due := mustDue(t, s); len(due) != 0 {
			t.Fatalf("double dispatched at +%ds", gap)
		}
	}
}

func TestGetDueJobsRunClaimHeartbeatKeepsLongRunClaimedPastTtl(t *testing.T) {
	t.Setenv("ICEBATH_CRON_TIMEOUT", "")
	os.Unsetenv("ICEBATH_CRON_TIMEOUT")
	t0 := time.Now().UTC()
	at := t0
	s := newClockStore(t, &at, nil)
	s.clk.nowFn = func() time.Time { return at }
	ttl := s.oneshotRunClaimTTL(context.Background())
	runAt := FormatISO(t0.Add(-5 * time.Second))
	seedJobs(t, s, map[string]any{"id": "slowrun", "name": "R", "prompt": "x", "schedule": map[string]any{"kind": "once", "run_at": runAt},
		"next_run_at": runAt, "enabled": true, "state": "scheduled", "repeat": map[string]any{"times": 1, "completed": 0}})
	if due := mustDue(t, s); len(due) != 1 {
		t.Fatalf("due %v", jobIDs(due))
	}
	if ok, err := s.claimDispatch(context.Background(), "slowrun"); !ok || err != nil {
		t.Fatalf("claimDispatch = %v, %v", ok, err)
	}
	at = t0.Add(time.Duration((ttl - 60) * float64(time.Second)))
	owner := mustGet(t, s, "slowrun").RunClaim.By
	if ok, _ := s.heartbeatRunClaim(context.Background(), "slowrun", owner); !ok {
		t.Fatal("heartbeat refused the live owner")
	}
	at = t0.Add(time.Duration((ttl + 10) * float64(time.Second)))
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("heartbeat claim expired %v", jobIDs(due))
	}
	mustGet(t, s, "slowrun")
	mustMark(t, s, "slowrun", true, MarkOptions{})
	got := mustGet(t, s, "slowrun")
	if got.State != "completed" || got.Enabled || deref(got.LastStatus) != "ok" {
		t.Fatalf("retired %+v", got)
	}
}

func TestHeartbeatRunClaimRejectsReplacedOwner(t *testing.T) {
	s, _ := newTestStore(t)
	future := FormatISO(time.Now().UTC().Add(time.Hour))
	original := FormatISO(time.Now().UTC())
	seedJobs(t, s, map[string]any{"id": "reclaimed", "name": "R", "prompt": "x", "schedule": map[string]any{"kind": "once", "run_at": future},
		"next_run_at": future, "enabled": true, "state": "scheduled", "run_claim": map[string]any{"at": original, "by": "new-owner"}})
	if ok, _ := s.heartbeatRunClaim(context.Background(), "reclaimed", "old-owner"); ok {
		t.Fatal("stale owner refreshed a newer claim")
	}
	if c := mustGet(t, s, "reclaimed").RunClaim; c == nil || *c != (Claim{At: original, By: "new-owner"}) {
		t.Fatalf("claim changed %+v", c)
	}
}

func TestMarkJobRunConcurrencyThreeConcurrentMarkJobRunNoOverwrites(t *testing.T) {
	s, _ := newTestStore(t)
	a := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Job A"), Schedule: "every 1h"})
	b := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Job B"), Schedule: "every 1h"})
	c := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Job C"), Schedule: "every 1h"})
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, run := range []struct {
		id      string
		success bool
		err     *string
	}{{a.ID, true, nil}, {b.ID, false, prompt("timeout")}, {c.ID, true, nil}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.markJobRun(context.Background(), run.id, run.success, MarkOptions{Error: run.err}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, want := range []struct {
		id, status, err string
	}{{a.ID, "ok", ""}, {b.ID, "error", "timeout"}, {c.ID, "ok", ""}} {
		got := mustGet(t, s, want.id)
		if deref(got.LastStatus) != want.status || deref(got.LastError) != want.err || got.LastRunAt == nil || got.Repeat.Completed != 1 {
			t.Fatalf("%s clobbered %+v", want.id, got)
		}
	}
}

func TestBadNextRunAtRecoveryBadNextRunAtDoesNotCrashOrBlockSiblingJobs(t *testing.T) {
	s, _ := newTestStore(t)
	past := FormatISO(time.Now().UTC().Add(-30 * time.Second))
	seedJobs(t, s,
		map[string]any{"id": "bad-next", "schedule": map[string]any{"kind": "interval", "minutes": 60},
			"next_run_at": "not-a-valid-iso-timestamp!!!", "enabled": true, "created_at": past},
		map[string]any{"id": "good-sibling", "schedule": map[string]any{"kind": "interval", "minutes": 5},
			"next_run_at": past, "enabled": true, "created_at": past})
	ids := jobIDs(mustDue(t, s))
	if !containsString(ids, "good-sibling") || containsString(ids, "bad-next") {
		t.Fatalf("due %v", ids)
	}
	repaired := mustGet(t, s, "bad-next")
	if repaired.NextRunAt != nil {
		if _, err := ParseISO(*repaired.NextRunAt); err != nil {
			t.Fatalf("repaired next run %q unparsable", *repaired.NextRunAt)
		}
	}
	if !containsString(jobIDs(mustDue(t, s)), "good-sibling") {
		t.Fatal("second tick starved the sibling")
	}
}

func TestPerJobScanContainmentUnforeseenPerJobExceptionDoesNotStarveSiblings(t *testing.T) {
	s, _ := newTestStore(t)
	past := FormatISO(time.Now().UTC().Add(-30 * time.Second))
	seedJobs(t, s,
		map[string]any{"id": "poison", "schedule": map[string]any{"kind": "interval", "minutes": 7},
			"next_run_at": past, "enabled": true, "created_at": past},
		map[string]any{"id": "good-sibling", "schedule": map[string]any{"kind": "interval", "minutes": 5},
			"next_run_at": past, "enabled": true, "created_at": past})
	real := dueScanGraceSeconds
	dueScanGraceSeconds = func(st *Store, job *Job) int {
		if job.Schedule != nil && job.Schedule.Minutes == 7 {
			panic(errors.New("simulated unforeseen malformed field"))
		}
		return real(st, job)
	}
	defer func() { dueScanGraceSeconds = real }()
	for tick := 0; tick < 2; tick++ {
		ids := jobIDs(mustDue(t, s))
		if !containsString(ids, "good-sibling") || containsString(ids, "poison") {
			t.Fatalf("tick %d due %v", tick, ids)
		}
	}
}

func oneshotRecord(times, completed int) map[string]any {
	return map[string]any{"id": "os1", "name": "one-shot", "enabled": true,
		"schedule": map[string]any{"kind": "once", "run_at": "2026-01-01T00:00:00+00:00"},
		"repeat":   map[string]any{"times": times, "completed": completed}}
}

func TestClaimDispatchClaimIncrementsAndPersists(t *testing.T) {
	s, _ := newTestStore(t)
	seedJobs(t, s, oneshotRecord(1, 0))
	if ok, err := s.claimDispatch(context.Background(), "os1"); !ok || err != nil {
		t.Fatalf("first claim refused %v", err)
	}
	if jobs, _ := s.LoadJobs(context.Background()); jobs[0].Repeat.Completed != 1 {
		t.Fatalf("completed %d", jobs[0].Repeat.Completed)
	}
}

func TestClaimDispatchAlreadyDispatchedOneshotIsRemoved(t *testing.T) {
	s, _ := newTestStore(t)
	seedJobs(t, s, oneshotRecord(1, 1))
	if ok, _ := s.claimDispatch(context.Background(), "os1"); ok {
		t.Fatal("already dispatched one shot claimed again")
	}
	if jobs, _ := s.LoadJobs(context.Background()); len(jobs) != 0 {
		t.Fatalf("dispatched one shot kept %v", jobIDs(jobs))
	}
}

func TestClaimDispatchMarkJobRunDoesNotDoubleCountPreclaimedOneshot(t *testing.T) {
	s, _ := newTestStore(t)
	seedJobs(t, s, oneshotRecord(1, 0))
	if ok, _ := s.claimDispatch(context.Background(), "os1"); !ok {
		t.Fatal("first claim refused")
	}
	if jobs, _ := s.LoadJobs(context.Background()); jobs[0].Repeat.Completed != 1 {
		t.Fatalf("completed %d", jobs[0].Repeat.Completed)
	}
	mustMark(t, s, "os1", true, MarkOptions{})
	jobs, _ := s.LoadJobs(context.Background())
	if len(jobs) != 1 || jobs[0].Repeat.Completed != 1 || jobs[0].State != "completed" || jobs[0].Enabled {
		t.Fatalf("pre claimed one shot double counted %+v", jobs)
	}
}

func TestClaimDispatchGetDueJobsRemovesStaleMaxedOneshot(t *testing.T) {
	s, _ := newTestStore(t)
	past := FormatISO(time.Now().UTC().Add(-5 * time.Second))
	seedJobs(t, s, map[string]any{"id": "os1", "name": "one-shot", "enabled": true, "schedule": map[string]any{"kind": "once", "run_at": past},
		"repeat": map[string]any{"times": 1, "completed": 1}, "next_run_at": nil})
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("maxed one shot due %v", jobIDs(due))
	}
	if jobs, _ := s.LoadJobs(context.Background()); len(jobs) != 0 {
		t.Fatalf("stale maxed one shot kept %v", jobIDs(jobs))
	}
}

func TestAdvanceNextRunsBatchAdvancesRecurringSkipsOneshots(t *testing.T) {
	s, _ := newTestStore(t)
	var rec, one []string
	for i := 0; i < 3; i++ {
		rec = append(rec, mustCreate(t, s, CreateJobOptions{Prompt: prompt("rec"), Schedule: "every 1h"}).ID)
	}
	one = append(one, mustCreate(t, s, CreateJobOptions{Prompt: prompt("one"), Schedule: "in 30m"}).ID)
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(naiveAgo(5 * time.Minute)) })
	n, err := s.advanceNextRuns(context.Background(), append(append([]string{}, rec...), one...))
	if err != nil || n != len(rec) {
		t.Fatalf("advanced %d, %v", n, err)
	}
	for _, id := range rec {
		if !inFuture(t, s, mustGet(t, s, id).NextRunAt) {
			t.Fatalf("%s not advanced", id)
		}
	}
	for _, id := range one {
		if inFuture(t, s, mustGet(t, s, id).NextRunAt) {
			t.Fatalf("one shot %s advanced", id)
		}
	}
}

func completedOneshot(t *testing.T, s *Store, age time.Duration) string {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Once"), Schedule: "in 30m", Repeat: 1})
	mustMark(t, s, job.ID, true, MarkOptions{DeliveryError: prompt("boom")})
	stamp := FormatISO(time.Now().UTC().Add(-age))
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.LastRunAt = strPtr(stamp)
		}
	})
	return job.ID
}

func TestCompletedOneshotRetentionSweepSweepPrunesOldCompletedOneshot(t *testing.T) {
	s, _ := newTestStore(t)
	old := completedOneshot(t, s, 30*24*time.Hour)
	mustDue(t, s)
	if j, _ := s.getJob(context.Background(), old); j != nil {
		t.Fatal("old completed one shot kept")
	}
}

func TestCompletedOneshotRetentionSweepSweepKeepsRecentCompletedOneshot(t *testing.T) {
	s, _ := newTestStore(t)
	recent := completedOneshot(t, s, 24*time.Hour)
	mustDue(t, s)
	if kept := mustGet(t, s, recent); kept.State != "completed" || deref(kept.LastDeliveryError) != "boom" {
		t.Fatalf("recent completed one shot %+v", kept)
	}
}

func TestCompletedOneshotRetentionSweepSweepIgnoresRecurringJobs(t *testing.T) {
	s, _ := newTestStore(t)
	recurring := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Recurring"), Schedule: "every 1h"})
	editJobs(t, s, func(j *Job) {
		if j.ID == recurring.ID {
			j.LastRunAt = strPtr(FormatISO(time.Now().UTC().Add(-365 * 24 * time.Hour)))
		}
	})
	mustDue(t, s)
	mustGet(t, s, recurring.ID)
}

func TestCompletedOneshotRetentionSweepSweepDisabledByNonpositiveRetention(t *testing.T) {
	s, _ := newTestStore(t)
	os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  completed_retention_days: 0.0\n"), 0o600)
	kept := completedOneshot(t, s, 30*24*time.Hour)
	mustDue(t, s)
	mustGet(t, s, kept)
}

func TestCompletedOneshotRetentionSweepRecurringJobsUnaffectedByRetentionChange(t *testing.T) {
	s, _ := newTestStore(t)
	recurring := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Recurring"), Schedule: "every 1h"})
	completedOneshot(t, s, 24*time.Hour)
	mustMark(t, s, recurring.ID, true, MarkOptions{})
	if got := mustGet(t, s, recurring.ID); !got.Enabled || got.State != "scheduled" || got.NextRunAt == nil {
		t.Fatalf("recurring job affected by retention %+v", got)
	}
}
