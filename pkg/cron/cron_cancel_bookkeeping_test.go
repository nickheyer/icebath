package cron

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestACappedCountSaysSoAfterACatchUp(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("tick"), Schedule: "every 1m", Deliver: prompt("local"), CatchUp: true})
	editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(FormatISO(s.clk.now().Add(-1100 * time.Minute))) })
	mustDue(t, s)
	var missed []map[string]any
	for _, row := range ledgerRows(t, s, job.ID) {
		if row["status"] == "missed" {
			missed = append(missed, row)
		}
	}
	if len(missed) != 1 {
		t.Fatalf("missed rows %v", missed)
	}
	if text, _ := missed[0]["error"].(string); !strings.Contains(text, "counting stopped at 1000") {
		t.Fatalf("missed row error %q", text)
	}
}

func TestTheJobListStillShowsALiveRunBehindARefusedTrigger(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	l := openExecutionsLedger(s)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("long"), Schedule: "every 1h", Deliver: prompt("local")})
	created, err := l.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	live := created["id"].(string)
	if _, err := l.MarkRunning(ctx, live); err != nil {
		t.Fatal(err)
	}
	created, err = l.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	refused := created["id"].(string)
	if _, err := l.Finish(ctx, refused, false, prompt("Fire claim was not acquired"), nil, false); err != nil {
		t.Fatal(err)
	}
	if err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE executions SET claimed_at='2999-01-01T00:00:00+00:00' WHERE id=?", refused)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := s.listJobs(ctx, true)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %v %v", jobIDs(listed), err)
	}
	if listed[0].LatestExecution["id"] != refused || listed[0].LiveExecution["id"] != live {
		t.Fatalf("latest %v live %v", listed[0].LatestExecution, listed[0].LiveExecution)
	}
	if _, err := l.Finish(ctx, live, true, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if listed, _ := s.listJobs(ctx, true); listed[0].LiveExecution != nil {
		t.Fatalf("finished run still live %v", listed[0].LiveExecution)
	}
}

// Fire marked running, cancelled by the operator and booked through the cancelled-run tail
func cancelledRun(t *testing.T, s *Store, job *Job) string {
	t.Helper()
	ctx := context.Background()
	ledger := openExecutionsLedger(s)
	created, err := ledger.Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fire := created["id"].(string)
	if _, err := ledger.MarkRunning(ctx, fire); err != nil {
		t.Fatal(err)
	}
	mustRequestCancel(t, s, fire, strPtr("operator asked"))
	if err := s.finishCancelledRun(ctx, mustGet(t, s, job.ID), fire, nil); err != nil {
		t.Fatal(err)
	}
	return fire
}

func TestReceiptRecoveryDoesNotReplayACancelledAgentRun(t *testing.T) {
	s, _ := newTestStore(t)
	var calls []string
	var job *Job
	receiptPeer(t, func(method string, _ map[string]any) (map[string]any, error) {
		calls = append(calls, method)
		return map[string]any{"status": "terminal", "job": job, "result": []any{false, "doc", "", "stopped"}}, nil
	})
	job = mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	fire := cancelledRun(t, s, job)
	journal := s.journalPath(job.ID, fire)
	writeTestFile(t, journal, `{"params": {"job_id": "`+job.ID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": {"session_id": "s", "admission_id": "a"}}`)
	booked := mustGet(t, s, job.ID)
	mustReconcile(t, s)
	after := mustGet(t, s, job.ID)
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	if deref(after.LastStatus) != deref(booked.LastStatus) || *after.Repeat != *booked.Repeat || after.FailureStreak != booked.FailureStreak {
		t.Fatalf("after %q %+v %d booked %q %+v %d", deref(after.LastStatus), *after.Repeat, after.FailureStreak,
			deref(booked.LastStatus), *booked.Repeat, booked.FailureStreak)
	}
	if row := executionRow(t, s, fire); row["status"] != "cancelled" {
		t.Fatalf("status = %v", row["status"])
	}
	if journalExists(journal) {
		t.Fatal("journal kept")
	}
}
