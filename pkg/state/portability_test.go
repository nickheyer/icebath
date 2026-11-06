package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func cronRunIDs(t *testing.T, db *SessionDB, jobID string, limit, offset int) []string {
	t.Helper()
	rows, err := db.ListCronJobRunsFrom(context.Background(), jobID, limit, offset)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if rowValue(r, "cron_job_id") != jobID {
			t.Fatalf("row stamped %v", rowValue(r, "cron_job_id"))
		}
		out = append(out, pyStr(rowValue(r, "id"))+"|"+pyStr(rowValue(r, "cron_execution_id")))
	}
	return out
}

func cronAdmit(t *testing.T, db *SessionDB, sessionID, requestID string, policyJob string) {
	t.Helper()
	readSession(t, db, NewSession{ID: sessionID, Source: "cron"})
	profileExec(t, db, "INSERT INTO session_admissions (admission_id, request_id, principal_id, target_session_id, lineage_json, "+
		"payload_json, payload_digest, intent, status) VALUES (?, ?, ?, ?, '{}', '{}', 'd', 'queue', 'terminal')",
		"adm-"+sessionID, requestID, CronPrincipalID, sessionID)
	if policyJob != "" {
		profileExec(t, db, "INSERT INTO state_meta (key, value) VALUES (?, ?)", LocalPolicyPrefix+sessionID,
			`{"policy": {"request_json": "{\"cron_job\": {\"id\": \"`+policyJob+`\"}}"}}`)
	}
}

func TestUnifiedRunsAreListedWithLegacyRunsAndOnlyForTheirJob(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "cron_alpha_20260101_000000", Source: "cron"})
	cronAdmit(t, db, "local-a1", "cron:alpha:exec1", "")
	cronAdmit(t, db, "local-b1", "cron:beta:exec2", "")
	cronAdmit(t, db, "local-ab", "cron:alpha:b:exec3", "alpha:b")
	cronAdmit(t, db, "local-legacy-colon", "cron:alpha:ex:4", "alpha")
	profileExec(t, db, "UPDATE sessions SET started_at = CASE id WHEN 'cron_alpha_20260101_000000' THEN 1 WHEN 'local-a1' THEN 2 "+
		"WHEN 'local-legacy-colon' THEN 3 ELSE 4 END")
	got := strings.Join(cronRunIDs(t, db, "alpha", 20, 0), ",")
	if got != "local-legacy-colon|ex:4,local-a1|exec1,cron_alpha_20260101_000000|None" {
		t.Fatalf("alpha runs %s", got)
	}
	if got := strings.Join(cronRunIDs(t, db, "beta", 20, 0), ","); got != "local-b1|exec2" {
		t.Fatalf("beta runs %s", got)
	}
	if got := strings.Join(cronRunIDs(t, db, "alpha:b", 20, 0), ","); got != "local-ab|exec3" {
		t.Fatalf("alpha:b runs %s", got)
	}
	if got := strings.Join(cronRunIDs(t, db, "alpha", 1, 2), ","); got != "cron_alpha_20260101_000000|None" {
		t.Fatalf("paged runs %s", got)
	}
	runs, err := db.ListCronJobRuns(context.Background(), "alpha", 1)
	if err != nil || len(runs) != 1 || rowValue(runs[0], "preview") != "" {
		t.Fatalf("limited runs %v %v", runs, err)
	}
}

func TestLegacyRunsOfAJobWhoseIDExtendsAnotherStayWithTheirOwnJob(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "cron_daily_20261011_120000", Source: "cron"})
	readSession(t, db, NewSession{ID: "cron_daily_digest_20261011_130000", Source: "cron"})
	if got := strings.Join(cronRunIDs(t, db, "daily", 20, 0), ","); got != "cron_daily_20261011_120000|None" {
		t.Fatalf("daily %s", got)
	}
	if got := strings.Join(cronRunIDs(t, db, "daily_digest", 20, 0), ","); got != "cron_daily_digest_20261011_130000|None" {
		t.Fatalf("digest %s", got)
	}
}

func TestRunLookupIsIndexServedOnNewAndUpgradedStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := usageOpen(t, path)
	profileExec(t, first, "DROP INDEX IF EXISTS idx_session_admissions_cron_request")
	first.Close()
	usageOpen(t, path).Close()
	db := usageOpen(t, path)
	if rows, err := db.ListCronJobRuns(context.Background(), "alpha", 20); err != nil || len(rows) != 0 {
		t.Fatalf("runs %v %v", rows, err)
	}
	query, args, err := cronJobRunsQuery("alpha", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := db.readRows(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, r := range plan {
		details = append(details, pyStr(r.Vals[r.Keys[len(r.Keys)-1]]))
	}
	if joined := strings.Join(details, " "); !strings.Contains(joined, "idx_session_admissions_cron_request") {
		t.Fatalf("plan %s", joined)
	}
}

func TestPromptRefreshEpochsOrderProfileAndSessionRequests(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	if n, err := db.PromptRefreshEpoch(ctx, "a"); n != 0 || err != nil {
		t.Fatalf("no request %d %v", n, err)
	}
	a := "a"
	if n, _ := db.RequestPromptRefresh(ctx, &a); n != 1 {
		t.Fatalf("session epoch %d", n)
	}
	if n, _ := db.PromptRefreshEpoch(ctx, "b"); n != 0 {
		t.Fatalf("session request leaked to b %d", n)
	}
	if n, _ := db.RequestPromptRefresh(ctx, nil); n != 2 {
		t.Fatalf("profile epoch %d", n)
	}
	if n, _ := db.RequestPromptRefresh(ctx, &a); n != 3 {
		t.Fatalf("second session epoch %d", n)
	}
	if x, y := mustEpoch(t, db, "a"), mustEpoch(t, db, "b"); x != 3 || y != 2 {
		t.Fatalf("epochs a=%d b=%d", x, y)
	}
	readSession(t, db, NewSession{ID: "p", Source: "cli", SystemPrompt: "stored"})
	readSession(t, db, NewSession{ID: "q", Source: "cli"})
	if n, err := db.CountSessionsWithStoredPrompt(ctx); n != 1 || err != nil {
		t.Fatalf("stored prompts %d %v", n, err)
	}
}

func mustEpoch(t *testing.T, db *SessionDB, sid string) int {
	t.Helper()
	n, err := db.PromptRefreshEpoch(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
