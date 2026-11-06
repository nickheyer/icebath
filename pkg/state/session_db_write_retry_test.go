package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Store with a two second write patience and millisecond jitter, closed at cleanup
func noMoreRowsDB(t *testing.T) *SessionDB {
	t.Helper()
	savedPatience, savedMin, savedMax := sessionWritePatienceS, sessionWriteRetryMinS, sessionWriteRetryMaxS
	sessionWritePatienceS, sessionWriteRetryMinS, sessionWriteRetryMaxS = 2.0, 0.001, 0.005
	t.Cleanup(func() {
		sessionWritePatienceS, sessionWriteRetryMinS, sessionWriteRetryMaxS = savedPatience, savedMin, savedMax
	})
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTransientInterfaceErrorIsRetriedToSuccess(t *testing.T) {
	db := noMoreRowsDB(t)
	ctx := context.Background()
	calls := 0
	err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		calls++
		if calls <= 3 {
			return &SQLiteError{Class: SQLiteInterfaceError, Msg: "no more rows available"}
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES ('nmr', 'ok') ON CONFLICT(key) DO UPDATE SET value=excluded.value")
		return err
	}, nil)
	if err != nil || calls != 4 {
		t.Fatalf("err %v calls %d", err, calls)
	}
	if v, ok, err := db.GetMeta(ctx, "nmr"); !ok || v != "ok" || err != nil {
		t.Fatalf("meta %q %v %v", v, ok, err)
	}
}

func TestUnrelatedInterfaceErrorPropagatesImmediately(t *testing.T) {
	db := noMoreRowsDB(t)
	calls := 0
	err := db.executeWrite(context.Background(), func(SQLiteConn) error {
		calls++
		return &SQLiteError{Class: SQLiteInterfaceError, Msg: "bad parameter or other API misuse"}
	}, nil)
	if class, ok := sqliteClassOf(err); !ok || class != SQLiteInterfaceError || !strings.Contains(err.Error(), "bad parameter") || calls != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

func TestNoMoreRowsViaDatabaseErrorIsRetried(t *testing.T) {
	db := noMoreRowsDB(t)
	calls := 0
	err := db.executeWrite(context.Background(), func(SQLiteConn) error {
		calls++
		if calls <= 2 {
			return &SQLiteError{Class: SQLiteDatabaseError, Msg: "no more rows available"}
		}
		return nil
	}, nil)
	if err != nil || calls != 3 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

func TestExhaustedPatiencePropagatesTheTransientError(t *testing.T) {
	db := noMoreRowsDB(t)
	sessionWritePatienceS = 0.05
	err := db.executeWrite(context.Background(), func(SQLiteConn) error {
		return &SQLiteError{Class: SQLiteInterfaceError, Msg: "no more rows available"}
	}, nil)
	if class, ok := sqliteClassOf(err); !ok || class != SQLiteInterfaceError || !strings.Contains(err.Error(), "no more rows") {
		t.Fatalf("err %v", err)
	}
}

func TestBlockedProbeOnDeleteFileDoesNotEnableReadPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	seedSession(t, db, "s")
	db.Close()
	ctx := context.Background()
	offline := openRaw(t, path)
	var mode string
	if err := offline.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil || strings.ToLower(mode) != "delete" {
		t.Fatalf("mode %q %v", mode, err)
	}
	offline.Close()
	sibling := openRaw(t, path)
	execAll(t, sibling, "BEGIN EXCLUSIVE")
	conn, err := db.openWriterConn(ctx)
	execAll(t, sibling, "ROLLBACK")
	sibling.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if db.walActive.Load() {
		t.Fatal("an unconfirmed WAL header enabled the read pool")
	}
	if rc, err := db.checkoutReadConn(ctx); rc != nil || err != nil {
		t.Fatalf("read conn %v err %v", rc, err)
	}
}

func TestConfirmedWALFileKeepsReadPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	defer db.Close()
	raw := openRaw(t, path)
	var mode string
	err := raw.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
	raw.Close()
	if err != nil || strings.ToLower(mode) != "wal" {
		return
	}
	if !db.walActive.Load() {
		t.Fatal("a confirmed WAL file did not enable the read pool")
	}
}
