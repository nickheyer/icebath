package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Peer whose every RPC answers through call, the way the Python case patches connect_gateway
func receiptPeer(t *testing.T, call func(method string, params map[string]any) (map[string]any, error)) {
	t.Helper()
	replaceCanonicalDoors(t, func(context.Context) (cronRPCClient, error) { return rpcClientFunc(call), nil }, nil)
}

// Creates a job and its execution row, marked running when asked
func receiptFire(t *testing.T, s *Store, deliver string, running bool) (*Job, string) {
	t.Helper()
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt(deliver)})
	ledger := openExecutionsLedger(s)
	execution, err := ledger.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fire := execution["id"].(string)
	if running {
		if _, err := ledger.MarkRunning(ctx, fire); err != nil {
			t.Fatal(err)
		}
	}
	return job, fire
}

func mustReconcile(t *testing.T, s *Store) {
	t.Helper()
	if err := s.reconcilePendingJournals(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func executionRow(t *testing.T, s *Store, fire string) map[string]any {
	t.Helper()
	row, err := openExecutionsLedger(s).Get(context.Background(), fire)
	if err != nil || row == nil {
		t.Fatalf("execution %s = %v, %v", fire, row, err)
	}
	return row
}

func journalExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRecoveredReceiptFinishesForeignExecutionAndIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	job, fire := receiptFire(t, s, "local", true)
	journal := foreignJournal(t, s, job.ID, fire)
	saved, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	receiptPeer(t, func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"status": "terminal", "job": job, "result": []any{true, "document", "answer", nil}}, nil
	})
	mustReconcile(t, s)
	if row := executionRow(t, s, fire); row["status"] != "completed" {
		t.Fatalf("status = %v", row["status"])
	}
	if journalExists(journal) {
		t.Fatal("journal kept")
	}
	completed := mustGet(t, s, job.ID).Repeat.Completed
	writeTestFile(t, journal, string(saved))
	mustReconcile(t, s)
	if got := mustGet(t, s, job.ID).Repeat.Completed; got != completed {
		t.Fatalf("completed = %d, want %d", got, completed)
	}
	if row := executionRow(t, s, fire); row["delivery_outcome"] != "suppressed" {
		t.Fatalf("delivery_outcome = %v", row["delivery_outcome"])
	}
}

func TestDefiniteRefusalIsBookedAndJournalRetired(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job, fire := receiptFire(t, s, "local", false)
	receiptPeer(t, func(method string, _ map[string]any) (map[string]any, error) {
		if method == "cron.submit" {
			return nil, &gateway.GatewayRPCError{Msg: "invalid_workdir"}
		}
		if method != "cron.recover" {
			t.Fatalf("method = %s", method)
		}
		return map[string]any{"status": "missing", "result": nil}, nil
	})
	out, err := s.runCanonicalJob(ctx, job, nil, nil, strPtr(fire))
	if err != nil || out.Success || out.Error == nil || !strings.Contains(*out.Error, "invalid_workdir") {
		t.Fatalf("out %+v err %v", out, err)
	}
	departOwner(t, s, fire)
	mustReconcile(t, s)
	if row := executionRow(t, s, fire); row["status"] != "failed" {
		t.Fatalf("status = %v", row["status"])
	}
	if mustGet(t, s, job.ID).State == "paused" {
		t.Fatal("job paused")
	}
	if journalExists(s.journalPath(job.ID, fire)) {
		t.Fatal("journal kept")
	}
}

func TestInProcessFirerKeepsItsRowJournalAndOutput(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job, fire := receiptFire(t, s, "local", true)
	journal := s.journalPath(job.ID, fire)
	writeTestFile(t, journal, `{"params": {"job_id": "`+job.ID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": {"x": 1}}`)
	var calls []string
	receiptPeer(t, func(method string, _ map[string]any) (map[string]any, error) {
		calls = append(calls, method)
		return map[string]any{"status": "terminal", "job": job, "result": []any{true, "document", "answer", nil}}, nil
	})
	mustReconcile(t, s)
	if row := executionRow(t, s, fire); row["status"] != "running" {
		t.Fatalf("status = %v", row["status"])
	}
	if !journalExists(journal) || len(calls) != 0 {
		t.Fatalf("journal kept %v calls %v", journalExists(journal), calls)
	}
	dir, err := s.jobOutputDir(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outputs, _ := filepath.Glob(filepath.Join(dir, "*.md")); len(outputs) != 0 {
		t.Fatalf("outputs = %v", outputs)
	}
	if last := mustGet(t, s, job.ID).LastRunAt; last != nil {
		t.Fatalf("last_run_at = %v", *last)
	}
	if row, err := openExecutionsLedger(s).Finish(ctx, fire, false, strPtr("interrupted"), nil, false); err != nil || row == nil {
		t.Fatalf("finish = %v, %v", row, err)
	}
}

func TestResumeSurvivesRecoveryOfOldUnknownJournal(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	journal := s.journalPath(job.ID, "lost-fire")
	writeTestFile(t, journal, `{"params": {"job_id": "`+job.ID+`", "request_id": "lost-fire", "extra_prompt": null}, "receipt": null}`)
	receiptPeer(t, func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"status": "unknown", "result": nil}, nil
	})
	mustReconcile(t, s)
	if state := mustGet(t, s, job.ID).State; state != "paused" {
		t.Fatalf("state = %s", state)
	}
	if _, err := s.resumeJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, s)
	saved := mustGet(t, s, job.ID)
	if !saved.Enabled || saved.State != "scheduled" {
		t.Fatalf("enabled %v state %s", saved.Enabled, saved.State)
	}
	if !journalExists(journal) {
		t.Fatal("journal retired")
	}
}

func TestRecoveredReceiptBooksTheSameFailureVerdictsAsTheFirer(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{{"blank", "  \n"}, {"declared", "[CRON_FAILURE]\nchild task failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			deliveryDBOverride = filepath.Join(t.TempDir(), "deliveries.db")
			t.Cleanup(func() { deliveryDBOverride = "" })
			job, fire := receiptFire(t, s, "telegram:1", true)
			foreignJournal(t, s, job.ID, fire)
			receiptPeer(t, func(string, map[string]any) (map[string]any, error) {
				return map[string]any{"status": "terminal", "job": job, "result": []any{true, "document", tc.answer, nil}}, nil
			})
			mustReconcile(t, s)
			saved := mustGet(t, s, job.ID)
			if deref(saved.LastStatus) != "error" || saved.FailureStreak != 1 {
				t.Fatalf("last_status %q failure_streak %d", deref(saved.LastStatus), saved.FailureStreak)
			}
			if row := executionRow(t, s, fire); row["status"] != "failed" {
				t.Fatalf("status = %v", row["status"])
			}
		})
	}
}
