package cron

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var missedErrorRe = regexp.MustCompile(`^missed \x{00d7}(\d+) \((.*)\)$`)

// Missed rows of the store ledger oldest first as job:count:detail
func bookedMissed(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := openExecutionsLedger(s).List(context.Background(), nil, 500, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i]["status"] != "missed" {
			continue
		}
		m := missedErrorRe.FindStringSubmatch(recText(rows[i], "error"))
		if m == nil {
			t.Fatalf("missed row error %q", rows[i]["error"])
		}
		out = append(out, fmt.Sprintf("%s:%s:%s", rows[i]["job_id"], m[1], m[2]))
	}
	return out
}

// Ledger rows the store claims as another live or dead process with the given claim time fingerprint
func seedForeignRunning(t *testing.T, l *Ledger, jobID string, startTime int64) string {
	t.Helper()
	prevID, prevStart := executionProcessID, ledgerStartTime
	executionProcessID = "ext-proc"
	ledgerStartTime = func(int) (int64, bool) { return startTime, true }
	defer func() { executionProcessID, ledgerStartTime = prevID, prevStart }()
	rec, err := l.Create(context.Background(), jobID, "cron", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := recText(rec, "id")
	if running, err := l.MarkRunning(context.Background(), id); err != nil || running == nil {
		t.Fatalf("MarkRunning = %v %v", running, err)
	}
	return id
}

func pinLedgerProcess(t *testing.T, exists bool, start func(int) (int64, bool)) {
	t.Helper()
	prevExists, prevStart := ledgerPidExists, ledgerStartTime
	ledgerPidExists = func(int) bool { return exists }
	ledgerStartTime = start
	t.Cleanup(func() { ledgerPidExists, ledgerStartTime = prevExists, prevStart })
}

func latestStatus(t *testing.T, l *Ledger, jobID string) string {
	t.Helper()
	rec, err := l.Latest(context.Background(), jobID)
	if err != nil || rec == nil {
		t.Fatalf("Latest = %v %v", rec, err)
	}
	return recText(rec, "status")
}

func TestDriftWithinToleranceKeepsExecutionRunning(t *testing.T) {
	const claimed = int64(178864182760)
	for _, tc := range []struct {
		drift    int64
		wantLive bool
	}{{100, true}, {3600, false}} {
		s := newClockStore(t, nil, time.UTC)
		l := openExecutionsLedger(s)
		id := seedForeignRunning(t, l, "job-drift", claimed)
		pinLedgerProcess(t, true, func(int) (int64, bool) { return claimed + tc.drift, true })
		recovered, err := l.RecoverInterrupted(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want, wantN := "unknown", 1
		if tc.wantLive {
			want, wantN = "running", 0
		}
		if recovered != wantN || latestStatus(t, l, "job-drift") != want {
			t.Fatalf("drift %d: recovered %d status %s", tc.drift, recovered, latestStatus(t, l, "job-drift"))
		}
		if rec, _ := l.Get(context.Background(), id); tc.wantLive == false && recText(rec, "error") != ownerGoneReason {
			t.Fatalf("reason %q", rec["error"])
		}
	}
}

func TestUnreadableStartTimeFailSafe(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	l := openExecutionsLedger(s)
	seedForeignRunning(t, l, "job-drift", 178864182760)
	pinLedgerProcess(t, true, func(int) (int64, bool) { return 0, false })
	if n, err := l.RecoverInterrupted(context.Background()); err != nil || n != 0 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if latestStatus(t, l, "job-drift") != "running" {
		t.Fatal("unreadable start time reaped a live row")
	}
}

func TestLiveOwnerWedgedPastDerivedBoundIsRecoveredUnlessItStampsProgress(t *testing.T) {
	t.Setenv("ICEBATH_CRON_TIMEOUT", "600")
	cur := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := mutableClockStore(t, &cur, time.UTC)
	l := openExecutionsLedger(s)
	pinLedgerProcess(t, true, func(int) (int64, bool) { return 42, true })
	wedged := seedForeignRunning(t, l, "wedged", 42)
	healthy := seedForeignRunning(t, l, "healthy", 42)
	if bound, ok := s.liveOwnerStaleAfterSeconds(context.Background()); !ok || bound != 7200 {
		t.Fatalf("bound %v %v", bound, ok)
	}
	cur = cur.Add(2*time.Hour + time.Minute)
	editExecution(t, l, healthy, "progress_at", FormatISO(cur.Add(-time.Minute)))
	n, err := l.RecoverInterrupted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	rec, _ := l.Get(context.Background(), wedged)
	if recText(rec, "status") != "unknown" || recText(rec, "error") != ownerWedgedReason {
		t.Fatalf("wedged row %v", rec)
	}
	if latestStatus(t, l, "healthy") != "running" {
		t.Fatal("a run stamping progress was reclaimed")
	}
	t.Setenv("ICEBATH_CRON_TIMEOUT", "0")
	cur = cur.Add(10 * time.Hour)
	if n, err := l.RecoverInterrupted(context.Background()); err != nil || n != 0 {
		t.Fatalf("unlimited inactivity reclaimed %d %v", n, err)
	}
}

func editExecution(t *testing.T, l *Ledger, id, column string, value any) {
	t.Helper()
	db, err := sql.Open("sqlite", l.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE executions SET "+column+"=? WHERE id=?", value, id); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerLifecycleOwnershipAndQueries(t *testing.T) {
	cur := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := mutableClockStore(t, &cur, time.UTC)
	l := openExecutionsLedger(s)
	ctx := context.Background()
	rec, err := l.Create(ctx, "job-a", "cron", strPtr("2026-05-01T05:00:00-07:00"))
	if err != nil || recText(rec, "status") != "claimed" || rec["scheduled_instant"] != "2026-05-01T12:00:00+00:00" {
		t.Fatalf("create = %v %v", rec, err)
	}
	id := recText(rec, "id")
	if len(id) != 32 || rec["process_id"] != executionProcessID {
		t.Fatalf("identity %v", rec)
	}
	if err := l.SetOccurrence(ctx, id, strPtr("2026-05-01T13:00:00+00:00")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.TouchProgress(ctx, id); !ok {
		t.Fatal("own claimed row not stamped")
	}
	if r, _ := l.MarkRunning(ctx, id); recText(r, "status") != "running" {
		t.Fatalf("running %v", r)
	}
	if r, _ := l.MarkRunning(ctx, id); r != nil {
		t.Fatal("running twice")
	}
	if err := l.SetOccurrence(ctx, id, nil); err == nil || err.Error() != "Cron occurrence could not be bound before dispatch" {
		t.Fatalf("bind after start = %v", err)
	}
	live, _ := l.Cancellable(ctx, "job-a")
	if recText(live, "id") != id {
		t.Fatalf("cancellable %v", live)
	}
	cur = cur.Add(time.Minute)
	queued := "queued"
	done, err := l.Finish(ctx, id, true, nil, &queued, false)
	if err != nil || recText(done, "status") != "completed" || done["error"] != nil {
		t.Fatalf("finish %v %v", done, err)
	}
	if again, _ := l.Finish(ctx, id, false, strPtr("late"), nil, false); again != nil {
		t.Fatal("terminal row rewritten")
	}
	if ok, _ := l.SettleDeliveryOutcome(ctx, id, "delivered"); !ok {
		t.Fatal("queued outcome not settled")
	}
	if ok, _ := l.SettleDeliveryOutcome(ctx, id, "failed"); ok {
		t.Fatal("settled outcome rewritten")
	}
	stamps, err := completedExecutions(s, "job-a", "2026-05-01T13:00:00+00:00")
	if err != nil || len(stamps) != 1 || stamps[0].ID != id || deref(stamps[0].FinishedAt) != FormatISO(cur) {
		t.Fatalf("completed stamps %v %v", stamps, err)
	}
	failing, _ := l.Create(ctx, "job-a", "manual", nil)
	failed, _ := l.Finish(ctx, recText(failing, "id"), false, nil, nil, false)
	if failed["error"] != "unknown failure" {
		t.Fatalf("failure detail %v", failed["error"])
	}
	cancelling, _ := l.Create(ctx, "job-b", "cron", nil)
	cancelled, _ := l.Finish(ctx, recText(cancelling, "id"), false, strPtr("stop"), nil, true)
	if recText(cancelled, "status") != "cancelled" {
		t.Fatalf("cancel %v", cancelled)
	}
	cur = cur.Add(time.Minute)
	if err := recordMissedOccurrences(s, "job-b", 3, strPtr("2026-05-01T09:00:00+00:00"), "skipped"); err != nil {
		t.Fatal(err)
	}
	if got := bookedMissed(t, s); len(got) != 1 || got[0] != "job-b:3:skipped" {
		t.Fatalf("missed %v", got)
	}
	if r, _ := l.RecordMissed(ctx, "job-b", 0, nil, "none"); r != nil {
		t.Fatal("zero missed booked")
	}
	latest, err := latestExecutions(s, []string{"job-a", "job-b", "job-a", ""})
	if err != nil || recText(latest["job-a"], "id") != recText(failing, "id") || recText(latest["job-b"], "status") != "missed" {
		t.Fatalf("latest %v %v", latest, err)
	}
	page, _ := l.List(ctx, strPtr("job-a"), 1, nil)
	before := recText(page[0], "claimed_at")
	older, _ := l.List(ctx, strPtr("job-a"), 50, &before)
	if len(page) != 1 || recText(page[0], "id") != recText(failing, "id") || len(older) != 1 || recText(older[0], "id") != id {
		t.Fatalf("cursor pages %v %v", page, older)
	}
}

func TestFirerGiveUpAndDeadOwnerTerminalisation(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	l := openExecutionsLedger(s)
	ctx := context.Background()
	own, _ := l.Create(ctx, "job-g", "cron", nil)
	if ok, _ := l.TerminalizeDeadOwner(ctx, recText(own, "id"), "worker exited 1"); ok {
		t.Fatal("own row terminalised as a dead owner")
	}
	if ok, _ := l.GiveUp(ctx, recText(own, "id"), "gave up"); !ok || latestStatus(t, l, "job-g") != "unknown" {
		t.Fatal("give up did not record unknown")
	}
	foreign := seedForeignRunning(t, l, "job-d", 7)
	pinLedgerProcess(t, true, func(int) (int64, bool) { return 7, true })
	if ok, _ := l.TerminalizeDeadOwner(ctx, foreign, "worker exited 1"); ok {
		t.Fatal("live worker terminalised")
	}
	if live, _ := l.OwnerLive(ctx, foreign, "job-d"); !live {
		t.Fatal("live foreign owner read as gone")
	}
	pinLedgerProcess(t, false, func(int) (int64, bool) { return 7, true })
	if live, _ := liveExecutions(s, []string{"job-d"}); len(live) != 0 {
		t.Fatalf("dead owner listed live %v", live)
	}
	if ok, err := l.TerminalizeDeadOwner(ctx, foreign, "worker exited 1"); err != nil || !ok {
		t.Fatalf("dead worker not terminalised %v", err)
	}
	if rec, _ := l.Get(ctx, foreign); recText(rec, "error") != "worker exited 1" {
		t.Fatalf("cause %v", rec["error"])
	}
	if rec, _ := l.RecoverReceipted(ctx, foreign, "job-d", true, nil, strPtr("delivered")); recText(rec, "status") != "completed" {
		t.Fatalf("receipt recovery %v", rec)
	}
	if !strings.HasSuffix(l.path, filepath.Join("cron", "executions.db")) {
		t.Fatalf("ledger path %s", l.path)
	}
}
