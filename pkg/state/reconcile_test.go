package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Installs no-op gateway ownership seams, the shape of a host with no gateway holding any home
func installOwnershipDoubles(t *testing.T) {
	t.Helper()
	prevExclusive, prevConflict, prevLive := ExclusiveMaintenance, IsOwnershipConflict, LiveRuntimeOwner
	prevCanonical, prevLabel, prevRecord := CanonicalHome, ProfileLabelForHome, ReadGatewayLockRecord
	ExclusiveMaintenance = func(ctx context.Context, homes []string, purpose string) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}
	IsOwnershipConflict = func(error) bool { return false }
	LiveRuntimeOwner = func(string) (int, bool, error) { return 0, false, nil }
	CanonicalHome = func(h string) (string, error) { return filepath.Clean(h), nil }
	ProfileLabelForHome = func(string) string { return "default" }
	ReadGatewayLockRecord = func(string) *pyjson.Object { return nil }
	t.Cleanup(func() {
		ExclusiveMaintenance, IsOwnershipConflict, LiveRuntimeOwner = prevExclusive, prevConflict, prevLive
		CanonicalHome, ProfileLabelForHome, ReadGatewayLockRecord = prevCanonical, prevLabel, prevRecord
	})
}

// Store with history whose runtime ledger tables and columns were removed again, the shape an older release kept
func legacyStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	readSession(t, db, NewSession{ID: "retained", Source: "cli"})
	readAppend(t, db, "retained", map[string]any{"role": "user", "content": "history survives"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn := openRaw(t, path)
	execAll(t, conn, "ALTER TABLE sessions DROP COLUMN runtime_generation", "ALTER TABLE sessions DROP COLUMN runtime_revision",
		"ALTER TABLE async_delegations DROP COLUMN owner_execution_id")
	for _, table := range []string{"worker_receipts", "worker_executions", "session_admissions", "runtime_epoch"} {
		execAll(t, conn, "DROP TABLE "+table)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Bytes of a file, failing the test when unreadable
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMissingHistoryTableIsNotRecreatedAsEmpty(t *testing.T) {
	for _, missing := range []string{"sessions", "messages"} {
		t.Run(missing, func(t *testing.T) {
			installOwnershipDoubles(t)
			path := filepath.Join(t.TempDir(), "state.db")
			kept := "messages"
			if missing == "messages" {
				kept = "sessions"
			}
			conn := openRaw(t, path)
			execAll(t, conn, "CREATE TABLE "+kept+" (id TEXT)", "INSERT INTO "+kept+" VALUES ('retain-me')")
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			before := fileBytes(t, path)
			report := RepairDeclarativeSchema(context.Background(), path, true)
			if report.Repaired || !strings.Contains(report.Error, "canonical history tables are missing") {
				t.Fatalf("report %+v", report)
			}
			if !bytes.Equal(fileBytes(t, path), before) {
				t.Fatal("the store changed")
			}
		})
	}
}

func TestSnapshotHistoryLossIsNotPromoted(t *testing.T) {
	installOwnershipDoubles(t)
	ctx := context.Background()
	path := legacyStore(t)
	before := fileBytes(t, path)
	saved := reconcileCopySnapshot
	reconcileCopySnapshot = func(ctx context.Context, source, destination string, sourceConn, destinationConn SQLiteConn) error {
		if err := saved(ctx, source, destination, sourceConn, destinationConn); err != nil {
			return err
		}
		if sourceConn != nil {
			conn := openRaw(t, destination)
			execAll(t, conn, "DROP TABLE messages")
			return conn.Close()
		}
		return nil
	}
	t.Cleanup(func() { reconcileCopySnapshot = saved })
	report := RepairDeclarativeSchema(ctx, path, true)
	if report.Repaired || !strings.Contains(report.Error, "canonical history tables are missing") {
		t.Fatalf("report %+v", report)
	}
	if !bytes.Equal(fileBytes(t, path), before) {
		t.Fatal("the store changed")
	}
}
