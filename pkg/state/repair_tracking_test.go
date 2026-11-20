package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRepairConnectionsAreTrackedForByteProbeSafety(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "state.db")
	makeRepairDB(t, db)
	if HasLiveConnection(db) {
		t.Fatal("seed connection still tracked")
	}
	conn, err := connectRepairDurable(context.Background(), db, 5.0)
	if err != nil {
		t.Fatal(err)
	}
	if !HasLiveConnection(db) {
		t.Fatal("repair connection must be tracked")
	}
	if b, ok := ReadHeaderBytesPreopen(db, 16, false); ok || b != nil {
		t.Fatal("byte probe must be refused while the repair connection is live")
	}
	conn.Close()
	if HasLiveConnection(db) {
		t.Fatal("closed repair connection still tracked")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); !ok {
		t.Fatal("byte probe must succeed once no connection is live")
	}
}

func TestByteProbeNeverCancelsTheRepairExclusion(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "state.db")
	conn := openRaw(t, db)
	execAll(t, conn, "PRAGMA journal_mode=DELETE", "CREATE TABLE t(v TEXT)")
	conn.Close()
	if !externalWriterCanBreakIn(t, db) {
		t.Fatal("fixture precondition: an unguarded writer must get in")
	}
	guard, err := openExclusive(context.Background(), db, "BEGIN EXCLUSIVE")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if externalWriterCanBreakIn(t, db) {
		t.Fatal("the exclusive guard must keep writers out")
	}
	if _, ok := ReadHeaderBytesPreopen(db, 16, false); ok {
		t.Fatal("byte probe must be refused under the repair guard")
	}
	if externalWriterCanBreakIn(t, db) {
		t.Fatal("the repair's EXCLUSIVE lock was cancelled by a byte-level probe on the database it holds")
	}
}
