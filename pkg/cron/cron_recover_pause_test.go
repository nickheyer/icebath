package cron

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Hands the fire's row to a departed owner process
func departOwner(t *testing.T, s *Store, fire string) {
	t.Helper()
	ctx := context.Background()
	if err := openExecutionsLedger(s).transaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE executions SET process_id=?,pid=? WHERE id=?", "old-owner", 999999, fire)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Hands the fire's row to a departed owner and writes its admission journal
func foreignJournal(t *testing.T, s *Store, jobID, fire string) string {
	t.Helper()
	departOwner(t, s, fire)
	journal := s.journalPath(jobID, fire)
	writeTestFile(t, journal, `{"params": {"job_id": "`+jobID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": null}`)
	return journal
}

func TestMissingAdmissionIsBookedFailedNotPaused(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	replaceCanonicalDoors(t, func(context.Context) (cronRPCClient, error) {
		return rpcClientFunc(func(string, map[string]any) (map[string]any, error) {
			return map[string]any{"status": "missing", "result": nil}, nil
		}), nil
	}, nil)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	ledger := openExecutionsLedger(s)
	execution, err := ledger.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fire := execution["id"].(string)
	if _, err := ledger.MarkRunning(ctx, fire); err != nil {
		t.Fatal(err)
	}
	journal := foreignJournal(t, s, job.ID, fire)
	if err := s.reconcilePendingJournals(ctx, true); err != nil {
		t.Fatal(err)
	}
	saved := mustGet(t, s, job.ID)
	if saved.State == "paused" || !saved.Enabled || deref(saved.LastStatus) != "error" {
		t.Fatalf("saved = state %s enabled %v last_status %v", saved.State, saved.Enabled, deref(saved.LastStatus))
	}
	if row, err := ledger.Get(ctx, fire); err != nil || row["status"] != "failed" {
		t.Fatalf("execution = %v, %v", row, err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("journal kept: %v", err)
	}
}

// Journals the fire then loses its observer through the run, so the body pauses the job
func unknownFire(t *testing.T, s *Store, jobID, fire string) string {
	t.Helper()
	journal := s.journalPath(jobID, fire)
	prev := runJobSeam
	t.Cleanup(func() { runJobSeam = prev })
	runJobSeam = func(_ context.Context, _ *Store, job *Job, _ runJobOptions) (jobRunOutcome, error) {
		writeTestFile(t, journal, `{"params": {"job_id": "`+job.ID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": null}`)
		return jobRunOutcome{}, &CronExecutionUnknown{Msg: "Cron execution unverified: observer dropped"}
	}
	run := mustGet(t, s, jobID)
	run.ExecutionID = strPtr(fire)
	if ok, err := s.runOneJobBody(context.Background(), run, runOneJobOptions{}, nil, nil, nil, nil); err != nil || ok {
		t.Fatalf("body = %v, %v", ok, err)
	}
	if state := mustGet(t, s, jobID).State; state != "paused" {
		t.Fatalf("state = %s", state)
	}
	return journal
}

func TestTerminalReceiptLiftsTheUnknownPauseItCaused(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	ledger := openExecutionsLedger(s)
	created, err := ledger.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fire := created["id"].(string)
	journal := unknownFire(t, s, job.ID, fire)
	receiptPeer(t, func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"status": "terminal", "job": job, "result": []any{true, "document", "answer", nil}}, nil
	})
	mustReconcile(t, s)
	saved := mustGet(t, s, job.ID)
	if saved.State != "scheduled" || !saved.Enabled || saved.PausedReason != nil {
		t.Fatalf("saved = %s %v %v", saved.State, saved.Enabled, saved.PausedReason)
	}
	if row := executionRow(t, s, fire); row["status"] != "completed" {
		t.Fatalf("status = %v", row["status"])
	}
	if journalExists(journal) {
		t.Fatal("journal kept")
	}
	second, err := ledger.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	unknownFire(t, s, job.ID, second["id"].(string))
	if _, err := s.resumeJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pauseJob(ctx, job.ID, prompt("operator hold")); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, s)
	if reason := deref(mustGet(t, s, job.ID).PausedReason); reason != "operator hold" {
		t.Fatalf("paused_reason = %q", reason)
	}
}
