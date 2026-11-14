package cron

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func makeClaimedOneshot(t *testing.T, s *Store, claimed bool) *Job {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("remind me"), Schedule: "in 30m"})
	if claimed {
		editJobs(t, s, func(j *Job) {
			if j.ID == job.ID {
				j.RunClaim = &Claim{At: "2026-08-17T10:00:00+00:00", By: "test:1"}
			}
		})
	}
	return job
}

func clearClaim(t *testing.T, s *Store, id string) bool {
	t.Helper()
	ok, err := s.clearRunClaim(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestClearRunClaimClearsClaimOnOneshot(t *testing.T) {
	s, _ := newTestStore(t)
	job := makeClaimedOneshot(t, s, true)
	if !clearClaim(t, s, job.ID) {
		t.Fatal("the claim was not cleared")
	}
	if reloaded := storedJob(t, s, job.ID); reloaded.RunClaim != nil {
		t.Fatalf("run_claim %+v", reloaded.RunClaim)
	}
}

func TestClearRunClaimNoopWhenAlreadyClear(t *testing.T) {
	s, _ := newTestStore(t)
	job := makeClaimedOneshot(t, s, false)
	if clearClaim(t, s, job.ID) {
		t.Fatal("a clear record reported a cleared claim")
	}
}

func TestClearRunClaimNeverTouchesRecurringJobs(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("tick"), Schedule: "every 10m"})
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.RunClaim = &Claim{At: "2026-08-17T10:00:00+00:00", By: "test:1"}
		}
	})
	if clearClaim(t, s, job.ID) {
		t.Fatal("a recurring job's claim was cleared")
	}
	if reloaded := storedJob(t, s, job.ID); reloaded.RunClaim == nil {
		t.Fatal("the recurring job's claim was touched")
	}
}

func TestClearRunClaimUnknownJobIdReturnsFalse(t *testing.T) {
	s, _ := newTestStore(t)
	if clearClaim(t, s, "no-such-job") {
		t.Fatal("an unknown job reported a cleared claim")
	}
}

var graceFixedNow = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

func graceStore(t *testing.T) *Store {
	t.Helper()
	now := graceFixedNow
	return newClockStore(t, &now, time.UTC)
}

func graceOneshot(id string, runAt time.Time, completed int, runClaim any) map[string]any {
	return map[string]any{
		"id": id, "name": id, "prompt": "x", "schedule": map[string]any{"kind": "once", "run_at": isoUTC(runAt)},
		"next_run_at": isoUTC(runAt), "last_run_at": nil, "enabled": true, "state": "scheduled",
		"repeat": map[string]any{"times": 1, "completed": completed}, "deliver": "local", "run_claim": runClaim, "fire_claim": nil,
	}
}

func TestOneShotGraceDueScanStaleBeyondGraceNotDueAndRetiredWithDiagnostic(t *testing.T) {
	s := graceStore(t)
	seedJobs(t, s, graceOneshot("stale", graceFixedNow.Add(-3*time.Hour), 0, nil))
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("due %v", jobIDs(due))
	}
	if stored := storedJobs(t, s); len(stored) != 0 {
		t.Fatalf("stored %v", jobIDs(stored))
	}
	diagnostics, err := filepath.Glob(filepath.Join(s.cronDir, "output", "stale", "*.md"))
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("expected a diagnostic file for the retired stale one-shot, got %v %v", diagnostics, err)
	}
}

func TestOneShotGraceDueScanWithinGraceStillDue(t *testing.T) {
	s := graceStore(t)
	seedJobs(t, s, graceOneshot("recent", graceFixedNow.Add(-60*time.Second), 0, nil))
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"recent"}) {
		t.Fatalf("due %v", jobIDs(due))
	}
	if stored := storedJobs(t, s); !reflect.DeepEqual(jobIDs(stored), []string{"recent"}) {
		t.Fatalf("stored %v", jobIDs(stored))
	}
}

func TestOneShotGraceDueScanStaleWithClaimSkippedButRecordKept(t *testing.T) {
	s := graceStore(t)
	runAt := graceFixedNow.Add(-3 * time.Hour)
	seedJobs(t, s, graceOneshot("claimed", runAt, 1, map[string]any{"at": isoUTC(runAt), "by": "other"}))
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("due %v", jobIDs(due))
	}
	if stored := storedJobs(t, s); !reflect.DeepEqual(jobIDs(stored), []string{"claimed"}) {
		t.Fatalf("stored %v", jobIDs(stored))
	}
}

func TestOneShotGraceDueScanRetriggeredStaleOneshotIsDue(t *testing.T) {
	s := graceStore(t)
	seedJobs(t, s, graceOneshot("stale", graceFixedNow.Add(-3*time.Hour), 0, nil))
	if _, err := s.triggerJob(context.Background(), "stale", ""); err != nil {
		t.Fatal(err)
	}
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"stale"}) {
		t.Fatalf("due %v", jobIDs(due))
	}
}

func TestOneShotGraceDueScanRecurringJobsUnaffected(t *testing.T) {
	s := graceStore(t)
	seedJobs(t, s, map[string]any{
		"id": "int", "name": "int", "prompt": "x", "schedule": map[string]any{"kind": "interval", "minutes": 60},
		"next_run_at": isoUTC(graceFixedNow.Add(-2 * time.Hour)), "last_run_at": nil, "enabled": true, "state": "scheduled",
		"repeat": map[string]any{"times": nil, "completed": 0}, "deliver": "local",
	})
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"int"}) {
		t.Fatalf("due %v", jobIDs(due))
	}
}

func TestGuardWarnsOnRearmedConsumedRecord(t *testing.T) {
	s, logs := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: s.clk.now().Add(time.Hour).Format("2006-01-02T15:04:05.000000-07:00"),
		Name: strPtr("warn-guard"), Deliver: strPtr("local")})
	if ok, err := s.claimDispatch(t.Context(), job.ID); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	mustMark(t, s, job.ID, true, MarkOptions{})
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.Enabled, j.State = true, "scheduled"
			j.NextRunAt = strPtr(s.clk.now().Add(-5 * time.Second).Format("2006-01-02T15:04:05.000000-07:00"))
		}
	})
	logs.Reset()
	due := mustDue(t, s)
	for _, d := range due {
		if d.ID == job.ID {
			t.Fatal("the consumed one shot fired again")
		}
	}
	for _, stored := range storedJobs(t, s) {
		if stored.ID == job.ID {
			t.Fatal("the consumed one shot was left in the store")
		}
	}
	warned := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "WITHOUT firing") {
			warned = true
			if !strings.Contains(line, "cron resume") {
				t.Fatalf("warning without the remediation hint: %s", line)
			}
			break
		}
	}
	if !warned {
		t.Fatalf("expected WARNING on removal of a re-armed consumed one-shot, got %s", logs.String())
	}
}
