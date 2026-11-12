package cron

import (
	"context"
	"strings"
	"testing"
)

func TestGivenUpFireLeavesItsAttemptRecoverableNotRunning(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	created, err := openExecutionsLedger(s).Create(context.Background(), job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fire := created["id"].(string)
	journal := s.journalPath(job.ID, fire)
	prev := runJobSeam
	t.Cleanup(func() { runJobSeam = prev })
	runJobSeam = func(_ context.Context, _ *Store, run *Job, _ runJobOptions) (jobRunOutcome, error) {
		writeTestFile(t, journal, `{"params": {"job_id": "`+run.ID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": null}`)
		return jobRunOutcome{}, &CronExecutionUnknown{Msg: "Cron execution unverified: owner lost"}
	}
	run := mustGet(t, s, job.ID)
	run.ExecutionID = strPtr(fire)
	if ok, err := s.runOneJobBody(context.Background(), run, runOneJobOptions{}, nil, nil, nil, nil); err != nil || ok {
		t.Fatalf("body = %v, %v", ok, err)
	}
	if state := mustGet(t, s, job.ID).State; state != "paused" {
		t.Fatalf("state = %s", state)
	}
	givenUp := executionRow(t, s, fire)
	if errText, _ := givenUp["error"].(string); givenUp["status"] != "unknown" || !strings.Contains(errText, "owner lost") {
		t.Fatalf("given up = %v %v", givenUp["status"], givenUp["error"])
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
}
