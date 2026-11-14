package cron

import (
	"context"
	"errors"
	"testing"
)

func TestSubmitErrorAfterCommitObservesTheAdmittedRun(t *testing.T) {
	s, _ := newTestStore(t)
	pinCronTestModel(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	authority, db, ctx := recoveryOwner(t, s)
	created, err := openExecutionsLedger(s).Create(ctx, job.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fire := created["id"].(string)
	real := cronOwnerOperation
	t.Cleanup(func() { cronOwnerOperation = real })
	cronOwnerOperation = func(opCtx context.Context, owner any, name string, params map[string]any) (map[string]any, error) {
		answer, err := real(opCtx, owner, name, params)
		if name != "submit" || err != nil {
			return answer, err
		}
		ownerSettleAnswer(t, opCtx, db, authority, answer["session_id"].(string))
		return nil, errors.New("owner answer lost after commit")
	}
	release := bindRecoveryOwner(ctx, authority)
	run := mustGet(t, s, job.ID)
	run.ExecutionID = strPtr(fire)
	_, _ = s.runOneJobBody(ctx, run, runOneJobOptions{}, nil, nil, nil, nil)
	release()
	if mustGet(t, s, job.ID).State == "paused" {
		t.Fatal("job paused")
	}
	if row := executionRow(t, s, fire); row["status"] != "completed" {
		t.Fatalf("status = %v", row["status"])
	}
}
