package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const ledgerDeadPid = int64(1<<22 + 12345)

var bg = context.Background()

func writeLedgerFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, path, data)
}

func writeLedgerSentinel(t *testing.T, h string, payload *pyjson.Object) {
	t.Helper()
	writeRecord(t, GetLifecycleSentinelPath(bg, h), payload)
}

func readLedgerSentinel(t *testing.T, h string) *pyjson.Object {
	t.Helper()
	return readRecord(t, GetLifecycleSentinelPath(bg, h))
}

func exitDiagRecords(t *testing.T, h string) []*pyjson.Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, "logs", "gateway-exit-diag.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []*pyjson.Object
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := pyjson.Loads(line)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v.(*pyjson.Object))
	}
	return out
}

func deadSentinel(startedAt string) *pyjson.Object {
	return rec("phase", "running", "pid", ledgerDeadPid, "start_time", 1000.0, "started_at", startedAt)
}

func fixedCreateTime(v float64) func(int) *float64 { return func(int) *float64 { return &v } }

func TestSampleMemoryHasExpectedKeysOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux only")
	}
	sample := SampleMemory()
	rss, _ := recGet(sample, "rss_kib").(int64)
	total, _ := recGet(sample, "mem_total_kib").(int64)
	if _, ok := sample.Get("mem_available_kib"); rss <= 0 || total <= 0 || !ok {
		t.Fatalf("%v", sample)
	}
}

func TestFirstBootReportsNothingAndClaimsSentinel(t *testing.T) {
	h := t.TempDir()
	if ev := RecordStartup(bg, h, true); ev != nil {
		t.Fatalf("evidence %v", ev)
	}
	s := readLedgerSentinel(t, h)
	if _, ok := s.Get("start_time"); recGet(s, "phase") != "running" || recGet(s, "pid") != int64(os.Getpid()) || !ok {
		t.Fatalf("%v", s)
	}
}

func TestCleanExitThenBootReportsNothing(t *testing.T) {
	h := t.TempDir()
	RecordStartup(bg, h, true)
	zero := 0
	MarkExited(bg, &zero, "graceful_shutdown", h)
	s := readLedgerSentinel(t, h)
	if recGet(s, "phase") != "exited" || recGet(s, "exit_code") != int64(0) || recGet(s, "exit_reason") != "graceful_shutdown" {
		t.Fatalf("%v", s)
	}
	if ev := RecordStartup(bg, h, true); ev != nil || len(exitDiagRecords(t, h)) != 0 {
		t.Fatalf("evidence %v", ev)
	}
}

func TestRunningSentinelFromDeadPidIsUnclean(t *testing.T) {
	h := t.TempDir()
	writeLedgerSentinel(t, h, deadSentinel("2026-07-11T04:30:00+00:00"))
	ev, err := DetectUncleanExit(bg, h)
	if err != nil || ev == nil || recGet(ev, "prior_pid") != ledgerDeadPid || recGet(ev, "prior_started_at") != "2026-07-11T04:30:00+00:00" {
		t.Fatalf("%v %v", ev, err)
	}
}

func TestRecordStartupPersistsUncleanReportAndReclaims(t *testing.T) {
	h := t.TempDir()
	writeLedgerSentinel(t, h, deadSentinel("2026-07-11T04:30:00+00:00"))
	if ev := RecordStartup(bg, h, true); ev == nil {
		t.Fatal("no evidence")
	}
	records := exitDiagRecords(t, h)
	if len(records) != 1 || recGet(records[0], "tag") != "gateway.previous_unclean_exit" ||
		recGet(records[0], "prior_pid") != ledgerDeadPid || recGet(records[0], "pid") != int64(os.Getpid()) {
		t.Fatalf("%v", records)
	}
	s := readLedgerSentinel(t, h)
	if recGet(s, "phase") != "running" || recGet(s, "pid") != int64(os.Getpid()) {
		t.Fatalf("%v", s)
	}
}

func TestRecordStartupCarriesUncleanFlagsOntoNewSentinel(t *testing.T) {
	h := t.TempDir()
	writeLedgerSentinel(t, h, deadSentinel("2026-07-11T04:30:00+00:00"))
	writeRecord(t, GetLoopHeartbeatPath(bg, h), rec("pid", ledgerDeadPid, "updated_at", "2026-07-11T05:00:00+00:00",
		"mem", rec("mem_total_kib", int64(1024*1024), "mem_available_kib", int64(20*1024))))
	ev := RecordStartup(bg, h, true)
	if ev == nil || recGet(ev, "suspected_oom") != true {
		t.Fatalf("%v", ev)
	}
	s := readLedgerSentinel(t, h)
	if recGet(s, "phase") != "running" || recGet(s, "prior_unclean_exit") != true || recGet(s, "prior_suspected_oom") != true {
		t.Fatalf("%v", s)
	}
}

func TestRecordStartupCleanBootHasNoPriorFlags(t *testing.T) {
	h := t.TempDir()
	writeLedgerSentinel(t, h, rec("phase", "exited", "pid", ledgerDeadPid, "exit_code", int64(0), "exit_reason", "graceful_shutdown"))
	if ev := RecordStartup(bg, h, true); ev != nil {
		t.Fatalf("%v", ev)
	}
	s := readLedgerSentinel(t, h)
	_, unclean := s.Get("prior_unclean_exit")
	_, oom := s.Get("prior_suspected_oom")
	if recGet(s, "phase") != "running" || unclean || oom {
		t.Fatalf("%v", s)
	}
}

func TestMarkExitedLeavesPidNoneSentinelAlone(t *testing.T) {
	h := t.TempDir()
	writeLedgerSentinel(t, h, rec("phase", "running", "pid", nil, "start_time", 2000.0))
	zero := 0
	MarkExited(bg, &zero, "graceful_shutdown", h)
	s := readLedgerSentinel(t, h)
	if recGet(s, "phase") != "running" || recGet(s, "pid") != nil {
		t.Fatalf("%v", s)
	}
}

func TestPriorExitLabelSurvivesCorruptSentinel(t *testing.T) {
	h := t.TempDir()
	writeLedgerFile(t, GetLifecycleSentinelPath(bg, h), "garbage")
	if got := ReadPriorExitLabel(h); got != "unknown" {
		t.Fatal(got)
	}
}

func TestSentinelCarriesProcessBirthThroughExit(t *testing.T) {
	h := t.TempDir()
	patch(t, &ledgerProcessCreateTime, fixedCreateTime(1234.5))
	RecordStartup(bg, h, true)
	running := readLedgerSentinel(t, h)
	if recGet(running, "create_time") != 1234.5 {
		t.Fatalf("%v", running)
	}
	zero := 0
	MarkExited(bg, &zero, "graceful_shutdown", h)
	exited := readLedgerSentinel(t, h)
	if recGet(exited, "phase") != "exited" || recGet(exited, "start_time") != recGet(running, "start_time") || recGet(exited, "create_time") != 1234.5 {
		t.Fatalf("%v", exited)
	}
}

func TestReplaceHandoverIsNotADeathAndPidReuseIs(t *testing.T) {
	h := t.TempDir()
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &ledgerProcessCreateTime, fixedCreateTime(5000.0))
	live := func(kv ...any) *pyjson.Object {
		r := rec("phase", "running", "pid", int64(4242), "start_time", 5003.7, "started_at", "x")
		for i := 0; i+1 < len(kv); i += 2 {
			r.Set(kv[i].(string), kv[i+1])
		}
		return r
	}
	for _, c := range []struct {
		sentinel *pyjson.Object
		death    bool
	}{
		{live("create_time", 5000.0), false},
		{live("create_time", 4000.0), true},
		{live(), false},
		{live("start_time", 4000.0), true},
	} {
		writeLedgerSentinel(t, h, c.sentinel)
		ev, err := DetectUncleanExit(bg, h)
		if err != nil || (ev != nil) != c.death {
			t.Fatalf("%v: evidence %v err %v", c.sentinel, ev, err)
		}
	}
}

func makeStateDB(t *testing.T, h string, corrupt bool, journalMode string, rows int) string {
	t.Helper()
	path := filepath.Join(h, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if journalMode != "delete" {
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode=" + journalMode).Scan(&mode); err != nil || mode != journalMode {
			t.Fatalf("journal mode %q %v", mode, err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("CREATE TABLE sessions (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO sessions (v) VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if _, err := stmt.Exec(strings.Repeat(fmt.Sprintf("row-%d", i), 40)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if corrupt {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte(strings.Repeat("\xEF", 4096)), 4096*6)
		f.Close()
	}
	return path
}

func writeIncidentSentinel(t *testing.T, h, phase string) {
	t.Helper()
	writeLedgerSentinel(t, h, rec("phase", phase, "pid", ledgerDeadPid, "start_time", 1000.0, "started_at", "2026-08-26T23:56:45+00:00"))
}

func checkIntegrity(h string, deadline *float64) string {
	return CheckStateDBIntegrity(bg, h, deadline)
}

func TestCheckerPassesAHealthyStore(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, false, "delete", 4000)
	if got := checkIntegrity(h, nil); got != "ok" {
		t.Fatal(got)
	}
}

func TestCheckerReportsATornBtreePage(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, true, "delete", 4000)
	got := checkIntegrity(h, nil)
	if got == "ok" || !(strings.Contains(got, "btreeInitPage") || strings.Contains(strings.ToLower(got), "malformed")) {
		t.Fatal(got)
	}
}

func TestCheckerToleratesAMissingStore(t *testing.T) {
	if got := checkIntegrity(t.TempDir(), nil); got != "absent" {
		t.Fatal(got)
	}
}

func TestUncleanExitRecordsTheCorruptionVerdict(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, true, "delete", 4000)
	writeIncidentSentinel(t, h, "running")
	ev := RecordStartup(bg, h, false)
	if ev == nil || recGet(ev, "state_db_integrity") == "ok" || recGet(exitDiagRecords(t, h)[0], "state_db_integrity") == "ok" {
		t.Fatalf("%v", ev)
	}
}

func TestUncleanExitOnAHealthyStoreRecordsOk(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, false, "delete", 4000)
	writeIncidentSentinel(t, h, "running")
	if ev := RecordStartup(bg, h, false); ev == nil || recGet(ev, "state_db_integrity") != "ok" {
		t.Fatalf("%v", ev)
	}
}

func TestCleanExitDoesNotPayForTheCheck(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, true, "delete", 4000)
	writeIncidentSentinel(t, h, "exited")
	called := 0
	patch(t, &checkStateDBIntegrity, func(context.Context, string, *float64) string { called++; return "ok" })
	RecordStartup(bg, h, true)
	if called != 0 {
		t.Fatal("integrity check ran on a clean boot")
	}
}

func TestDeferredStartupNeverRunsTheCheckInline(t *testing.T) {
	h := t.TempDir()
	var started atomic.Bool
	released := make(chan struct{})
	defer close(released)
	patch(t, &checkStateDBIntegrity, func(context.Context, string, *float64) string {
		started.Store(true)
		select {
		case <-released:
		case <-time.After(10 * time.Second):
		}
		return "ok"
	})
	writeIncidentSentinel(t, h, "running")
	begin := time.Now()
	ev := RecordStartup(bg, h, true)
	if ev == nil || recGet(ev, "state_db_integrity") != "deferred" || started.Load() || time.Since(begin) >= 5*time.Second {
		t.Fatalf("%v started %v", ev, started.Load())
	}
	r := exitDiagRecords(t, h)[0]
	if recGet(r, "state_db_integrity") != "deferred" || recGet(r, "tag") != "gateway.previous_unclean_exit" {
		t.Fatalf("%v", r)
	}
}

func TestDeferredScanAppendsASecondRecordAndReturnsTheVerdict(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, true, "wal", 4000)
	writeIncidentSentinel(t, h, "running")
	ev := RecordStartup(bg, h, true)
	if ev == nil || recGet(ev, "state_db_integrity") != "deferred" {
		t.Fatalf("%v", ev)
	}
	verdict := RunDeferredIntegrityCheck(bg, ev, h, 60.0)
	records := exitDiagRecords(t, h)
	if len(records) != 2 || recGet(records[0], "tag") != "gateway.previous_unclean_exit" || recGet(records[1], "tag") != "gateway.state_db_integrity" {
		t.Fatalf("%v", records)
	}
	d := records[1]
	if recGet(d, "deferred") != true || recGet(d, "state_db_integrity") != verdict || recGet(d, "prior_pid") != ledgerDeadPid ||
		verdict == "ok" || recGet(ev, "state_db_integrity") != verdict {
		t.Fatalf("%v %q", d, verdict)
	}
}

func TestDeferredScanNeverRaises(t *testing.T) {
	h := t.TempDir()
	writeIncidentSentinel(t, h, "running")
	ev := RecordStartup(bg, h, true)
	patch(t, &checkStateDBIntegrity, func(context.Context, string, *float64) string { panic("boom") })
	if got := RunDeferredIntegrityCheck(bg, ev, h, StateDBIntegrityDeadlineS); !strings.HasPrefix(got, "check-failed") {
		t.Fatal(got)
	}
}

// Records every scan while still running the real one
func spyOnScan(t *testing.T) *int {
	calls := new(int)
	real := checkStateDBIntegrity
	patch(t, &checkStateDBIntegrity, func(ctx context.Context, h string, d *float64) string {
		*calls++
		return real(ctx, h, d)
	})
	return calls
}

func TestDeferredScanStillRunsOnAWalStore(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, false, "wal", 4000)
	calls := spyOnScan(t)
	verdict := RunDeferredIntegrityCheck(bg, rec("prior_pid", ledgerDeadPid), h, 60.0)
	records := exitDiagRecords(t, h)
	if verdict != "ok" || *calls != 1 || recGet(records[len(records)-1], "state_db_integrity") != "ok" {
		t.Fatalf("%q %d", verdict, *calls)
	}
}

func TestDeferredScanSkipsARollbackJournalStore(t *testing.T) {
	h := t.TempDir()
	path := makeStateDB(t, h, false, "delete", 4000)
	calls := spyOnScan(t)
	ev := rec("prior_pid", ledgerDeadPid)
	logs := captureLogs(t)
	verdict := RunDeferredIntegrityCheck(bg, ev, h, 60.0)
	want := "skipped: rollback-journal store (journal_mode=delete), scan would block live writes"
	if *calls != 0 || verdict != want || recGet(ev, "state_db_integrity") != verdict {
		t.Fatalf("%q calls %d", verdict, *calls)
	}
	r := exitDiagRecords(t, h)
	last := r[len(r)-1]
	if recGet(last, "tag") != "gateway.state_db_integrity" || recGet(last, "state_db_integrity") != verdict || recGet(last, "prior_pid") != ledgerDeadPid {
		t.Fatalf("%v", last)
	}
	var warnings []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(strings.ToLower(warnings[0]), "unclean") || !strings.Contains(strings.ToLower(warnings[0]), "skipped") ||
		!strings.Contains(warnings[0], path) || !strings.Contains(warnings[0], "PRAGMA quick_check") {
		t.Fatalf("%v", warnings)
	}
	writer, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, q := range []string{"BEGIN EXCLUSIVE", "INSERT INTO sessions (v) VALUES ('live')", "COMMIT"} {
		if _, err := writer.ExecContext(bg, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestDeferredScanSkipsWhenTheJournalModeIsUndeterminable(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, false, "wal", 4000)
	calls := spyOnScan(t)
	patch(t, &ledgerOnDiskJournalMode, func(context.Context, state.SQLiteConn) (string, bool, error) {
		return "", false, &state.SQLiteError{Class: state.SQLiteDatabaseError, Msg: "file is not a database"}
	})
	verdict := RunDeferredIntegrityCheck(bg, rec("prior_pid", ledgerDeadPid), h, 60.0)
	want := "skipped: rollback-journal store (journal_mode=unknown), scan would block live writes"
	r := exitDiagRecords(t, h)
	if *calls != 0 || verdict != want || recGet(r[len(r)-1], "state_db_integrity") != verdict {
		t.Fatalf("%q calls %d", verdict, *calls)
	}
}

func TestDeferredScanOfAnAbsentStoreIsUnchanged(t *testing.T) {
	h := t.TempDir()
	verdict := RunDeferredIntegrityCheck(bg, rec("prior_pid", ledgerDeadPid), h, 60.0)
	r := exitDiagRecords(t, h)
	if _, err := os.Stat(filepath.Join(h, "state.db")); verdict != "absent" || recGet(r[len(r)-1], "state_db_integrity") != "absent" || err == nil {
		t.Fatalf("%q", verdict)
	}
}

func TestDeadlineBoundsTheScan(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, false, "delete", 300_000)
	patch(t, &integrityCheckProgressOps, 1_000)
	deadline := 0.01
	if got := checkIntegrity(h, &deadline); !strings.HasPrefix(got, "check-incomplete: exceeded") {
		t.Fatal(got)
	}
}

func TestNoDeadlineKeepsTheHistoricVerdictContract(t *testing.T) {
	h := t.TempDir()
	makeStateDB(t, h, false, "delete", 300_000)
	patch(t, &integrityCheckProgressOps, 1_000)
	if got := checkIntegrity(h, nil); got != "ok" {
		t.Fatal(got)
	}
}

type leaseRecorder struct {
	count atomic.Int64
	phase atomic.Value
}

// Startup progress lease installed the way an armed startup watchdog installs it
func armedLease(t *testing.T) *leaseRecorder {
	r := &leaseRecorder{}
	state.SetStartupProgressLease(func(expectedS float64, phase string) {
		r.count.Add(1)
		r.phase.Store(phase)
	})
	t.Cleanup(func() { state.SetStartupProgressLease(nil) })
	return r
}

func TestUncleanExitCheckRenewsTheStartupLeaseWhileSqliteProgresses(t *testing.T) {
	h := t.TempDir()
	lease := armedLease(t)
	patch(t, &integrityCheckLeaseRenewS, 0.0)
	patch(t, &integrityCheckProgressOps, 1_000)
	makeStateDB(t, h, false, "delete", 4000)
	writeIncidentSentinel(t, h, "running")
	ev := RecordStartup(bg, h, false)
	if ev == nil || recGet(ev, "state_db_integrity") != "ok" {
		t.Fatalf("%v", ev)
	}
	if lease.phase.Load() != "state_db_unclean_integrity_check" || lease.count.Load() <= 1 {
		t.Fatalf("phase %v count %d", lease.phase.Load(), lease.count.Load())
	}
}

func TestUncleanExitCheckKeepsTheVerdictContractUnderTheLease(t *testing.T) {
	h := t.TempDir()
	lease := armedLease(t)
	makeStateDB(t, h, true, "delete", 4000)
	verdict := checkIntegrity(h, nil)
	if verdict == "ok" || verdict == "absent" || strings.HasPrefix(verdict, "check-failed") {
		t.Fatal(verdict)
	}
	if lease.phase.Load() != "state_db_unclean_integrity_check" {
		t.Fatalf("phase %v", lease.phase.Load())
	}
	if got := checkIntegrity(filepath.Join(h, "nowhere"), nil); got != "absent" {
		t.Fatal(got)
	}
}
