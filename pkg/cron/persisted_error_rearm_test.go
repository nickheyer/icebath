package cron

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func resetErrorRecoveryStats(t *testing.T) {
	t.Helper()
	reset := func() {
		telemetryMu.Lock()
		persistedErrorRecoveries, persistedErrorRecoveriesRecent = 0, nil
		telemetryMu.Unlock()
		cadenceMu.Lock()
		cadenceCache = map[string]*float64{}
		cadenceMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func errorRecoveries() int {
	return PersistedErrorRecoveryStats()["persisted_error_recoveries"].(int)
}

func wedgeJob(t *testing.T, s *Store, id string, nextRun, lastRun time.Time) {
	t.Helper()
	if _, err := s.updateJob(context.Background(), id, map[string]any{
		"next_run_at": isoUTC(nextRun), "last_status": "error",
		"last_error": "EAGAIN [Errno 11] Resource temporarily unavailable", "last_run_at": isoUTC(lastRun),
	}); err != nil {
		t.Fatal(err)
	}
}

func legalityStore(t *testing.T) *Store {
	t.Helper()
	resetErrorRecoveryStats(t)
	return newClockStore(t, nil, time.UTC)
}

func TestCronRearmRespectsScheduleLegalityWeekdayJobErroredFridayDoesNotFireOnSaturday(t *testing.T) {
	s := legalityStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("weekday report"), Schedule: "0 9 * * 1-5"})
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	wedgeJob(t, s, job.ID, time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC), time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC))
	setNow(s, now)
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("Saturday must not fire a weekday-only job, due %v", jobIDs(due))
	}
	next, ok := s.clk.parseAware(deref(mustGet(t, s, job.ID).NextRunAt), time.UTC)
	if !ok || !next.Equal(time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected re-arm to Monday 9am, got %v", deref(mustGet(t, s, job.ID).NextRunAt))
	}
	if errorRecoveries() < 1 {
		t.Fatal("the re-arm was not counted")
	}
}

func TestCronRearmRespectsScheduleLegalityCronValueParkedAtNextLegalOccurrenceLeftAlone(t *testing.T) {
	s := legalityStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("daily"), Schedule: "30 14 * * *"})
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	legalNext := time.Date(2026, 8, 22, 14, 30, 0, 0, time.UTC)
	wedgeJob(t, s, job.ID, legalNext, now.Add(-27*time.Hour))
	setNow(s, now)
	mustDue(t, s)
	next, ok := s.clk.parseAware(deref(mustGet(t, s, job.ID).NextRunAt), time.UTC)
	if !ok || !next.Equal(legalNext) || errorRecoveries() != 0 {
		t.Fatalf("next %v recoveries %d", deref(mustGet(t, s, job.ID).NextRunAt), errorRecoveries())
	}
}

func TestCronRearmRespectsScheduleLegalityIntervalJobStillRearmsToNow(t *testing.T) {
	s := legalityStore(t)
	writeProbeScript(t, s, "p.py")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("probe"), Schedule: "every 10m", NoAgent: true, Script: "p.py"})
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	wedgeJob(t, s, job.ID, now.Add(5*time.Minute), now.Add(-110*time.Minute))
	setNow(s, now)
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{job.ID}) {
		t.Fatalf("wedged interval job must become due immediately after re-arm, due %v", jobIDs(due))
	}
}

func writeProbeScript(t *testing.T, s *Store, name string) {
	t.Helper()
	path := filepath.Join(s.home, "scripts", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func staleErrorProbe(t *testing.T) (*Store, string) {
	t.Helper()
	resetErrorRecoveryStats(t)
	s, _ := newTestStore(t)
	writeProbeScript(t, s, "probe.py")
	t.Setenv("HOME", filepath.Dir(s.home))
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("probe"), Schedule: "every 10m", NoAgent: true, Script: "probe.py"})
	useRealTickSeams(t)
	return s, job.ID
}

func persistStaleError(t *testing.T, s *Store, id string, errorAge time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	wedgeJob(t, s, id, now.Add(5*time.Minute), now.Add(-errorAge))
}

func tickOnce(t *testing.T, s *Store) {
	t.Helper()
	if _, err := schedulerTick(context.Background(), s, tickOptions{Verbose: false, Sync: true}); err != nil {
		t.Fatal(err)
	}
}

func latestExecution(t *testing.T, s *Store, id string) map[string]any {
	t.Helper()
	latest, err := openExecutionsLedger(s).Latest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return latest
}

func TestPersistedStaleErrorRecoveryStaleErrorRecurringJobRedispatchesWithoutForceRun(t *testing.T) {
	s, id := staleErrorProbe(t)
	persistStaleError(t, s, id, 110*time.Minute)
	tickOnce(t, s)
	latest := latestExecution(t, s, id)
	if latest == nil {
		t.Fatal("wedged stale-error job must be re-dispatched without force-run")
	}
	if latest["status"] != "completed" {
		t.Fatalf("execution %v", latest)
	}
	if errorRecoveries() < 1 {
		t.Fatal("the recovery was not counted")
	}
}

func TestPersistedStaleErrorRecoveryRecentErrorWithinCadenceNotForceRearmed(t *testing.T) {
	s, id := staleErrorProbe(t)
	persistStaleError(t, s, id, 5*time.Minute)
	tickOnce(t, s)
	if latest := latestExecution(t, s, id); latest != nil {
		t.Fatalf("a within-cadence error is a normal retry and must not be force re-armed, got %v", latest)
	}
}

func TestPersistedStaleErrorRecoveryLiveFireClaimFromOtherProcessNotRearmed(t *testing.T) {
	s, id := staleErrorProbe(t)
	persistStaleError(t, s, id, 110*time.Minute)
	now := time.Now().UTC()
	stamp := func(at time.Time) string { return at.Format("2006-01-02T15:04:05.000000") + "+00:00" }
	if _, err := s.updateJob(context.Background(), id, map[string]any{"fire_claim": map[string]any{"at": stamp(now), "by": "other-host:deadbeef"}}); err != nil {
		t.Fatal(err)
	}
	job := mustGet(t, s, id)
	if s.jobIsStaleErrorRecurring(job, job.Schedule, now) {
		t.Fatal("a job with a live fire claim read as wedged")
	}
	tickOnce(t, s)
	if latest := latestExecution(t, s, id); latest != nil {
		t.Fatalf("a job with a live fire_claim held by another process must not be re-armed, got %v", latest)
	}
	expired := stamp(now.Add(-(FireClaimTTLSeconds + 1) * time.Second))
	if _, err := s.updateJob(context.Background(), id, map[string]any{"fire_claim": map[string]any{"at": expired, "by": "other-host:deadbeef"}}); err != nil {
		t.Fatal(err)
	}
	if !s.jobIsStaleErrorRecurring(mustGet(t, s, id), job.Schedule, now) {
		t.Fatal("an expired fire claim still hides the wedged job")
	}
}

func watchdogJob(t *testing.T, s *Store) *Job {
	t.Helper()
	path := filepath.Join(s.home, "scripts", "watch.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\necho alert\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mustCreate(t, s, CreateJobOptions{Schedule: "every 2m", Script: "watch.sh", NoAgent: true, Deliver: strPtr("local"), Name: strPtr("watchdog"), Repeat: 0})
}

func TestIntentionalRemoveStillDeletes(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchdogJob(t, s)
	if removed, err := s.removeJob(context.Background(), job.ID); err != nil || !removed {
		t.Fatalf("removed %v %v", removed, err)
	}
	if got, err := s.getJob(context.Background(), job.ID); err != nil || got != nil {
		t.Fatalf("job %+v %v", got, err)
	}
}

func TestReplaceFlagAllowsWholesaleRewrite(t *testing.T) {
	s, _ := newTestStore(t)
	watchdogJob(t, s)
	if err := s.SaveJobs(context.Background(), []*Job{}, nil, true); err != nil {
		t.Fatal(err)
	}
	if stored := storedJobs(t, s); len(stored) != 0 {
		t.Fatalf("stored %v", jobIDs(stored))
	}
}
