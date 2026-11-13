package gateway

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type ledgerTestRow struct {
	State          string
	Attempts       int
	OwnerPid       any
	OwnerStartedAt any
	Content        string
	LastError      *string
	UpdatedAt      float64
	AdapterProfile *string
}

func freshLedger(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", dir)
	db := filepath.Join(dir, "state.db")
	patch(t, &ledgerDBPath, func() string { return db })
	return db
}

type ledgerRecordOpts struct {
	oid, sessionKey, platform, chatID, content string
	thread, profile                            *string
}

func ledgerRecord(t *testing.T, o ledgerRecordOpts) {
	t.Helper()
	thread := "171.001"
	ob := DeliveryObligation{ObligationID: "ob-1", SessionKey: "agent:main:slack:channel:C1", Platform: "slack",
		ChatID: "C1", ThreadID: &thread, Content: "the final answer", AdapterProfile: o.profile}
	if o.oid != "" {
		ob.ObligationID = o.oid
	}
	if o.sessionKey != "" {
		ob.SessionKey = o.sessionKey
	}
	if o.platform != "" {
		ob.Platform = o.platform
	}
	if o.chatID != "" {
		ob.ChatID = o.chatID
	}
	if o.content != "" {
		ob.Content = o.content
	}
	if o.thread != nil {
		ob.ThreadID = o.thread
	}
	if err := RecordObligation(context.Background(), ob); err != nil {
		t.Fatal(err)
	}
}

func ledgerExec(t *testing.T, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := ledgerConnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func ledgerRowOf(t *testing.T, oid string) *ledgerTestRow {
	t.Helper()
	ctx := context.Background()
	conn, err := ledgerConnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var r ledgerTestRow
	err = conn.QueryRowContext(ctx, `SELECT state, attempts, owner_pid, owner_started_at, content, last_error,
		updated_at, adapter_profile FROM delivery_obligations WHERE obligation_id=?`, oid).Scan(&r.State, &r.Attempts,
		&r.OwnerPid, &r.OwnerStartedAt, &r.Content, &r.LastError, &r.UpdatedAt, &r.AdapterProfile)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func ledgerOrphan(t *testing.T, oid string) {
	t.Helper()
	ledgerExec(t, "UPDATE delivery_obligations SET owner_pid=999999999, owner_started_at=1 WHERE obligation_id=?", oid)
}

func ledgerMarkFailed(t *testing.T, oid, text string) {
	t.Helper()
	if err := MarkFailed(context.Background(), oid, text); err != nil {
		t.Fatal(err)
	}
}

func sweepRuntime(t *testing.T, platform string, now *float64, profile *string) []ClaimedObligation {
	t.Helper()
	claimed, err := SweepFailedForRuntime(context.Background(), platform, now, profile)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func sweepBoot(t *testing.T, now *float64, platforms map[string]bool, targets map[LedgerTarget]bool) []ClaimedObligation {
	t.Helper()
	claimed, err := SweepRecoverable(context.Background(), now, platforms, targets)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func pendingRetries(t *testing.T, now float64) []PendingRetry {
	t.Helper()
	out, err := PendingRetries(context.Background(), &now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func claimedIDs(claimed []ClaimedObligation) []string {
	ids := []string{}
	for _, c := range claimed {
		ids = append(ids, c.ObligationID)
	}
	return ids
}

func TestLedgerSchemaMigrationAddsAdapterProfileToExistingLedger(t *testing.T) {
	db := freshLedger(t)
	ctx := context.Background()
	conn, err := state.ConnectSQLite(db, state.SQLiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE delivery_obligations (obligation_id TEXT PRIMARY KEY,
		session_key TEXT NOT NULL, platform TEXT NOT NULL, chat_id TEXT NOT NULL, thread_id TEXT, content TEXT NOT NULL,
		state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, created_at REAL NOT NULL, updated_at REAL NOT NULL,
		owner_pid INTEGER, owner_started_at INTEGER, last_error TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := initializeLedgerSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('delivery_obligations') WHERE name='adapter_profile'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("adapter_profile column count %d err %v", n, err)
	}
}

func TestLedgerRecordStartsPending(t *testing.T) {
	freshLedger(t)
	ledgerRecord(t, ledgerRecordOpts{})
	if got := ledgerRowOf(t, "ob-1").State; got != "pending" {
		t.Fatalf("state %q", got)
	}
}

func TestLedgerObligationIDStableAndDistinct(t *testing.T) {
	a := ComputeObligationID("sk1", "msg1", "hello")
	if a != ComputeObligationID("sk1", "msg1", "hello") || len(a) != 24 {
		t.Fatalf("unstable id %q", a)
	}
	for _, other := range []string{ComputeObligationID("sk1:threadB", "msg1", "hello"),
		ComputeObligationID("sk1", "msg2", "hello"), ComputeObligationID("sk1", "msg1", "other")} {
		if other == a {
			t.Fatal("collision")
		}
	}
}

func TestLedgerSweepLiveOwnerRowsNeverClaimed(t *testing.T) {
	freshLedger(t)
	ledgerRecord(t, ledgerRecordOpts{})
	if got := sweepBoot(t, nil, nil, nil); len(got) != 0 {
		t.Fatalf("claimed %v", got)
	}
}

func TestLedgerSweepDeadOwnerPendingClaimedWithoutMarker(t *testing.T) {
	freshLedger(t)
	ledgerRecord(t, ledgerRecordOpts{})
	ledgerOrphan(t, "ob-1")
	claimed := sweepBoot(t, nil, nil, nil)
	if len(claimed) != 1 || claimed[0].NeedsMarker || claimed[0].Attempts != 1 {
		t.Fatalf("claimed %+v", claimed)
	}
	if again := sweepBoot(t, nil, nil, nil); len(again) != 0 {
		t.Fatalf("double claim %v", again)
	}
}

func TestLedgerRuntimeSweep(t *testing.T) {
	telegram := ledgerRecordOpts{platform: "telegram"}
	cases := map[string]func(t *testing.T){
		"claims_current_process_send_path_degraded_row": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			claimed := sweepRuntime(t, "telegram", nil, nil)
			if len(claimed) != 1 || !claimed[0].NeedsMarker || claimed[0].Attempts != 1 || ledgerRowOf(t, "ob-1").State != "attempting" {
				t.Fatalf("claimed %+v", claimed)
			}
		},
		"permanent_failure_is_not_claimed": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "Forbidden: bot was blocked by the user")
			if got := sweepRuntime(t, "telegram", f64(ledgerNow()+10_000), nil); len(got) != 0 {
				t.Fatalf("claimed %v", got)
			}
			if r := ledgerRowOf(t, "ob-1"); r.State != "failed" || r.Attempts != 0 {
				t.Fatalf("row %+v", r)
			}
		},
		"other_rejection_is_claimed_only_after_its_backoff": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "503 Service Unavailable")
			ledgerExec(t, "UPDATE delivery_obligations SET attempts=1, updated_at=1000.0 WHERE obligation_id='ob-1'")
			if got := sweepRuntime(t, "telegram", f64(1060), nil); len(got) != 0 {
				t.Fatalf("claimed early %v", got)
			}
			want := []PendingRetry{{Platform: "telegram", Profile: "default", NotBefore: 1120}}
			if got := pendingRetries(t, 1060); !reflect.DeepEqual(got, want) {
				t.Fatalf("pending %+v", got)
			}
			claimed := sweepRuntime(t, "telegram", f64(1121), nil)
			if !reflect.DeepEqual(claimedIDs(claimed), []string{"ob-1"}) || !claimed[0].NeedsMarker || ledgerRowOf(t, "ob-1").State != "attempting" {
				t.Fatalf("claimed %+v", claimed)
			}
		},
		"other_rejection_keeps_the_last_attempt_for_a_restart": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "503 Service Unavailable")
			ledgerExec(t, "UPDATE delivery_obligations SET attempts=?, updated_at=1000.0 WHERE obligation_id='ob-1'", LedgerMaxAttempts-1)
			if got := sweepRuntime(t, "telegram", f64(11000), nil); len(got) != 0 {
				t.Fatalf("claimed %v", got)
			}
			if got := pendingRetries(t, 11000); len(got) != 0 {
				t.Fatalf("pending %v", got)
			}
			if r := ledgerRowOf(t, "ob-1"); r.State != "failed" || r.Attempts != LedgerMaxAttempts-1 {
				t.Fatalf("row %+v", r)
			}
		},
		"reconnect_only_row_is_never_timer_driven": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			if got := pendingRetries(t, ledgerNow()+10_000); len(got) != 0 {
				t.Fatalf("pending %v", got)
			}
			if got := claimedIDs(sweepRuntime(t, "telegram", nil, nil)); !reflect.DeepEqual(got, []string{"ob-1"}) {
				t.Fatalf("claimed %v", got)
			}
		},
		"claim_is_platform_scoped_and_not_reclaimed_while_attempting": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerRecord(t, ledgerRecordOpts{oid: "ob-2", sessionKey: "agent:main:slack:channel:C2", platform: "slack", chatID: "C2"})
			ledgerMarkFailed(t, "ob-2", "send_path_degraded")
			if got := claimedIDs(sweepRuntime(t, "telegram", nil, nil)); !reflect.DeepEqual(got, []string{"ob-1"}) {
				t.Fatalf("claimed %v", got)
			}
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 {
				t.Fatalf("reclaimed %v", got)
			}
			if r := ledgerRowOf(t, "ob-2"); r.State != "failed" || r.Attempts != 0 {
				t.Fatalf("row %+v", r)
			}
		},
		"other_live_owner_is_not_claimed_or_abandoned": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerExec(t, "UPDATE delivery_obligations SET owner_pid=?, owner_started_at=?, attempts=? WHERE obligation_id=?",
				12345, 101, LedgerMaxAttempts, "ob-1")
			started := int64(202)
			patch(t, &ledgerOwnerStamp, func() (int64, *int64) { return 54321, &started })
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 {
				t.Fatalf("claimed %v", got)
			}
			if r := ledgerRowOf(t, "ob-1"); r.State != "failed" || r.Attempts != LedgerMaxAttempts {
				t.Fatalf("row %+v", r)
			}
		},
		"unowned_row_is_not_claimed": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerExec(t, "UPDATE delivery_obligations SET owner_pid=NULL, owner_started_at=NULL WHERE obligation_id=?", "ob-1")
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 || ledgerRowOf(t, "ob-1").State != "failed" {
				t.Fatalf("claimed %v", got)
			}
		},
		"missing_current_process_start_stamp_fails_closed": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerExec(t, "UPDATE delivery_obligations SET owner_started_at=NULL WHERE obligation_id=?", "ob-1")
			patch(t, &ledgerOwnerStamp, func() (int64, *int64) { return int64(os.Getpid()), nil })
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 || ledgerRowOf(t, "ob-1").State != "failed" {
				t.Fatalf("claimed %v", got)
			}
		},
		"same_pid_with_different_start_stamp_is_not_claimed": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerExec(t, "UPDATE delivery_obligations SET owner_pid=?, owner_started_at=? WHERE obligation_id=?", os.Getpid(), 101, "ob-1")
			started := int64(202)
			patch(t, &ledgerOwnerStamp, func() (int64, *int64) { return int64(os.Getpid()), &started })
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 || ledgerRowOf(t, "ob-1").State != "failed" {
				t.Fatalf("claimed %v", got)
			}
		},
		"profile_scope_never_claims_another_bot_identity": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			reviewer := "reviewer"
			ledgerRecord(t, ledgerRecordOpts{oid: "ob-2", sessionKey: "agent:reviewer:telegram:dm:C2", platform: "telegram", chatID: "C2", profile: &reviewer})
			ledgerMarkFailed(t, "ob-2", "send_path_degraded")
			claimed := sweepRuntime(t, "telegram", nil, &reviewer)
			if !reflect.DeepEqual(claimedIDs(claimed), []string{"ob-2"}) || claimed[0].Profile != "reviewer" || ledgerRowOf(t, "ob-1").State != "failed" {
				t.Fatalf("claimed %+v", claimed)
			}
		},
		"current_owner_row_at_attempt_cap_is_abandoned": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerExec(t, "UPDATE delivery_obligations SET attempts=? WHERE obligation_id=?", LedgerMaxAttempts, "ob-1")
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 || ledgerRowOf(t, "ob-1").State != "abandoned" {
				t.Fatalf("claimed %v", got)
			}
		},
		"delivered_row_is_never_reclaimed_by_reconnect_sweep": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 1 {
				t.Fatalf("first claim %v", got)
			}
			if err := MarkDelivered(context.Background(), "ob-1"); err != nil {
				t.Fatal(err)
			}
			ledgerExec(t, "UPDATE delivery_obligations SET last_error=? WHERE obligation_id=?", "send_path_degraded", "ob-1")
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 {
				t.Fatalf("reclaimed %v", got)
			}
			if r := ledgerRowOf(t, "ob-1"); r == nil || r.State != "delivered" || r.Attempts != 1 {
				t.Fatalf("row %+v", r)
			}
		},
		"current_owner_stale_row_is_abandoned": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			now := ledgerNow()
			ledgerExec(t, "UPDATE delivery_obligations SET created_at=? WHERE obligation_id=?", now-LedgerStaleAfterSeconds-1, "ob-1")
			if got := sweepRuntime(t, "telegram", &now, nil); len(got) != 0 || ledgerRowOf(t, "ob-1").State != "abandoned" {
				t.Fatalf("claimed %v", got)
			}
		},
		"boot_claimed_row_is_not_reclaimed_by_runtime_sweep_mid_send": func(t *testing.T) {
			ledgerRecord(t, telegram)
			ledgerMarkFailed(t, "ob-1", "send_path_degraded")
			ledgerOrphan(t, "ob-1")
			if got := claimedIDs(sweepBoot(t, nil, nil, nil)); !reflect.DeepEqual(got, []string{"ob-1"}) {
				t.Fatalf("boot claimed %v", got)
			}
			if got := sweepRuntime(t, "telegram", nil, nil); len(got) != 0 {
				t.Fatalf("runtime reclaimed %v", got)
			}
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			freshLedger(t)
			fn(t)
		})
	}
}

func TestLedgerPruneOldDeliveredRows(t *testing.T) {
	freshLedger(t)
	ctx := context.Background()
	ledgerRecord(t, ledgerRecordOpts{})
	if err := MarkDelivered(ctx, "ob-1"); err != nil {
		t.Fatal(err)
	}
	ledgerExec(t, "UPDATE delivery_obligations SET updated_at=? WHERE obligation_id=?", ledgerNow()-ledgerRetentionSeconds-60, "ob-1")
	if err := ledgerTransaction(ctx, func(conn state.SQLiteConn) error { return pruneLedgerUnlocked(ctx, conn, ledgerNow()) }); err != nil {
		t.Fatal(err)
	}
	if r := ledgerRowOf(t, "ob-1"); r != nil {
		t.Fatalf("row survived %+v", r)
	}
}

func TestLedgerAbsentPlatformDoesNotBurnAttempts(t *testing.T) {
	freshLedger(t)
	ledgerRecord(t, ledgerRecordOpts{platform: "telegram"})
	if err := MarkAttempting(context.Background(), "ob-1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < LedgerMaxAttempts+2; i++ {
		ledgerOrphan(t, "ob-1")
		if got := sweepBoot(t, nil, map[string]bool{"discord": true}, nil); len(got) != 0 {
			t.Fatalf("claimed %v", got)
		}
	}
	if r := ledgerRowOf(t, "ob-1"); r.Attempts != 0 || r.State != "attempting" {
		t.Fatalf("an unsendable boot must not spend the budget %+v", r)
	}
}

func TestLedgerRowStillDeliversOnceItsPlatformReturns(t *testing.T) {
	freshLedger(t)
	ledgerRecord(t, ledgerRecordOpts{platform: "telegram"})
	for i := 0; i < LedgerMaxAttempts+2; i++ {
		ledgerOrphan(t, "ob-1")
		sweepBoot(t, nil, map[string]bool{"discord": true}, nil)
	}
	ledgerOrphan(t, "ob-1")
	claimed := sweepBoot(t, nil, map[string]bool{"telegram": true}, nil)
	if len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("claimed %+v", claimed)
	}
}

func TestLedgerOwnerAlivePidProbe(t *testing.T) {
	cases := []struct {
		name  string
		probe func(int) bool
		want  bool
	}{
		{"alive_when_pid_exists", func(int) bool { return true }, true},
		{"dead_when_pid_gone", func(int) bool { return false }, false},
		{"probe_exception_means_dead", func(int) bool { panic("probe blew up") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch(t, &statusProcessStartTime, fixedInt(nil))
			patch(t, &statusPidExists, tc.probe)
			if got := ledgerOwnerAlive(int64(12345), int64(999)); got != tc.want {
				t.Fatalf("alive %v", got)
			}
		})
	}
	t.Run("raw_os_kill_probe_never_used", func(t *testing.T) {
		patch(t, &statusProcessStartTime, fixedInt(nil))
		var calls []int
		patch(t, &statusPidExists, func(pid int) bool { calls = append(calls, pid); return true })
		if !ledgerOwnerAlive(int64(4242), int64(999)) || !reflect.DeepEqual(calls, []int{4242}) {
			t.Fatalf("probe calls %v", calls)
		}
	})
}

func TestLedgerOwnerLivenessToleratesFingerprintDrift(t *testing.T) {
	const recorded = int64(178864182760)
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(recorded+100))
	if !ledgerOwnerAlive(int64(4242), recorded) {
		t.Fatal("one second drift reconciled a live owner to dead")
	}
	patch(t, &statusProcessStartTime, fixedInt(recorded+3600))
	if ledgerOwnerAlive(int64(4242), recorded) {
		t.Fatal("far fingerprint kept a recycled pid alive")
	}
}

func floodRecord(t *testing.T, oid string, profile *string) {
	t.Helper()
	thread := "77"
	if err := RecordObligation(context.Background(), DeliveryObligation{ObligationID: oid, SessionKey: "session-" + oid,
		Platform: "telegram", ChatID: "123", ThreadID: &thread, Content: strings.Repeat(".", 3000), AdapterProfile: profile}); err != nil {
		t.Fatal(err)
	}
	ledgerMarkFailed(t, oid, "flood_control:185")
}

func TestLedgerRuntimeDeadlinePreservesScopeAndRetryBudget(t *testing.T) {
	freshLedger(t)
	other := "other"
	floodRecord(t, "due", nil)
	floodRecord(t, "other", &other)
	floodRecord(t, "blocked", nil)
	ledgerMarkFailed(t, "blocked", "Forbidden: bot was blocked by the user")
	stamp := ledgerRowOf(t, "due").UpdatedAt
	if got := sweepRuntime(t, "telegram", f64(stamp+184), nil); len(got) != 0 || ledgerRowOf(t, "due").Attempts != 0 {
		t.Fatalf("claimed inside the wait %v", got)
	}
	claimed := sweepRuntime(t, "telegram", f64(stamp+186), nil)
	if !reflect.DeepEqual(claimedIDs(claimed), []string{"due"}) || !claimed[0].NeedsMarker || !strings.Contains(claimed[0].Marker, "rate limit") {
		t.Fatalf("claimed %+v", claimed)
	}
	if ledgerRowOf(t, "due").LastError != nil {
		t.Fatal("claim kept the stale refusal")
	}
	if got := sweepRuntime(t, "telegram", f64(stamp+187), nil); len(got) != 0 {
		t.Fatalf("reclaimed %v", got)
	}
	ctx := context.Background()
	if ok, err := ReleaseRuntimeClaim(ctx, "due", claimed[0].LastError); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	if ledgerRowOf(t, "due").Attempts != 0 {
		t.Fatal("release spent an attempt")
	}
	if got := sweepRuntime(t, "telegram", f64(ledgerNow()), nil); len(got) != 0 {
		t.Fatalf("claimed before the renewed wait %v", got)
	}
	if got := sweepRuntime(t, "telegram", f64(ledgerNow()+186), nil); len(got) != 1 {
		t.Fatalf("claimed %v", got)
	}
	if err := MarkDelivered(ctx, "due"); err != nil {
		t.Fatal(err)
	}
	if got := sweepRuntime(t, "telegram", f64(ledgerNow()+187), nil); len(got) != 0 {
		t.Fatalf("delivered row reclaimed %v", got)
	}
	if ledgerRowOf(t, "other").Attempts != 0 || ledgerRowOf(t, "blocked").Attempts != 0 {
		t.Fatal("another scope spent an attempt")
	}
}

func TestLedgerBootAdoptsWaitingRowsWithoutSpendingOrLosingDeadline(t *testing.T) {
	freshLedger(t)
	floodRecord(t, "boot", nil)
	original := ledgerRowOf(t, "boot")
	ledgerExec(t, "UPDATE delivery_obligations SET owner_pid=NULL, owner_started_at=NULL, adapter_profile=NULL")
	claimed := sweepBoot(t, f64(original.UpdatedAt+20), nil, map[LedgerTarget]bool{{Platform: "telegram", NullProfile: true}: true})
	if len(claimed) != 1 || !claimed[0].Adopted {
		t.Fatalf("claimed %+v", claimed)
	}
	adopted := ledgerRowOf(t, "boot")
	if adopted.OwnerPid != int64(os.Getpid()) || adopted.AdapterProfile == nil || *adopted.AdapterProfile != "default" ||
		adopted.Attempts != 0 || adopted.UpdatedAt != original.UpdatedAt {
		t.Fatalf("adopted %+v", adopted)
	}
	if got := sweepBoot(t, f64(original.UpdatedAt+30), nil, nil); len(got) != 0 {
		t.Fatalf("re-adopted %v", got)
	}
	if got := sweepRuntime(t, "telegram", f64(original.UpdatedAt+184), nil); len(got) != 0 {
		t.Fatalf("claimed inside the wait %v", got)
	}
	claimed = sweepRuntime(t, "telegram", f64(original.UpdatedAt+186), nil)
	if len(claimed) != 1 || !claimed[0].NeedsMarker {
		t.Fatalf("claimed %+v", claimed)
	}
	if r := ledgerRowOf(t, "boot"); r.State != "attempting" || r.LastError != nil {
		t.Fatalf("row %+v", r)
	}
}

type closeCountingConn struct {
	state.SQLiteConn
	closed *[]state.SQLiteConn
}

func (c *closeCountingConn) Close() error {
	*c.closed = append(*c.closed, c.SQLiteConn)
	return c.SQLiteConn.Close()
}

func (c *closeCountingConn) Raw(f func(driverConn any) error) error {
	return c.SQLiteConn.(interface {
		Raw(f func(driverConn any) error) error
	}).Raw(f)
}

func countLedgerConnections(t *testing.T, db string) (opened, closed *[]state.SQLiteConn, labels *[]string) {
	t.Helper()
	opened, closed, labels = &[]state.SQLiteConn{}, &[]state.SQLiteConn{}, &[]string{}
	real := state.OpenDB
	patch(t, &state.OpenDB, func(ctx context.Context, path, dbLabel string, opts ...state.OpenDBOption) (state.SQLiteConn, error) {
		conn, err := real(ctx, path, dbLabel, opts...)
		if err != nil || path != db {
			return conn, err
		}
		*opened = append(*opened, conn)
		*labels = append(*labels, dbLabel)
		return &closeCountingConn{SQLiteConn: conn, closed: closed}, nil
	})
	return opened, closed, labels
}

func TestLedgerOperationsCloseEveryConnection(t *testing.T) {
	db := freshLedger(t)
	opened, closed, _ := countLedgerConnections(t, db)
	ctx := context.Background()
	oid := ComputeObligationID("sess", "msg", "content")
	if err := RecordObligation(ctx, DeliveryObligation{ObligationID: oid, SessionKey: "sess", Platform: "telegram", ChatID: "123", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := MarkAttempting(ctx, oid); err != nil {
		t.Fatal(err)
	}
	if err := MarkDelivered(ctx, oid); err != nil {
		t.Fatal(err)
	}
	sweepBoot(t, nil, nil, nil)
	if len(*opened) == 0 || !reflect.DeepEqual(*opened, *closed) {
		t.Fatalf("opened %d closed %d", len(*opened), len(*closed))
	}
}

func TestLedgerRecordingAReplyDoesNotOpenASecondConnection(t *testing.T) {
	db := freshLedger(t)
	ctx := context.Background()
	if err := RecordObligation(ctx, DeliveryObligation{ObligationID: "ob-warm", SessionKey: "s", Platform: "p", ChatID: "c", Content: "warm-up (schema)"}); err != nil {
		t.Fatal(err)
	}
	opened, _, labels := countLedgerConnections(t, db)
	if err := RecordObligation(ctx, DeliveryObligation{ObligationID: "ob-1", SessionKey: "s", Platform: "p", ChatID: "c", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || (*labels)[0] != ledgerDBLabel {
		t.Fatalf("opened %d labels %v", len(*opened), *labels)
	}
}

func TestLedgerOpensThroughTheCanonicalOpenDB(t *testing.T) {
	db := freshLedger(t)
	_, _, labels := countLedgerConnections(t, db)
	ctx := context.Background()
	conn, err := ledgerConnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var busy int
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil || busy <= 0 {
		t.Fatalf("busy timeout %d %v", busy, err)
	}
	if len(*labels) != 1 || (*labels)[0] == "" {
		t.Fatalf("labels %v", *labels)
	}
}

func TestLedgerFloodClassifiers(t *testing.T) {
	if !IsFloodError("flood_control:185") || !IsFloodError("Flood control exceeded. Retry in 185 seconds") || IsFloodError("please retry in 5 seconds") || IsFloodError(nil) {
		t.Fatal("flood classification")
	}
	if FloodWaitSeconds("flood_control:185", FloodRetryDefaultSeconds) != 185 || FloodWaitSeconds("Flood control exceeded. Retry in 12.5 seconds", 60) != 12.5 ||
		FloodWaitSeconds("flood_control:junk", 60) != 60 || FloodWaitSeconds("flood_control:-4", 60) != 60 {
		t.Fatal("flood wait")
	}
	if FloodRetryDelay(5000.0) != FloodRetryCapSeconds+FloodRetrySlackSeconds || FloodRetryDelay("junk") != FloodRetryDefaultSeconds+FloodRetrySlackSeconds || FloodRetryDelay(-3) != FloodRetrySlackSeconds {
		t.Fatal("flood delay")
	}
	if !IsReconnectOnly(" Send_Path_Degraded ") || IsReconnectOnly(nil) {
		t.Fatal("reconnect only")
	}
	if due, ok := RetryNotBefore(1000.0, "503", 0); !ok || due != 1030 {
		t.Fatalf("backoff %v %v", due, ok)
	}
}

func TestLedgerEnabledGate(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		cfg  map[string]any
		want bool
	}{
		{map[string]any{}, true},
		{map[string]any{"gateway": nil}, true},
		{map[string]any{"gateway": "junk"}, true},
		{map[string]any{"gateway": map[string]any{"delivery_ledger": false}}, false},
		{map[string]any{"gateway": map[string]any{"delivery_ledger": " Off "}}, false},
		{map[string]any{"gateway": map[string]any{"delivery_ledger": "yes"}}, true},
		{map[string]any{"gateway": map[string]any{"delivery_ledger": 0}}, false},
	}
	for _, tc := range cases {
		if got := LedgerEnabled(ctx, tc.cfg); got != tc.want {
			t.Fatalf("%v gives %v", tc.cfg, got)
		}
	}
}

func TestLedgerCrashLeftReplyAdoptsOnceUnlessAlreadyLedgered(t *testing.T) {
	freshLedger(t)
	ctx := context.Background()
	start := ledgerNow() - 5
	ob := DeliveryObligation{ObligationID: "crash-1", SessionKey: "sk", Platform: "telegram", ChatID: "1", Content: "left"}
	if err := RecordCrashLeftReply(ctx, ob, start); err != nil {
		t.Fatal(err)
	}
	r := ledgerRowOf(t, "crash-1")
	if r == nil || r.State != "attempting" || r.OwnerPid != nil || *r.AdapterProfile != "default" {
		t.Fatalf("row %+v", r)
	}
	ob.ObligationID = "crash-2"
	if err := RecordCrashLeftReply(ctx, ob, start); err != nil {
		t.Fatal(err)
	}
	if ledgerRowOf(t, "crash-2") != nil {
		t.Fatal("already ledgered reply adopted twice")
	}
}
