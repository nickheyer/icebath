package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSessionDBPathWithoutBackingStore(t *testing.T) {
	if got := (&SessionDB{}).Path(); got != "" {
		t.Fatalf("path %q", got)
	}
}

func TestReadCtxExportRunsOnAReadConnection(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	var one int
	if err := db.ReadCtx(ctx, func(conn SQLiteConn) error { return conn.QueryRowContext(ctx, "SELECT 1").Scan(&one) }); err != nil || one != 1 {
		t.Fatalf("read = %d, %v", one, err)
	}
}
