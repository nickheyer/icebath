package cron

import (
	"math"
	"reflect"
	"testing"
	"time"
)

var latenessFixedNow = time.Date(2026, 9, 1, 9, 31, 0, 0, time.UTC)

func latenessStore(t *testing.T) *Store {
	t.Helper()
	now := latenessFixedNow
	return newClockStore(t, &now, time.UTC)
}

func isoUTC(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05") + "+00:00" }

func latenessJob(id string, schedule map[string]any, nextRun time.Time, extra map[string]any) map[string]any {
	job := map[string]any{
		"id": id, "name": id, "prompt": "x", "schedule": schedule, "next_run_at": isoUTC(nextRun), "last_run_at": nil,
		"enabled": true, "state": "scheduled", "repeat": map[string]any{"times": nil, "completed": 0}, "deliver": "local",
	}
	for k, v := range extra {
		job[k] = v
	}
	return job
}

func dailyLatenessJob(id string, nextRun time.Time, extra map[string]any) map[string]any {
	return latenessJob(id, map[string]any{"kind": "cron", "expr": "0 9 * * *"}, nextRun, extra)
}

func intervalLatenessJob(id string, nextRun time.Time, extra map[string]any) map[string]any {
	return latenessJob(id, map[string]any{"kind": "interval", "minutes": 60}, nextRun, extra)
}

func TestDueScanDispatchStampCatchUpBeyondGraceStampedAndPersisted(t *testing.T) {
	s := latenessStore(t)
	scheduled := latenessFixedNow.Add(-(24*time.Hour + 31*time.Minute))
	seedJobs(t, s, dailyLatenessJob("daily", scheduled, nil))
	due := mustDue(t, s)
	if !reflect.DeepEqual(jobIDs(due), []string{"daily"}) {
		t.Fatalf("due %v", jobIDs(due))
	}
	stamp := due[0].LastDispatch
	if stamp == nil || stamp.Kind != "catch_up" || stamp.ScheduledAt != isoUTC(scheduled) || stamp.DispatchedAt != isoUTC(latenessFixedNow) {
		t.Fatalf("stamp %+v", stamp)
	}
	if expected := latenessFixedNow.Sub(scheduled).Seconds(); math.Abs(stamp.LatenessSeconds-expected) > 1 {
		t.Fatalf("lateness %v want %v", stamp.LatenessSeconds, expected)
	}
	persisted := storedJobs(t, s)
	if len(persisted) != 1 || persisted[0].LastDispatch == nil || *persisted[0].LastDispatch != *stamp {
		t.Fatalf("persisted %+v", persisted[0].LastDispatch)
	}
}

func TestDueScanDispatchStampLateWithinGraceStampedLate(t *testing.T) {
	s := latenessStore(t)
	seedJobs(t, s, dailyLatenessJob("daily", latenessFixedNow.Add(-31*time.Minute), nil))
	stamp := mustDue(t, s)[0].LastDispatch
	if stamp == nil || stamp.Kind != "late" || math.Abs(stamp.LatenessSeconds-31*60) > 1 {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestDueScanDispatchStampOnTimeDispatchStampedOnTime(t *testing.T) {
	s := latenessStore(t)
	seedJobs(t, s, intervalLatenessJob("hourly", latenessFixedNow.Add(-30*time.Second), nil))
	stamp := mustDue(t, s)[0].LastDispatch
	if stamp == nil || stamp.Kind != "on_time" || math.Abs(stamp.LatenessSeconds-30) > 1 {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestDueScanDispatchStampManualTriggerNotStamped(t *testing.T) {
	s := latenessStore(t)
	runAt := latenessFixedNow.Add(-5 * time.Hour)
	seedJobs(t, s, dailyLatenessJob("manual", runAt, map[string]any{"manual_run_at": isoUTC(runAt)}))
	due := mustDue(t, s)
	if !reflect.DeepEqual(jobIDs(due), []string{"manual"}) || due[0].LastDispatch != nil {
		t.Fatalf("due %v stamp %+v", jobIDs(due), due[0].LastDispatch)
	}
}

func TestDueScanDispatchStampNewDispatchOverwritesStaleStamp(t *testing.T) {
	s := latenessStore(t)
	scheduled := latenessFixedNow.Add(-10 * time.Second)
	stale := map[string]any{"scheduled_at": "2026-08-30T09:00:00+00:00", "dispatched_at": "2026-08-30T11:00:00+00:00", "lateness_seconds": 7200.0, "kind": "catch_up"}
	seedJobs(t, s, intervalLatenessJob("hourly", scheduled, map[string]any{"last_dispatch": stale}))
	stamp := mustDue(t, s)[0].LastDispatch
	if stamp == nil || stamp.Kind != "on_time" || stamp.ScheduledAt != isoUTC(scheduled) {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestClassifyDispatchLatenessWithinToleranceIsOnTime(t *testing.T) {
	if classifyDispatchLateness(0, 7200) != "on_time" || classifyDispatchLateness(299, 7200) != "on_time" {
		t.Fatal("within tolerance is not on time")
	}
}

func TestClassifyDispatchLatenessBeyondToleranceWithinGraceIsLate(t *testing.T) {
	if classifyDispatchLateness(301, 7200) != "late" || classifyDispatchLateness(7200, 7200) != "late" {
		t.Fatal("beyond tolerance within grace is not late")
	}
}

func TestClassifyDispatchLatenessBeyondGraceIsCatchUp(t *testing.T) {
	if got := classifyDispatchLateness(7201, 7200); got != "catch_up" {
		t.Fatal(got)
	}
}
