package cron

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Moves a job's next_run_at seconds into the past
func parkJob(t *testing.T, s *Store, id string, secondsAgo float64) {
	t.Helper()
	at := s.clk.now().Add(-time.Duration(secondsAgo * float64(time.Second)))
	editJobs(t, s, func(j *Job) {
		if j.ID == id {
			j.NextRunAt = strPtr(FormatISO(at))
		}
	})
}

func ledgerRows(t *testing.T, s *Store, id string) []map[string]any {
	t.Helper()
	rows, err := openExecutionsLedger(s).List(context.Background(), &id, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func ledgerStatuses(t *testing.T, s *Store, id string) []string {
	t.Helper()
	out := []string{}
	for _, r := range ledgerRows(t, s, id) {
		out = append(out, r["status"].(string))
	}
	return out
}

func writeHomeConfig(t *testing.T, s *Store, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Home(), "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCatchUpOffSkipsAndBooksOneMissedRowWithTheCount(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("brief"), Schedule: "every 1h", Deliver: prompt("local"), CatchUp: false})
	parkJob(t, s, job.ID, 4*3600+60)
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("due %v", jobIDs(due))
	}
	rows := ledgerRows(t, s, job.ID)
	if len(rows) != 1 || rows[0]["status"] != "missed" {
		t.Fatalf("rows %v", rows)
	}
	if errText, _ := rows[0]["error"].(string); !strings.HasPrefix(errText, "missed ×5 (skipped") {
		t.Fatalf("error %q", errText)
	}
	if rows[0]["started_at"] != nil {
		t.Fatalf("missed row started at %v", rows[0]["started_at"])
	}
}

func TestJobCatchUpBeatsTheGlobalSetting(t *testing.T) {
	s, _ := newTestStore(t)
	writeHomeConfig(t, s, "cron:\n  catch_up_missed: false\n")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("scan"), Schedule: "every 1h", Deliver: prompt("local"), CatchUp: true})
	parkJob(t, s, job.ID, 3*3600+60)
	if got := jobIDs(mustDue(t, s)); strings.Join(got, ",") != job.ID {
		t.Fatalf("due %v", got)
	}
	rows := ledgerRows(t, s, job.ID)
	if len(rows) != 1 || rows[0]["status"] != "missed" || strings.SplitN(rows[0]["error"].(string), " (", 2)[0] != "missed ×3" {
		t.Fatalf("rows %v", rows)
	}
}

func TestGlobalSettingStaysTheDefaultForJobsWithoutTheirOwn(t *testing.T) {
	s, _ := newTestStore(t)
	writeHomeConfig(t, s, "cron:\n  catch_up_missed: false\n")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("brief"), Schedule: "every 1h", Deliver: prompt("local")})
	parkJob(t, s, job.ID, 2*3600)
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("due %v", jobIDs(due))
	}
	if got := ledgerStatuses(t, s, job.ID); strings.Join(got, ",") != "missed" {
		t.Fatalf("statuses %v", got)
	}
}

func TestExplicitGraceSkipsPastItUnlessTheJobOptsIntoCatchUp(t *testing.T) {
	s, _ := newTestStore(t)
	capped := mustCreate(t, s, CreateJobOptions{Prompt: prompt("brief"), Schedule: "* * * * *", Deliver: prompt("local"), GraceMinutes: 5})
	opted := mustCreate(t, s, CreateJobOptions{Prompt: prompt("scan"), Schedule: "* * * * *", Deliver: prompt("local"), GraceMinutes: 5, CatchUp: true})
	slot := s.clk.now().Add(-20 * time.Minute).Truncate(time.Minute)
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(FormatISO(slot)) })
	if got := jobIDs(mustDue(t, s)); strings.Join(got, ",") != opted.ID {
		t.Fatalf("due %v", got)
	}
	if got := ledgerStatuses(t, s, capped.ID); strings.Join(got, ",") != "missed" {
		t.Fatalf("capped statuses %v", got)
	}
}

func TestMissedSlotsCountInTheJobsPinnedZone(t *testing.T) {
	s, _ := newTestStore(t)
	writeHomeConfig(t, s, "timezone: America/Los_Angeles\n")
	ny := mustZone(t, "America/New_York")
	sched, err := s.parseSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	first := FormatISO(time.Date(2026, 10, 22, 9, 0, 0, 0, ny))
	count, err := s.countMissedSlots(sched, first, time.Date(2026, 10, 23, 15, 0, 0, 0, ny), strPtr("America/New_York"))
	if err != nil || count != 2 {
		t.Fatalf("count %d %v", count, err)
	}
}

func TestOneShotPastGraceSkipsByDefaultAndFiresOnceWhenOptedIn(t *testing.T) {
	s, _ := newTestStore(t)
	runAt := FormatISO(s.clk.now().Add(time.Hour))
	skipped := mustCreate(t, s, CreateJobOptions{Prompt: prompt("stale reminder"), Schedule: runAt, Deliver: prompt("local")})
	kept := mustCreate(t, s, CreateJobOptions{Prompt: prompt("late reminder"), Schedule: runAt, Deliver: prompt("local"), CatchUp: true})
	parkJob(t, s, skipped.ID, 3600)
	parkJob(t, s, kept.ID, 3600)
	if got := jobIDs(mustDue(t, s)); strings.Join(got, ",") != kept.ID {
		t.Fatalf("due %v", got)
	}
	if job, err := s.getJob(context.Background(), skipped.ID); err != nil || job != nil {
		t.Fatalf("expired one shot kept %v %v", job, err)
	}
	if got := ledgerStatuses(t, s, skipped.ID); strings.Join(got, ",") != "missed" {
		t.Fatalf("skipped statuses %v", got)
	}
	if got := ledgerRows(t, s, kept.ID); len(got) != 0 {
		t.Fatalf("kept rows %v", got)
	}
}

func TestCatchUpRejectsNonBoolean(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.createJob(context.Background(), CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h", Deliver: prompt("local"), CatchUp: "sometimes"})
	if err == nil || !strings.Contains(err.Error(), "catch_up") {
		t.Fatalf("err %v", err)
	}
}

func TestAPreExistingLedgerIsWidenedWithoutLosingRows(t *testing.T) {
	s, _ := newTestStore(t)
	raw := openRawDB(t, filepath.Join(s.cronDir, "executions.db"))
	if _, err := raw.Exec(`CREATE TABLE executions (
             id TEXT PRIMARY KEY, job_id TEXT NOT NULL, source TEXT NOT NULL,
             process_id TEXT NOT NULL, pid INTEGER NOT NULL, process_started_at INTEGER,
             status TEXT NOT NULL CHECK(status IN
               ('claimed','running','completed','failed','unknown')),
             claimed_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, error TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("INSERT INTO executions (id, job_id, source, process_id, pid, status, claimed_at) VALUES ('old', 'j1', 'builtin', 'p', 1, 'completed', '2026-01-01T00:00:00+00:00')"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if err := recordMissedOccurrences(s, "j1", 2, nil, "test"); err != nil {
		t.Fatal(err)
	}
	got := ledgerStatuses(t, s, "j1")
	sort.Strings(got)
	if strings.Join(got, ",") != "completed,missed" {
		t.Fatalf("statuses %v", got)
	}
}

func TestRemovingAJobStopsItsLiveRun(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("long"), Schedule: "every 1h", Deliver: prompt("local")})
	ctx := context.Background()
	l := openExecutionsLedger(s)
	execID := createExecution(t, l, job.ID, "test", nil)
	if _, err := l.MarkRunning(ctx, execID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.removeJob(ctx, job.ID); err != nil || !ok {
		t.Fatalf("remove %v %v", ok, err)
	}
	request := s.newCancelRequest(execID)
	if !request.IsSet() || request.Reason() != "job removed while running" {
		t.Fatalf("cancel set=%v reason=%q", request.IsSet(), request.Reason())
	}
}
