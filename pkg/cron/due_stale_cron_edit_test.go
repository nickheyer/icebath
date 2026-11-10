package cron

import (
	"context"
	"strings"
	"testing"
	"time"
)

var (
	saturday0700 = time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	monday0700   = time.Date(2026, 8, 24, 7, 0, 0, 0, time.UTC)
)

// Store in the UTC zone reading the settable instant
func staleEditStore(t *testing.T, at time.Time) (*Store, *time.Time) {
	t.Helper()
	now := at
	s := newClockStore(t, nil, time.UTC)
	s.clk.nowFn = func() time.Time { return now }
	return s, &now
}

// Persists a cron job whose next_run_at was computed under another expression
func writeStaleCronJob(t *testing.T, s *Store, expr string, nextRunAt time.Time) string {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("t")})
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.Schedule = &Schedule{Kind: "cron", Expr: expr}
			j.NextRunAt = strPtr(FormatISO(nextRunAt))
		}
	})
	return job.ID
}

func dueHas(t *testing.T, s *Store, id string) bool {
	t.Helper()
	for _, j := range mustDue(t, s) {
		if j.ID == id {
			return true
		}
	}
	return false
}

func TestStaleNextRunOnExcludedDowDoesNotFire(t *testing.T) {
	s, _ := staleEditStore(t, saturday0700.Add(30*time.Second))
	id := writeStaleCronJob(t, s, "0 7 * * 1-5", saturday0700)
	if dueHas(t, s, id) {
		t.Fatal("stale Saturday instant fired a weekday schedule")
	}
	if got := deref(mustGet(t, s, id).NextRunAt); !strings.HasPrefix(got, FormatISO(monday0700)) {
		t.Fatalf("re anchored to %s", got)
	}
}

func TestMatchingNextRunStillFires(t *testing.T) {
	s, _ := staleEditStore(t, saturday0700.Add(30*time.Second))
	id := writeStaleCronJob(t, s, "0 7 * * *", saturday0700)
	if !dueHas(t, s, id) {
		t.Fatal("matching instant did not fire")
	}
}

func TestManualTriggerBypassesStaleScheduleGuard(t *testing.T) {
	s, _ := staleEditStore(t, saturday0700.Add(30*time.Second))
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "0 7 * * 1-5", Name: prompt("manual")})
	triggered, err := s.triggerJob(context.Background(), job.ID, "")
	if err != nil || triggered == nil {
		t.Fatalf("trigger %v %v", triggered, err)
	}
	if !dueHas(t, s, job.ID) {
		t.Fatal("run now instant was refused by the stale schedule guard")
	}
	if deref(triggered.ManualRunAt) != deref(triggered.NextRunAt) || triggered.ManualRunAt == nil {
		t.Fatalf("manual_run_at %v next_run_at %v", deref(triggered.ManualRunAt), deref(triggered.NextRunAt))
	}
	mustMark(t, s, job.ID, true, MarkOptions{})
	if mustGet(t, s, job.ID).ManualRunAt != nil {
		t.Fatal("manual_run_at survived the run")
	}
}

func TestDelayedManualTriggerIsNotCountedAsCatchUp(t *testing.T) {
	trigger := saturday0700.Add(30 * time.Second)
	s, now := staleEditStore(t, trigger)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "0 7 * * 1-5", Name: prompt("delayed")})
	if _, err := s.triggerJob(context.Background(), job.ID, ""); err != nil {
		t.Fatal(err)
	}
	*now = trigger.Add(5 * time.Hour)
	if !dueHas(t, s, job.ID) {
		t.Fatal("delayed run now did not fire")
	}
	if got := s.CatchUpOccurrenceCount(); got != 0 {
		t.Fatalf("run now counted as %d catch ups", got)
	}
}

func TestManualTriggerSurvivesTimezoneChangeBeforeTick(t *testing.T) {
	trigger := time.Date(2026, 8, 22, 21, 0, 0, 0, time.FixedZone("", 10*3600))
	scan := time.Date(2026, 8, 22, 13, 0, 0, 0, time.FixedZone("", 2*3600))
	s, now := staleEditStore(t, trigger)
	s.clk.zoneFn = func() *time.Location { return trigger.Location() }
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "0 7 * * 1-5", Name: prompt("tz-manual")})
	if _, err := s.triggerJob(context.Background(), job.ID, ""); err != nil {
		t.Fatal(err)
	}
	*now = scan
	s.clk.zoneFn = func() *time.Location { return scan.Location() }
	if !dueHas(t, s, job.ID) {
		t.Fatal("zone migration repair replaced the run now instant")
	}
}

func TestStaleNextRunSkipsEvenInsideCatchupWindow(t *testing.T) {
	s, _ := staleEditStore(t, saturday0700.Add(5*time.Hour))
	id := writeStaleCronJob(t, s, "0 7 * * 1-5", saturday0700)
	if dueHas(t, s, id) {
		t.Fatal("catch up resurrected a stale instant")
	}
	if got := deref(mustGet(t, s, id).NextRunAt); !strings.HasPrefix(got, FormatISO(monday0700)) {
		t.Fatalf("re anchored to %s", got)
	}
}

func TestHelperReportsMatchForNonCronAndUnvalidatable(t *testing.T) {
	cases := []struct {
		sched *Schedule
		want  bool
	}{
		{&Schedule{Kind: "interval"}, true},
		{&Schedule{Kind: "cron"}, true},
		{&Schedule{Kind: "cron", Expr: "0 7 * * *"}, true},
		{&Schedule{Kind: "cron", Expr: "0 7 * * 1-5"}, false},
	}
	for _, tc := range cases {
		if got := cronNextRunMatchesExpr(tc.sched, saturday0700); got != tc.want {
			t.Errorf("%+v matches=%v want %v", tc.sched, got, tc.want)
		}
	}
}

func TestStaleEditWithoutManualMarkerStillReanchors(t *testing.T) {
	s, _ := staleEditStore(t, saturday0700.Add(5*time.Minute))
	id := writeStaleCronJob(t, s, "0 7 * * 1-5", saturday0700)
	if mustGet(t, s, id).ManualRunAt != nil {
		t.Fatal("fixture carries a manual marker")
	}
	if dueHas(t, s, id) {
		t.Fatal("stale edit fired")
	}
	after := mustGet(t, s, id)
	if deref(after.NextRunAt) == FormatISO(saturday0700) || after.ManualRunAt != nil {
		t.Fatalf("after %v %v", deref(after.NextRunAt), after.ManualRunAt)
	}
}
