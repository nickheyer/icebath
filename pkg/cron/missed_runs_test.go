package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetMissedHooks(t *testing.T) {
	t.Helper()
	testHooks.missed = nil
	saved := recordCronMissed
	recordCronMissed = func(_ context.Context, job *Job) { testHooks.missed = append(testHooks.missed, job.ID) }
	t.Cleanup(func() { testHooks.missed, recordCronMissed = nil, saved })
}

func TestStaleRecurringRunsOnceAndBooksCollapsedSlots(t *testing.T) {
	resetMissedHooks(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h"})
	first := FormatISO(now.Add(-3*time.Hour - 40*time.Minute))
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(first) })
	if due := mustDue(t, s); len(due) != 1 {
		t.Fatalf("stale recurring job skipped %v", jobIDs(due))
	}
	if s.CatchUpOccurrenceCount() != 1 {
		t.Fatalf("catch up count %d", s.CatchUpOccurrenceCount())
	}
	if len(bookedMissed(t, s)) != 1 || bookedMissed(t, s)[0] != job.ID+":3:collapsed into one catch-up run" {
		t.Fatalf("booked %v", bookedMissed(t, s))
	}
	if !inFuture(t, s, mustGet(t, s, job.ID).NextRunAt) {
		t.Fatal("not fast forwarded")
	}
}

func TestCatchUpOffSkipsTheMissAndBooksIt(t *testing.T) {
	resetMissedHooks(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  catch_up_missed: false\n"), 0o600)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h"})
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(FormatISO(now.Add(-90 * time.Minute))) })
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("catch up off still fired %v", jobIDs(due))
	}
	if len(testHooks.missed) != 1 || !strings.HasPrefix(bookedMissed(t, s)[0], job.ID+":2:skipped, catch-up off; next run ") {
		t.Fatalf("missed %v booked %v", testHooks.missed, bookedMissed(t, s))
	}
	on := true
	if !s.jobCatchesUp(&Job{CatchUp: &on}) || s.jobCatchesUp(&Job{Schedule: &Schedule{Kind: "once"}}) {
		t.Fatal("explicit catch_up or one shot rule ignored")
	}
	grace := 5
	os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  catch_up_missed: 0\n"), 0o600)
	if !s.jobCatchesUp(&Job{Schedule: &Schedule{Kind: "cron"}}) || s.jobCatchesUp(&Job{Schedule: &Schedule{Kind: "cron"}, GraceMinutes: &grace}) {
		t.Fatal("non boolean config or explicit grace rule ignored")
	}
}

func TestCountMissedSlotsStopsAtNowAndCap(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	sched := mustParse(t, s, "every 1m")
	n, err := s.countMissedSlots(sched, FormatISO(now.Add(-10*time.Minute)), now, nil)
	if err != nil || n != 11 {
		t.Fatalf("counted %d, %v", n, err)
	}
	if n, _ := s.countMissedSlots(sched, FormatISO(now.Add(-30*24*time.Hour)), now, nil); n != maxCountedSlots {
		t.Fatalf("cap %d", n)
	}
	if n, _ := s.countMissedSlots(sched, "garbage", now, nil); n != 0 {
		t.Fatalf("garbage %d", n)
	}
}

func TestExpiredOneshotRetiredWithDiagnostic(t *testing.T) {
	resetMissedHooks(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	runAt := FormatISO(now.Add(-10 * time.Minute))
	seedJobs(t, s,
		map[string]any{"id": "late1", "name": "late", "prompt": "x", "schedule": map[string]any{"kind": "once", "run_at": runAt},
			"next_run_at": runAt, "enabled": true, "state": "scheduled", "repeat": map[string]any{"times": 1, "completed": 0}},
		map[string]any{"id": "late2", "name": "outage", "prompt": "x", "schedule": map[string]any{"kind": "once", "run_at": runAt},
			"next_run_at": runAt, "enabled": true, "state": "scheduled", "run_claim": map[string]any{"at": runAt, "by": "x", "outage": true}})
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("expired one shots fired %v", jobIDs(due))
	}
	if j, _ := s.getJob("late1"); j != nil {
		t.Fatal("expired one shot kept")
	}
	mustGet(t, s, "late2")
	out, _ := filepath.Glob(filepath.Join(s.outputDir, "late1", "*.md"))
	if len(out) != 1 {
		t.Fatalf("diagnostics %v", out)
	}
	text, _ := os.ReadFile(out[0])
	for _, want := range []string{"# Cron job removed before firing", "- name: late\n", "- scheduled run time: " + runAt, "- grace window: 120s"} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("diagnostic lacks %q\n%s", want, text)
		}
	}
	if len(testHooks.missed) != 1 || bookedMissed(t, s)[0] != "late1:1:one-shot past its grace window; catch-up off" {
		t.Fatalf("missed %v booked %v", testHooks.missed, bookedMissed(t, s))
	}
}

func TestWedgedDiagnosticOnlyWithoutARecordedRun(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	times := 1
	writeWedged(context.Background(), s, &Job{ID: "w1", Name: "wedge", Repeat: &Repeat{Times: &times, Completed: 1}, RunClaim: &Claim{At: "t0"}})
	writeWedged(context.Background(), s, &Job{ID: "w2", LastRunAt: strPtr("t")})
	out, _ := filepath.Glob(filepath.Join(s.outputDir, "*", "*.md"))
	if len(out) != 1 || filepath.Base(filepath.Dir(out[0])) != "w1" {
		t.Fatalf("diagnostics %v", out)
	}
	text, _ := os.ReadFile(out[0])
	if !strings.Contains(string(text), "- dispatch claimed: 1/1\n- run claimed at: t0 by unknown\n") {
		t.Fatalf("diagnostic %s", text)
	}
}

func TestOccurrenceIdentityAndPendingSlots(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	if got := scheduledInstant(strPtr("2026-05-01T05:00:00-07:00")); deref(got) != "2026-05-01T12:00:00+00:00" {
		t.Fatalf("instant %v", deref(got))
	}
	if scheduledInstant(strPtr("2026-05-01T05:00:00")) != nil || scheduledInstant(strPtr("junk")) != nil || scheduledInstant(nil) != nil {
		t.Fatal("naive or junk instant accepted")
	}
	occRun, err := openExecutionsLedger(s).Create(context.Background(), "occ", "cron", strPtr("2026-05-01T05:00:00-07:00"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openExecutionsLedger(s).Finish(context.Background(), recText(occRun, "id"), true, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if !completedOccurrence(s, &Job{ID: "occ"}, "2026-05-01T05:00:00-07:00") || completedOccurrence(s, &Job{ID: "other"}, "2026-05-01T12:00:00+00:00") {
		t.Fatal("completed occurrence lookup")
	}
	stamp := pendingSlotStamp("2026-05-01T11:00:00+00:00", now)
	if stamp.ScheduledAt != "2026-05-01T11:00:00+00:00" || stamp.At != FormatISO(now) || stamp.By != machineID() {
		t.Fatalf("stamp %+v", stamp)
	}
	job := &Job{ID: "p", Schedule: &Schedule{Kind: "interval"}, PendingSlot: stamp}
	if deref(unclaimedPendingSlot(s, job, now)) != stamp.ScheduledAt {
		t.Fatal("own pending slot not restorable")
	}
	job.PendingSlot = &PendingSlot{ScheduledAt: stamp.ScheduledAt, At: FormatISO(now), By: "other-host:1"}
	if unclaimedPendingSlot(s, job, now) != nil {
		t.Fatal("live foreign pending slot taken")
	}
	job.PendingSlot.At = FormatISO(now.Add(-time.Hour))
	if unclaimedPendingSlot(s, job, now) == nil {
		t.Fatal("expired foreign pending slot kept")
	}
	if unclaimedPendingSlot(s, &Job{Schedule: &Schedule{Kind: "once"}, PendingSlot: stamp}, now) != nil {
		t.Fatal("one shot pending slot")
	}
	os.WriteFile(filepath.Join(s.cronDir, "catch_up_occurrences"), []byte("\xef\xbb\xbf 41\n"), 0o600)
	s.recordCatchUpOccurrence(context.Background())
	if s.CatchUpOccurrenceCount() != 42 {
		t.Fatalf("count %d", s.CatchUpOccurrenceCount())
	}
}
