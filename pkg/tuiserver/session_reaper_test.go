package tuiserver

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const reaperIdleS = 6 * 3600

// Fresh session store under a temp dir
func reaperTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// Swaps a package variable for the case and restores it after
func swapVar[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// Live session registry holding exactly the given records for the case
func swapSessions(t *testing.T, records map[string]*tuigateway.Session) {
	t.Helper()
	for _, live := range tuigateway.LiveSessions() {
		saved := live
		tuigateway.SetSessionRecord(saved.SID, nil)
		t.Cleanup(func() { tuigateway.SetSessionRecord(saved.SID, saved.Session) })
	}
	for sid, record := range records {
		tuigateway.SetSessionRecord(sid, record)
		t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	}
}

func reaperExec(t *testing.T, db *state.DB, query string, args ...any) {
	t.Helper()
	err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(context.Background(), query, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Row with one user message, its start and message clock set to the given times
func seedReaperSession(t *testing.T, db *state.DB, sid, source string, lastActive float64, startedAt *float64) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: source}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	started := lastActive
	if startedAt != nil {
		started = *startedAt
	}
	reaperExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", started, sid)
	reaperExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", lastActive, sid)
}

func reaperRow(t *testing.T, db *state.DB, sid string) map[string]any {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil || row == nil {
		t.Fatalf("row %s: %v", sid, err)
	}
	out := map[string]any{}
	for _, key := range row.Keys {
		out[key] = row.Vals[key]
	}
	return out
}

// Sweep scope of the TestSweepOrphanedSessionRows cases over the given store
func sweepScope(t *testing.T, db *state.DB, ttl float64, records map[string]*tuigateway.Session) {
	t.Helper()
	swapVar(t, &reaperDB, func(context.Context) *state.DB { return db })
	swapVar(t, &sessionTTLS, ttl)
	swapSessions(t, records)
}

func TestEndsStaleTuiDesktopAndSubagent(t *testing.T) {
	db := reaperTestDB(t)
	stale := nowSeconds() - 8*3600
	for _, row := range [][2]string{{"stale-tui", "tui"}, {"stale-desktop", "desktop"}, {"stale-sub", "subagent"}} {
		seedReaperSession(t, db, row[0], row[1], stale, nil)
	}
	sweepScope(t, db, reaperIdleS, nil)
	swept := sweepOrphanedSessionRows(context.Background())
	sort.Strings(swept)
	if !reflect.DeepEqual(swept, []string{"stale-desktop", "stale-sub", "stale-tui"}) {
		t.Fatalf("swept %v", swept)
	}
	for _, sid := range []string{"stale-tui", "stale-desktop", "stale-sub"} {
		row := reaperRow(t, db, sid)
		if row["ended_at"] == nil || row["end_reason"] != "startup_orphan_reap" {
			t.Fatalf("%s not ended: %v", sid, row)
		}
	}
}

func TestSparesFreshRowWithOldCopiedHistory(t *testing.T) {
	db := reaperTestDB(t)
	started := nowSeconds()
	seedReaperSession(t, db, "fresh-branch", "tui", nowSeconds()-8*3600, &started)
	sweepScope(t, db, reaperIdleS, nil)
	if swept := sweepOrphanedSessionRows(context.Background()); len(swept) != 0 {
		t.Fatalf("swept %v", swept)
	}
	if reaperRow(t, db, "fresh-branch")["ended_at"] != nil {
		t.Fatal("fresh branch ended")
	}
}

func TestSparesPinnedIdleRow(t *testing.T) {
	db := reaperTestDB(t)
	stale := nowSeconds() - 8*3600
	seedReaperSession(t, db, "pinned-tui", "tui", stale, nil)
	seedReaperSession(t, db, "stale-tui", "tui", stale, nil)
	if _, err := db.SetSessionPinned(context.Background(), "pinned-tui", true); err != nil {
		t.Fatal(err)
	}
	sweepScope(t, db, reaperIdleS, nil)
	if swept := sweepOrphanedSessionRows(context.Background()); !reflect.DeepEqual(swept, []string{"stale-tui"}) {
		t.Fatalf("swept %v", swept)
	}
	if reaperRow(t, db, "pinned-tui")["ended_at"] != nil || reaperRow(t, db, "stale-tui")["end_reason"] != "startup_orphan_reap" {
		t.Fatal("pinned row swept or stale row kept")
	}
}

func TestSparesLiveInMemoryAndGatewayRows(t *testing.T) {
	db := reaperTestDB(t)
	stale := nowSeconds() - 8*3600
	seedReaperSession(t, db, "resumed-tui", "tui", stale, nil)
	seedReaperSession(t, db, "gateway-row", "telegram", stale, nil)
	seedReaperSession(t, db, "recent-tui", "tui", nowSeconds()-30, nil)
	resumedAgent := &agent.AIAgent{}
	resumedAgent.SetSessionID("resumed-tui")
	sweepScope(t, db, reaperIdleS, map[string]*tuigateway.Session{
		"mem-sid": tuigateway.NewSession(map[string]any{"agent": resumedAgent, "session_key": "resumed-tui"}),
	})
	if swept := sweepOrphanedSessionRows(context.Background()); len(swept) != 0 {
		t.Fatalf("swept %v", swept)
	}
	for _, sid := range []string{"resumed-tui", "gateway-row", "recent-tui"} {
		if reaperRow(t, db, sid)["ended_at"] != nil {
			t.Fatalf("%s ended", sid)
		}
	}
}

func TestLeavesAlreadyEndedRowsUntouched(t *testing.T) {
	db := reaperTestDB(t)
	seedReaperSession(t, db, "reaped-tui", "tui", nowSeconds()-8*3600, nil)
	if err := db.EndSession("reaped-tui", "ws_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	before := reaperRow(t, db, "reaped-tui")
	sweepScope(t, db, reaperIdleS, nil)
	if swept := sweepOrphanedSessionRows(context.Background()); len(swept) != 0 {
		t.Fatalf("swept %v", swept)
	}
	after := reaperRow(t, db, "reaped-tui")
	if after["end_reason"] != "ws_orphan_reap" || after["ended_at"] != before["ended_at"] {
		t.Fatalf("row rewritten %v", after)
	}
}

func TestZeroTTLSkipsSweep(t *testing.T) {
	db := reaperTestDB(t)
	seedReaperSession(t, db, "stale-tui", "tui", nowSeconds()-8*3600, nil)
	sweepScope(t, db, 0, nil)
	if swept := sweepOrphanedSessionRows(context.Background()); len(swept) != 0 {
		t.Fatalf("swept %v", swept)
	}
	if reaperRow(t, db, "stale-tui")["ended_at"] != nil {
		t.Fatal("row ended")
	}
}

func TestOncePerProcessAndConfigAndTTLGates(t *testing.T) {
	started := 0
	swapVar(t, &reaperAfterFunc, func(time.Duration, func()) *time.Timer { started++; return nil })
	swapVar(t, &wsOrphanReapGraceS, func() float64 { return 20 })
	swapVar(t, &sessionTTLS, float64(reaperIdleS))
	swapVar(t, &startupOrphanSweepRan, false)
	swapVar(t, &sessionOrphanReaperEnabled, func(context.Context) bool { return false })
	ctx := context.Background()
	scheduleStartupOrphanSweep(ctx)
	if started != 0 || startupOrphanSweepRan {
		t.Fatal("disabled sweep scheduled")
	}
	sessionOrphanReaperEnabled = func(context.Context) bool { return true }
	scheduleStartupOrphanSweep(ctx)
	scheduleStartupOrphanSweep(ctx)
	if started != 1 || !startupOrphanSweepRan {
		t.Fatalf("scheduled %d", started)
	}
	startupOrphanSweepRan = false
	wsOrphanReapGraceS = func() float64 { return 0 }
	scheduleStartupOrphanSweep(ctx)
	if started != 1 {
		t.Fatal("zero grace scheduled")
	}
	wsOrphanReapGraceS = func() float64 { return 20 }
	sessionTTLS = 0
	startupOrphanSweepRan = false
	scheduleStartupOrphanSweep(ctx)
	if started != 1 {
		t.Fatal("zero ttl scheduled")
	}
}

func TestConfigFlagReadsDashboardStartupOrphanSweep(t *testing.T) {
	ctx := context.Background()
	swapVar(t, &reaperLoadCfg, func(context.Context) map[string]any {
		return map[string]any{"dashboard": map[string]any{"startup_orphan_sweep": false}}
	})
	if orphanReaperEnabled(ctx) {
		t.Fatal("flag false read as enabled")
	}
	reaperLoadCfg = func(context.Context) map[string]any { return map[string]any{} }
	if !orphanReaperEnabled(ctx) {
		t.Fatal("missing flag read as disabled")
	}
	reaperLoadCfg = func(context.Context) map[string]any { panic(errors.New("boom")) }
	if !orphanReaperEnabled(ctx) {
		t.Fatal("failing config read as disabled")
	}
}

// Heartbeat scope of the TestBackendHeartbeatRefresher cases over the given store
func heartbeatScope(t *testing.T, db *state.DB) {
	t.Helper()
	swapVar(t, &reaperDB, func(context.Context) *state.DB { return db })
	swapVar(t, &heartbeatRefreshS, 0)
	swapVar(t, &heartbeatRefresherStarted, false)
	swapVar(t, &backendIDForThisProcess, func(context.Context) string { return "test-backend-A" })
}

func TestFirstCallRegistersRowInDB(t *testing.T) {
	db := reaperTestDB(t)
	heartbeatScope(t, db)
	startBackendHeartbeatRefresher(context.Background())
	rows, err := db.ListBackendHeartbeats(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	if rows[0].GetOr("backend_id", nil) != "test-backend-A" {
		t.Fatalf("backend id %v", rows[0].GetOr("backend_id", nil))
	}
	if beat, _ := rows[0].GetOr("last_heartbeat", nil).(float64); beat <= 0 {
		t.Fatalf("last heartbeat %v", rows[0].GetOr("last_heartbeat", nil))
	}
}

func TestRepeatCallsAreNoops(t *testing.T) {
	db := reaperTestDB(t)
	heartbeatScope(t, db)
	for range 3 {
		startBackendHeartbeatRefresher(context.Background())
	}
	rows, err := db.ListBackendHeartbeats(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
}

func TestRefresherFailureDoesNotRaise(t *testing.T) {
	swapVar(t, &refreshBackendHeartbeat, func(context.Context) { panic(errors.New("disk full")) })
	swapVar(t, &heartbeatRefreshS, 0)
	swapVar(t, &heartbeatRefresherStarted, false)
	startBackendHeartbeatRefresher(context.Background())
}
