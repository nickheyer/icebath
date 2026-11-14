package cron

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func createExecution(t *testing.T, l *Ledger, jobID, source string, slot *string) string {
	t.Helper()
	row, err := l.Create(context.Background(), jobID, source, slot)
	if err != nil {
		t.Fatal(err)
	}
	return row["id"].(string)
}

func finishExecution(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, err := l.Finish(context.Background(), id, true, nil, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerMigrationAndCompletionIdentity(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	s.executionsFile = filepath.Join(t.TempDir(), "executions.db")
	raw := openRawDB(t, s.executionsFile)
	if _, err := raw.Exec(`CREATE TABLE executions (
            id TEXT PRIMARY KEY, job_id TEXT NOT NULL, source TEXT NOT NULL,
            process_id TEXT NOT NULL, pid INTEGER NOT NULL, process_started_at INTEGER,
            status TEXT NOT NULL, claimed_at TEXT NOT NULL,
            started_at TEXT, finished_at TEXT, error TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("INSERT INTO executions VALUES ('legacy','job','builtin','old',-1,NULL,'completed','2026-01-01T00:00:00Z',NULL,NULL,NULL)"); err != nil {
		t.Fatal(err)
	}
	slot := "2026-01-01T00:00:00+00:00"
	job := &Job{ID: "job"}
	if completedOccurrence(s, job, slot) {
		t.Fatal("legacy row without an instant proved the slot")
	}
	var id string
	var instant sql.NullString
	if err := raw.QueryRow("SELECT id, scheduled_instant FROM executions").Scan(&id, &instant); err != nil || id != "legacy" || instant.Valid {
		t.Fatalf("migrated legacy row %s %v %v", id, instant, err)
	}
	l := openExecutionsLedger(s)
	for _, status := range []string{"failed", "unknown", "running", "claimed"} {
		rowID := createExecution(t, l, "job", "control", &slot)
		if _, err := raw.Exec("UPDATE executions SET status=? WHERE id=?", status, rowID); err != nil {
			t.Fatal(err)
		}
		if completedOccurrence(s, job, slot) {
			t.Fatalf("a %s row proved the slot", status)
		}
	}
	finishExecution(t, l, createExecution(t, l, "job", "builtin", &slot))
	createExecution(t, l, "job", "manual", nil)
	if !completedOccurrence(s, job, "2026-01-01T08:00:00+0800") {
		t.Fatal("the same instant in another offset was not recognised")
	}
	if completedOccurrence(s, job, "2026-01-01T00:00:01Z") {
		t.Fatal("a different instant was proven")
	}
	if completedOccurrence(s, &Job{ID: "other"}, slot) {
		t.Fatal("another job's slot was proven")
	}
}

func TestNaiveNextRunCarriesNoOccurrenceIdentity(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	naive := mustCreate(t, s, CreateJobOptions{Prompt: prompt("legacy"), Schedule: "every 4h"})
	editJobs(t, s, func(j *Job) {
		j.NextRunAt = strPtr(time.Now().Add(-10 * time.Minute).Local().Format("2006-01-02T15:04:05.000000"))
	})
	for _, j := range mustDue(t, s) {
		if j.ID == naive.ID {
			if j.ScheduledInstant != nil {
				t.Fatalf("naive instant stamped %s", *j.ScheduledInstant)
			}
			return
		}
	}
	t.Fatal("naive job not due")
}

func TestCompletionBeforeOccurrenceDoesNotProveSlotCompleted(t *testing.T) {
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newClockStore(t, &early, time.UTC)
	s.executionsFile = filepath.Join(t.TempDir(), "executions.db")
	l := openExecutionsLedger(s)
	slot := "2026-01-05T00:00:00+00:00"
	finishExecution(t, l, createExecution(t, l, "job", "control", &slot))
	if completedOccurrence(s, &Job{ID: "job"}, slot) {
		t.Fatal("a completion before the occurrence proved it")
	}
	setNow(s, time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	finishExecution(t, l, createExecution(t, l, "job", "builtin", &slot))
	if !completedOccurrence(s, &Job{ID: "job"}, slot) {
		t.Fatal("the legitimate completion did not prove the slot")
	}
}

func TestCompletedOccurrenceSkipNamesJobSlotAndRow(t *testing.T) {
	s, buf := newTestStore(t)
	s.clk.zoneFn = func() *time.Location { return time.UTC }
	s.executionsFile = filepath.Join(t.TempDir(), "executions.db")
	slot := FormatISO(time.Now().UTC().Add(-2 * time.Minute))
	stored := mustCreate(t, s, CreateJobOptions{Prompt: prompt("test"), Schedule: "every 4h"})
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(slot) })
	l := openExecutionsLedger(s)
	completed := createExecution(t, l, stored.ID, "builtin", &slot)
	finishExecution(t, l, completed)
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("completed slot dispatched %v", jobIDs(due))
	}
	if deref(mustGet(t, s, stored.ID).NextRunAt) == slot {
		t.Fatal("consumed slot not advanced")
	}
	var skips []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, completed) {
			skips = append(skips, line)
		}
	}
	if len(skips) != 1 {
		t.Fatalf("skip lines %q", skips)
	}
	if !strings.Contains(skips[0], stored.ID) || !strings.Contains(skips[0], deref(scheduledInstant(&slot))) || !strings.Contains(skips[0], "level=WARN") {
		t.Fatalf("skip line %s", skips[0])
	}
}
