package cron

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Owner of the test home admitting cron fires without running them, config.yaml naming test-model
func cronOwnerFixture(t *testing.T, s *Store) (*gateway.SessionAuthority, *state.SessionDB, context.Context) {
	t.Helper()
	authority, db, ctx := recoveryOwner(t, s)
	writeTestFile(t, filepath.Join(s.Home(), "config.yaml"), "model: test-model\n")
	return authority, db, ctx
}

// Refusal of a runtime operation naming its reason, failing the case otherwise
func wantRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("err = %v, want %s", err, reason)
	}
}

func cronRuns(t *testing.T, ctx context.Context, db *state.SessionDB, jobID string) []string {
	t.Helper()
	runs, err := db.ListCronJobRuns(ctx, jobID, 20)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		id, _ := r.Get("id")
		job, _ := r.Get("cron_job_id")
		execution, _ := r.Get("cron_execution_id")
		out = append(out, fmt.Sprintf("%v|%v|%v", id, job, execution))
	}
	return out
}

func TestAFreshExecutionIDWithAColonIsRefusedBeforeItCanMisfileARun(t *testing.T) {
	s, _ := newTestStore(t)
	authority, db, ctx := cronOwnerFixture(t, s)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")}).ID
	params := map[string]any{"job_id": job, "request_id": "fire:1", "extra_prompt": nil}
	_, err := gateway.CronOwnerOperation(ctx, authority, "submit", params)
	wantRefusal(t, err, "invalid_params")
	recovered, err := gateway.CronOwnerOperation(ctx, authority, "recover", params)
	if err != nil || len(recovered) != 2 || recovered["status"] != "missing" || recovered["result"] != nil {
		t.Fatalf("recover = %v, %v", recovered, err)
	}
	if runs := cronRuns(t, ctx, db, job); len(runs) != 0 {
		t.Fatalf("runs = %v", runs)
	}
	if runs := cronRuns(t, ctx, db, job+":fire"); len(runs) != 0 {
		t.Fatalf("misfiled runs = %v", runs)
	}
}

func TestAnExecutionIDAdmittedWithAColonBeforeTheUpgradeKeepsItsAdmission(t *testing.T) {
	s, _ := newTestStore(t)
	authority, db, ctx := cronOwnerFixture(t, s)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")}).ID
	params := map[string]any{"job_id": job, "request_id": "fire:1", "extra_prompt": "extra"}
	guard := gateway.SwapCronExecutionIDGuard(func(string) bool { return false })
	receipt, err := gateway.CronOwnerOperation(ctx, authority, "submit", params)
	gateway.SwapCronExecutionIDGuard(guard)
	if err != nil {
		t.Fatal(err)
	}
	admissions := func() []string {
		var rows []string
		if err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			found, err := conn.QueryContext(ctx, "SELECT admission_id, request_id FROM session_admissions")
			if err != nil {
				return err
			}
			defer found.Close()
			for found.Next() {
				var admission, request string
				if err := found.Scan(&admission, &request); err != nil {
					return err
				}
				rows = append(rows, admission+"|"+request)
			}
			return found.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := admissions()
	if again, err := gateway.CronOwnerOperation(ctx, authority, "submit", params); err != nil || !maps.Equal(again, receipt) {
		t.Fatalf("retry = %v, %v want %v", again, err, receipt)
	}
	recovered, err := gateway.CronOwnerOperation(ctx, authority, "recover", params)
	if err != nil || recovered["session_id"] != receipt["session_id"] || recovered["admission_id"] != receipt["admission_id"] {
		t.Fatalf("recover = %v, %v", recovered, err)
	}
	changed := maps.Clone(params)
	changed["extra_prompt"] = "changed"
	for _, op := range []string{"submit", "recover"} {
		_, err := gateway.CronOwnerOperation(ctx, authority, op, changed)
		wantRefusal(t, err, "admission_conflict")
	}
	want := fmt.Sprintf("%v|cron:%s:fire:1", receipt["admission_id"], job)
	if after := admissions(); len(after) != 1 || after[0] != want || len(before) != 1 || before[0] != want {
		t.Fatalf("admissions before %v after %v want %s", before, after, want)
	}
	sid := receipt["session_id"].(string)
	if runs := cronRuns(t, ctx, db, job); len(runs) != 1 || runs[0] != sid+"|"+job+"|fire:1" {
		t.Fatalf("runs = %v", runs)
	}
	if runs := cronRuns(t, ctx, db, job+":fire"); len(runs) != 0 {
		t.Fatalf("misfiled runs = %v", runs)
	}
	identity, err := db.CronRunIdentity(ctx, sid)
	if err != nil || !pyjson.Equal(identity, pyjson.ObjectOf("cron_job_id", job, "cron_execution_id", "fire:1")) {
		t.Fatalf("identity = %v, %v", identity, err)
	}
}
