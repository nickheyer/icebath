package cron

import (
	"context"
	"testing"
	"time"
)

const (
	legacyUTCNextRun = "2026-09-02T04:00:00+00:00"
	daily0400        = "0 4 * * *"
)

// Zeroes the module level migration counters for one test, the autouse counter reset fixture
func resetMigrationCounters(t *testing.T) {
	t.Helper()
	reset := func() {
		telemetryMu.Lock()
		timezoneMigrationCatchups, timezoneMigrationCatchupsRecent = 0, nil
		telemetryMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// Store whose patched now is the given aware instant in its own fixed offset
func brusselsStore(t *testing.T, now string) *Store {
	t.Helper()
	resetMigrationCounters(t)
	at, err := ParseISO(now)
	if err != nil {
		t.Fatal(err)
	}
	return newClockStore(t, &at.Time, at.Time.Location())
}

// Persists a cron job with a pinned next_run_at, the legacy row shape
func writeCronJob(t *testing.T, s *Store, expr, nextRunAt string) string {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("t")})
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.Schedule = &Schedule{Kind: "cron", Expr: expr}
			j.NextRunAt = strPtr(nextRunAt)
		}
	})
	return job.ID
}

func migrationCatchups() int {
	return TimezoneMigrationCatchupStats()["timezone_migration_catchups"].(int)
}

func TestLegacyUtcOffsetNextRunStillFires(t *testing.T) {
	s := brusselsStore(t, "2026-09-02T06:05:00+02:00")
	jid := writeCronJob(t, s, daily0400, legacyUTCNextRun)
	if !containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("legacy offset occurrence was not due")
	}
	stats := TimezoneMigrationCatchupStats()
	if stats["timezone_migration_catchups"] != 1 {
		t.Fatalf("stats %v", stats)
	}
	record := stats["recent"].([]map[string]any)[0]
	if record["job_id"] != jid || record["expr"] != daily0400 || record["stored_next_run_at"] != legacyUTCNextRun ||
		record["normalized_next_run_at"] != "2026-09-02T06:00:00+02:00" {
		t.Fatalf("record %v", record)
	}
}

func TestLegacyOffsetCatchupFiresAtMostOnce(t *testing.T) {
	s := brusselsStore(t, "2026-09-02T06:05:00+02:00")
	jid := writeCronJob(t, s, daily0400, legacyUTCNextRun)
	if !containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("legacy offset occurrence was not due")
	}
	if ok, err := s.advanceNextRun(context.Background(), jid); !ok || err != nil {
		t.Fatalf("advance %v %v", ok, err)
	}
	if _, won, err := s.claimJobForFire(context.Background(), jid, FireClaimOptions{}); !won || err != nil {
		t.Fatalf("claim %v %v", won, err)
	}
	if got := deref(mustGet(t, s, jid).NextRunAt); got != "2026-09-03T04:00:00+02:00" {
		t.Fatalf("next_run_at %s", got)
	}
	if containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("legacy catch up fired twice")
	}
}

func TestGenuineExprEditStillReanchorsWithoutFiring(t *testing.T) {
	s := brusselsStore(t, "2026-09-02T06:05:00+02:00")
	jid := writeCronJob(t, s, "0 9 * * *", "2026-09-02T04:00:00+02:00")
	if containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("edited expression fired")
	}
	if got := deref(mustGet(t, s, jid).NextRunAt); got != "2026-09-02T09:00:00+02:00" {
		t.Fatalf("next_run_at %s", got)
	}
	if n := migrationCatchups(); n != 0 {
		t.Fatalf("catchups %d", n)
	}
}

func TestExprEditOnALegacyOffsetRowStillDoesNotFire(t *testing.T) {
	s := brusselsStore(t, "2026-09-02T06:05:00+02:00")
	jid := writeCronJob(t, s, "0 9 * * *", legacyUTCNextRun)
	if containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("edited legacy row fired")
	}
	if got := deref(mustGet(t, s, jid).NextRunAt); got != "2026-09-02T09:00:00+02:00" {
		t.Fatalf("next_run_at %s", got)
	}
	if n := migrationCatchups(); n != 0 {
		t.Fatalf("catchups %d", n)
	}
}

func TestFutureLocalWallClockIsLeftScheduled(t *testing.T) {
	s := brusselsStore(t, "2026-09-02T05:00:00+02:00")
	jid := writeCronJob(t, s, daily0400, legacyUTCNextRun)
	if containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("future occurrence fired")
	}
	if got := deref(mustGet(t, s, jid).NextRunAt); got != legacyUTCNextRun {
		t.Fatalf("next_run_at %s", got)
	}
	if n := migrationCatchups(); n != 0 {
		t.Fatalf("catchups %d", n)
	}
}

func TestFutureStoredWallClockStillTakesTheOffsetRepairPath(t *testing.T) {
	s := brusselsStore(t, "2026-09-02T14:00:00+02:00")
	jid := writeCronJob(t, s, "0 21 * * *", "2026-09-02T21:00:00+10:00")
	if containsString(jobIDs(mustDue(t, s)), jid) {
		t.Fatal("westward move fired early")
	}
	if got := deref(mustGet(t, s, jid).NextRunAt); got != "2026-09-02T21:00:00+02:00" {
		t.Fatalf("next_run_at %s", got)
	}
	if n := migrationCatchups(); n != 0 {
		t.Fatalf("catchups %d", n)
	}
}

func TestClassifierSeparatesMigrationFromEdit(t *testing.T) {
	parse := func(text string) time.Time {
		at, err := ParseISO(text)
		if err != nil {
			t.Fatal(err)
		}
		return at.Time
	}
	daily := &Schedule{Kind: "cron", Expr: daily0400}
	nine := &Schedule{Kind: "cron", Expr: "0 9 * * *"}
	rawLegacy, normalized, onLattice := parse(legacyUTCNextRun), parse("2026-09-02T06:00:00+02:00"), parse("2026-09-02T04:00:00+02:00")
	for _, tc := range []struct {
		sched     *Schedule
		raw, next time.Time
		want      string
	}{
		{daily, onLattice, onLattice, StaleCronMatch},
		{daily, rawLegacy, normalized, StaleCronTimezoneMigration},
		{nine, onLattice, onLattice, StaleCronExprEdit},
		{nine, rawLegacy, normalized, StaleCronExprEdit},
	} {
		if got := classifyStaleCronNextRun(tc.sched, tc.raw, tc.next); got != tc.want {
			t.Fatalf("%s %s %s = %s want %s", tc.sched.Expr, FormatISO(tc.raw), FormatISO(tc.next), got, tc.want)
		}
	}
}
