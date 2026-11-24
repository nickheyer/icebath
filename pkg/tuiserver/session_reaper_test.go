package tuiserver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

const reaperIdleS = 6 * 3600

// Fresh session store under a temp dir, closed when the case ends before the dir is removed
func reaperTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Swaps a package variable for the case and restores it after
func swapVar[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
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

func TestConfigFlagReadsSessionsStartupOrphanSweep(t *testing.T) {
	if orphanReaperEnabled(turnLimitHome(t, "sessions:\n  startup_orphan_sweep: false\n")) {
		t.Fatal("flag false read as enabled")
	}
	if !orphanReaperEnabled(turnLimitHome(t, "sessions: {}\n")) {
		t.Fatal("missing flag read as disabled")
	}
}

// Heartbeat scope of the TestBackendHeartbeatRefresher cases over the given store
func heartbeatScope(t *testing.T, db *state.DB) {
	t.Helper()
	swapVar(t, &reaperDB, func(context.Context) (*state.DB, error) { return db, nil })
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
