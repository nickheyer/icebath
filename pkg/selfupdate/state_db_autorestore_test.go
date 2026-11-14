package selfupdate

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func autorestoreDB(t *testing.T, path, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE t (v TEXT)", "INSERT INTO t VALUES ('" + marker + "')"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func autorestoreMarker(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("SELECT v FROM t LIMIT 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUpdateAutorestoreRefusesUnderOwnLiveConnection(t *testing.T) {
	tmp := t.TempDir()
	dst, src := filepath.Join(tmp, "state.db"), filepath.Join(tmp, "snap.db")
	autorestoreDB(t, src, "snapshot-good")
	autorestoreDB(t, dst, "live-old")
	ctx := context.Background()
	held, err := state.ConnectTracked(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for _, q := range []string{"PRAGMA journal_mode=WAL", "INSERT INTO t VALUES ('held-write')"} {
		if _, err := held.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	walBefore, err := os.Stat(dst + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(dst, os.O_RDWR, 0)
	f.Write(make([]byte, 100))
	f.Close()
	var out bytes.Buffer
	if ok, err := restoreStateDBFromSnapshot(ctx, &out, dst, src); ok || err != nil {
		t.Fatalf("restore %v %v", ok, err)
	}
	if !strings.Contains(out.String(), "Auto-restore refused") {
		t.Fatal(out.String())
	}
	walAfter, err := os.Stat(dst + "-wal")
	if err != nil || !os.SameFile(walBefore, walAfter) {
		t.Fatal("held WAL generation replaced")
	}
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, e := range entries {
		if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && strings.Contains(target, dst) && strings.Contains(target, "(deleted)") {
			t.Fatalf("deleted descriptor %s", target)
		}
	}
}

func TestUpdateAutorestoreStillWorksWithoutHolder(t *testing.T) {
	tmp := t.TempDir()
	dst, src := filepath.Join(tmp, "state.db"), filepath.Join(tmp, "snap.db")
	autorestoreDB(t, src, "snapshot-good")
	autorestoreDB(t, dst, "live-old")
	if err := os.WriteFile(dst+"-wal", make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if ok, err := restoreStateDBFromSnapshot(context.Background(), &out, dst, src); !ok || err != nil {
		t.Fatalf("restore %v %v\n%s", ok, err, out.String())
	}
	if autorestoreMarker(t, dst) != "snapshot-good" {
		t.Fatal("marker not restored")
	}
	if _, err := os.Stat(dst + "-wal"); err == nil {
		t.Fatal("stale WAL left beside the restored database")
	}
}
