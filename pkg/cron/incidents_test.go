package cron

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func pointIncidentsDB(t *testing.T) (*Store, *IncidentStore, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	root := t.TempDir()
	incidentsExecutionsFile = filepath.Join(root, "cron", "executions.db")
	t.Cleanup(func() { incidentsExecutionsFile = "" })
	return s, openIncidents(s), root
}

func mustUpsert(t *testing.T, inc *IncidentStore, jobID, errText string) (string, bool) {
	t.Helper()
	id, isNew, err := inc.UpsertIncident(jobID, errText, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id, isNew
}

func mustIncident(t *testing.T, inc *IncidentStore, id string) *Incident {
	t.Helper()
	got, err := inc.GetIncident(id)
	if err != nil || got == nil {
		t.Fatal(id, got, err)
	}
	return got
}

func incidentCount(t *testing.T, inc *IncidentStore, st *string) int {
	t.Helper()
	n, err := inc.CountIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNewFailureCreatesIncidentAndIsNew(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	id, isNew := mustUpsert(t, inc, "job-1", "Provider timeout: read timed out")
	row := mustIncident(t, inc, id)
	if !isNew || !strings.HasPrefix(id, "job-1_") || row.JobID != "job-1" || row.State != "detected" || row.FailureType != "timeout" || row.FirstSeenAt != row.LastSeenAt || incidentCount(t, inc, nil) != 1 {
		t.Fatal(id, isNew, row)
	}
}

func TestSameSignatureDedupsSameIncident(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	id1, new1 := mustUpsert(t, inc, "job-1", "Provider timeout: read timed out")
	id2, new2 := mustUpsert(t, inc, "job-1", "PROVIDER TIMEOUT: read timed out   ")
	if id1 != id2 || !new1 || new2 || incidentCount(t, inc, nil) != 1 || mustIncident(t, inc, id1).State != "detected" {
		t.Fatal("normalized (case/whitespace) signature must dedup")
	}
}

func TestErrorChangeMintsNewIncident(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	id1, _ := mustUpsert(t, inc, "job-1", "provider timeout")
	id2, new2 := mustUpsert(t, inc, "job-1", "provider rate limit 429")
	if id1 == id2 || !new2 || incidentCount(t, inc, nil) != 2 {
		t.Fatal("changed error must mint a new incident")
	}
}

func TestIncidentsListNewestInstantFirstAcrossDSTFallBack(t *testing.T) {
	s, inc, _ := pointIncidentsDB(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	s.clk.zoneFn = func() *time.Location { return ny }
	first := time.Date(2026, 11, 1, 5, 50, 0, 0, time.UTC).In(ny)
	s.clk.nowFn = func() time.Time { return first }
	earlier, _ := mustUpsert(t, inc, "job-1", "first failure")
	second := time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC).In(ny)
	s.clk.nowFn = func() time.Time { return second }
	later, _ := mustUpsert(t, inc, "job-2", "second failure")
	rows, err := inc.ListIncidents(nil)
	if err != nil || len(rows) != 2 || rows[0].ID != later || rows[1].ID != earlier {
		t.Fatal(rows, err)
	}
	if !strings.HasSuffix(rows[0].LastSeenAt, "01:10:00-05:00") || !strings.HasSuffix(rows[1].LastSeenAt, "01:50:00-04:00") {
		t.Fatal(rows[0].LastSeenAt, rows[1].LastSeenAt)
	}
}

func TestRedactionAppliedToIncidentError(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	secret := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	id, _ := mustUpsert(t, inc, "job-1", "failed: "+secret+" boom")
	if row := mustIncident(t, inc, id); strings.Contains(row.Error, secret) || !strings.Contains(row.Error, "boom") {
		t.Fatal(row.Error)
	}
}

func TestErrorTruncatedToBoundedLength(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	id, _ := mustUpsert(t, inc, "job-1", strings.Repeat("x", 2000))
	if n := len([]rune(mustIncident(t, inc, id).Error)); n > 500 {
		t.Fatal(n)
	}
}

func TestFailureTypeClassification(t *testing.T) {
	pointIncidentsDB(t)
	cases := [][2]string{
		{"delivery failed for telegram chat", "delivery"},
		{"Provider read timed out after 60s", "timeout"},
		{"authentication failed: invalid API key", "auth"},
		{"HTTP 429: rate limit exceeded", "rate_limit"},
		{"configuration validation blocked the run", "config"},
		{"script exited with code 1", "script"},
		{"agent crashed mid-conversation", "agent"},
		{"something completely unexpected happened", "unknown"},
	}
	for _, c := range cases {
		if got := classifyFailureType(c[0]); got != c[1] {
			t.Fatal(c, got)
		}
	}
}

func TestIncidentSignatureMasksDurationsAndKeepsTypedFailure(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	if incidentErrorSignature("j", "idle for 603s") != incidentErrorSignature("j", "idle for 12.5 seconds") || incidentErrorSignature("j", "HTTP 500") == incidentErrorSignature("j", "HTTP 503") {
		t.Fatal("durations must be masked and status codes kept")
	}
	typed := "delivery"
	id, _, err := inc.UpsertIncidentTyped("job-9", "boom", nil, &typed, nil)
	if err != nil || mustIncident(t, inc, id).FailureType != "delivery" {
		t.Fatal(err)
	}
}

func TestLifecycleTransitions(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	id, _ := mustUpsert(t, inc, "job-1", "boom")
	if ok, _ := inc.SetIncidentState(id, "alerted"); !ok {
		t.Fatal("alerted")
	}
	if ok, _ := inc.SetIncidentState(id, "closed"); !ok {
		t.Fatal("closed")
	}
	row := mustIncident(t, inc, id)
	if row.State != "closed" || row.AckedAt == nil || row.ClosedAt == nil {
		t.Fatal(row)
	}
	for _, st := range []string{"alerted", "closed", "bogus"} {
		if ok, _ := inc.SetIncidentState(id, st); ok {
			t.Fatal(st)
		}
	}
	if ok, _ := inc.AckIncident(id); ok || mustIncident(t, inc, id).State != "closed" {
		t.Fatal("closed is terminal")
	}
	bogus := "bogus"
	if rows, _ := inc.ListIncidents(&bogus); len(rows) != 0 || incidentCount(t, inc, &bogus) != 0 {
		t.Fatal("bogus state filter")
	}
}

func TestAckedSignatureStaysClosedOnRefresh(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	id, _ := mustUpsert(t, inc, "job-1", "same failure text")
	_, _ = inc.AckIncident(id)
	same, isNew := mustUpsert(t, inc, "job-1", "SAME FAILURE TEXT")
	if same != id || isNew || mustIncident(t, inc, id).State != "closed" {
		t.Fatal("acked signature must stay closed on refresh")
	}
}

func TestMissingDBNoCrash(t *testing.T) {
	_, inc, root := pointIncidentsDB(t)
	id, isNew := mustUpsert(t, inc, "job-1", "boom")
	if info, err := os.Stat(filepath.Join(root, "cron", "executions.db")); !isNew || err != nil || !info.Mode().IsRegular() {
		t.Fatal(isNew, err)
	}
	rows, _ := inc.ListIncidents(nil)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], mustIncident(t, inc, id)) || incidentCount(t, inc, nil) != 1 {
		t.Fatal(rows)
	}
	if got, err := inc.GetIncident("nope"); got != nil || err != nil {
		t.Fatal(got, err)
	}
}

func TestMarkIncidentAlertedSetsStateNeverResurrects(t *testing.T) {
	s, inc, _ := pointIncidentsDB(t)
	id, _ := mustUpsert(t, inc, "job-1", "boom")
	s.markIncidentAlerted(&id)
	if mustIncident(t, inc, id).State != "alerted" {
		t.Fatal("alerted")
	}
	_, _ = inc.AckIncident(id)
	s.markIncidentAlerted(&id)
	if mustIncident(t, inc, id).State != "closed" {
		t.Fatal("resurrected")
	}
	s.markIncidentAlerted(nil)
	missing := "nonexistent"
	s.markIncidentAlerted(&missing)
}

func TestBestEffortIncidentStoreFailureReturnsFalse(t *testing.T) {
	s, _, root := pointIncidentsDB(t)
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	incidentsExecutionsFile = filepath.Join(blocker, "cron", "executions.db")
	if withheld, incID := s.upsertIncidentForFailure(&Job{ID: "abc123", Name: "j"}, "boom", nil); withheld || incID != nil {
		t.Fatal(withheld, incID)
	}
}

func TestAlertedGateHonoursCooldownOptOutAndLegacyRows(t *testing.T) {
	s, inc, _ := pointIncidentsDB(t)
	setHours := func(v string) {
		if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  failure_repeat_alert_hours: "+v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setHours("6.0")
	job := &Job{ID: "abc123", Name: "j"}
	check := func(wantWithheld bool, wantID string) {
		t.Helper()
		withheld, id := s.upsertIncidentForFailure(job, "repeat boom", nil)
		if withheld != wantWithheld || id == nil || *id != wantID {
			t.Fatal(withheld, id, wantWithheld, wantID)
		}
	}
	withheld, idp := s.upsertIncidentForFailure(job, "repeat boom", nil)
	if withheld || idp == nil {
		t.Fatal(withheld, idp)
	}
	id := *idp
	s.markIncidentAlerted(&id)
	if mustIncident(t, inc, id).State != "alerted" {
		t.Fatal("alerted")
	}
	check(true, id)
	setHours("0")
	check(false, id)
	setHours("6.0")
	if err := inc.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE cron_incidents SET alerted_at=NULL WHERE id=?", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check(false, id)
	s.markIncidentAlerted(&id)
	if mustIncident(t, inc, id).AlertedAt == nil {
		t.Fatal("alerted_at not restamped")
	}
	check(true, id)
	if ok, _ := inc.AckIncident(id); !ok {
		t.Fatal("ack")
	}
	setHours("0")
	check(true, id)
}

func TestSuccessfulRunResolvesOpenIncidentsAndARepeatReopensThem(t *testing.T) {
	s, inc, _ := pointIncidentsDB(t)
	id, _ := mustUpsert(t, inc, "job-1", "provider 503")
	s.markIncidentAlerted(&id)
	other, _ := mustUpsert(t, inc, "job-2", "provider 503")
	composed := s.composeRunDelivery(&Job{ID: "job-1", Name: "j1"}, true, nil, "all good", nil, false)
	if composed.content != "all good" {
		t.Fatalf("content %q", composed.content)
	}
	resolved := "resolved"
	if row := mustIncident(t, inc, id); row.State != "resolved" || row.ClosedAt == nil || mustIncident(t, inc, other).State != "detected" || incidentCount(t, inc, &resolved) != 1 {
		t.Fatal(row)
	}
	same, reopened := mustUpsert(t, inc, "job-1", "provider 503")
	if row := mustIncident(t, inc, id); same != id || !reopened || row.State != "detected" || row.ClosedAt != nil {
		t.Fatal(row)
	}
}

func TestOperatorAckSurvivesASuccessfulRun(t *testing.T) {
	_, inc, _ := pointIncidentsDB(t)
	flaky, _ := mustUpsert(t, inc, "job-1", "known flaky")
	_, _ = inc.AckIncident(flaky)
	if n, err := inc.CloseIncidentsForRecoveredJob("job-1"); n != 0 || err != nil || mustIncident(t, inc, flaky).State != "closed" {
		t.Fatal(n, err)
	}
	if _, reopened := mustUpsert(t, inc, "job-1", "known flaky"); reopened || mustIncident(t, inc, flaky).State != "closed" {
		t.Fatal("operator ack must survive")
	}
}

func TestCliListAndAck(t *testing.T) {
	s, inc, _ := pointIncidentsDB(t)
	ctx := context.Background()
	id, _ := mustUpsert(t, inc, "job-1", "provider timeout boom")
	run := func(a *CronArgs) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code, err := s.cronIncidents(ctx, &out, a)
		if err != nil {
			t.Fatal(err)
		}
		return code, out.String()
	}
	if code, out := run(&CronArgs{IncidentAction: "list"}); code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "job-1") {
		t.Fatalf("list %d %q", code, out)
	}
	closed := "closed"
	if code, out := run(&CronArgs{IncidentAction: "list", State: &closed}); code != 0 || !strings.Contains(out, "No cron failure incidents recorded.") {
		t.Fatalf("filtered list %d %q", code, out)
	}
	ack := &CronArgs{IncidentAction: "ack", IncidentID: id}
	code, out := run(ack)
	if code != 0 || mustIncident(t, inc, id).State != "closed" || !strings.Contains(strings.ToLower(out), "acknowledged") {
		t.Fatalf("ack %d %q", code, out)
	}
	if code, out := run(ack); code != 0 || !strings.Contains(strings.ToLower(out), "already closed") {
		t.Fatalf("second ack %d %q", code, out)
	}
	if code, _ := run(&CronArgs{IncidentAction: "ack"}); code != 1 {
		t.Fatalf("missing id exit %d", code)
	}
}
