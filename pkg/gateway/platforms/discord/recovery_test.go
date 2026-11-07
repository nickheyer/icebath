package discord

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/state"
)

func recoveryCount(t *testing.T, conn state.SQLiteConn, query string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecoveryLedgerPrunesExpiredRows(t *testing.T) {
	ctx := context.Background()
	store := NewDiscordRecoveryStore(ctx, t.TempDir())
	old := pydatetime.ISOFormatUTC(time.Now().UTC().Add(-31 * 24 * time.Hour))
	inserted := store.Call(ctx, func(conn state.SQLiteConn) (any, error) {
		if _, err := conn.ExecContext(ctx, "INSERT INTO discord_messages "+
			"(message_id, status, updated_at) VALUES ('old-message', 'responded', ?)", old); err != nil {
			return nil, err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO discord_recovery_scans "+
			"(scan_id, started_at, completed_at, status, channels, window_seconds, limit_count) "+
			"VALUES ('old-scan', ?, ?, 'success', '[]', 3600, 10)", old, old)
		return true, err
	}, false)
	if inserted != true {
		t.Fatalf("insert = %v", inserted)
	}
	store.initialized = false
	store.Call(ctx, func(state.SQLiteConn) (any, error) { return nil, nil }, nil)
	got := store.Call(ctx, func(conn state.SQLiteConn) (any, error) {
		return [2]int{
			recoveryCount(t, conn, "SELECT COUNT(*) FROM discord_messages WHERE message_id='old-message'"),
			recoveryCount(t, conn, "SELECT COUNT(*) FROM discord_recovery_scans WHERE scan_id='old-scan'"),
		}, nil
	}, nil)
	if got != [2]int{0, 0} {
		t.Fatalf("old rows = %v", got)
	}
}

func TestRecoveryLedgerKeepsFreshRowsAndRollsBackAFailedCall(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := NewDiscordRecoveryStore(ctx, root)
	fresh := pydatetime.ISOFormatUTC(time.Now().UTC().Add(-29 * 24 * time.Hour))
	store.Call(ctx, func(conn state.SQLiteConn) (any, error) {
		_, err := conn.ExecContext(ctx, "INSERT INTO discord_recovery_cursors VALUES ('123', '99', ?)", fresh)
		return nil, err
	}, nil)
	failed := store.Call(ctx, func(conn state.SQLiteConn) (any, error) {
		if _, err := conn.ExecContext(ctx, "INSERT INTO discord_recovery_cursors VALUES ('456', '1', ?)", fresh); err != nil {
			return nil, err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO missing_table VALUES (1)")
		return "unreachable", err
	}, "fallback")
	if failed != "fallback" {
		t.Fatalf("failed call = %v", failed)
	}
	store.initialized = false
	got := store.Call(ctx, func(conn state.SQLiteConn) (any, error) {
		return recoveryCount(t, conn, "SELECT COUNT(*) FROM discord_recovery_cursors"), nil
	}, nil)
	if got != 1 {
		t.Fatalf("cursor rows = %v, want the fresh cursor only", got)
	}
	path := filepath.Join(root, "gateway", "discord_message_recovery.db")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %o", info.Mode().Perm())
	}
}

func TestRecoveryLedgerReturnsDefaultWhenUnavailable(t *testing.T) {
	ctx := context.Background()
	blocker := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewDiscordRecoveryStore(ctx, blocker)
	called := false
	got := store.Call(ctx, func(state.SQLiteConn) (any, error) { called = true; return 1, nil }, "default")
	if got != "default" || called {
		t.Fatalf("got %v called %v", got, called)
	}
}

func TestRecoveryStoreDefaultsToTheHomeInEffect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	path, err := NewDiscordRecoveryStore(context.Background(), "").Path()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "gateway", "discord_message_recovery.db") {
		t.Fatalf("path = %q", path)
	}
}

func TestRecoveryStoreHonorsConfiguredDeleteJournalMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("database:\n  journal_mode: delete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got := NewDiscordRecoveryStore(ctx, dir).Call(ctx, func(conn state.SQLiteConn) (any, error) {
		var mode string
		err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
		return mode, err
	}, nil)
	if got != "delete" {
		t.Fatalf("journal mode = %v", got)
	}
}
