package state

import (
	"os"
	"path/filepath"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestStorageHealthLatch(t *testing.T) {
	ResetStorageState("")
	t.Cleanup(func() { ResetStorageState("") })
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if StorageState(db) != StorageOK {
		t.Fatal("fresh path must read ok")
	}
	fts := dbErr(`fts5: corrupt structure record for table "messages_fts"`, 0, SQLiteDatabaseError)
	schema := dbErr("malformed database schema (messages_fts)", 0, SQLiteDatabaseError)
	if NoteStorageError(db, fts) || NoteStorageError(db, schema) || NoteStorageError(db, opErr("database is locked")) {
		t.Fatal("only structural corruption latches")
	}
	if NoteStorageError(db, &SQLiteError{Class: SQLiteOperationalError, Msg: "database disk image is malformed"}) != true {
		t.Fatal("an operational corrupt image is still a DatabaseError and latches")
	}
	ResetStorageState(db)
	first := dbErr("database disk image is malformed", sqlite3.SQLITE_CORRUPT, SQLiteDatabaseError)
	if !NoteStorageError(db, first) {
		t.Fatal("structural corruption must latch")
	}
	MarkStorageCorrupt(db, "second reason")
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(link, "state.db")
	if StorageState(via) != StorageCorrupt {
		t.Error("a symlinked spelling of the path shares the latch")
	}
	if reason, ok := StorageCorruptReason(via); !ok || reason != "database disk image is malformed" {
		t.Errorf("first reason kept, got %q %v", reason, ok)
	}
	ResetStorageState(via)
	if StorageState(db) != StorageOK {
		t.Error("reset by any spelling clears the latch")
	}
}

func TestRealpathLaxResolvesExistingPrefix(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "rel")); err != nil {
		t.Fatal(err)
	}
	base := realpathLax(dir)
	if got := realpathLax(filepath.Join(dir, "rel", "missing", "..", "state.db")); got != filepath.Join(base, "real", "state.db") {
		t.Errorf("realpathLax = %q", got)
	}
	if got := readEmptyHeader(t, dir); got != 0 {
		t.Errorf("empty file header length %d", got)
	}
}

// Reads the header of an empty file and returns its length, failing when the read refuses
func readEmptyHeader(t *testing.T, dir string) int {
	path := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	data, ok := ReadHeaderBytesPreopen(path, 100, false)
	if !ok || data == nil {
		t.Fatal("an empty file reads as zero bytes like Python")
	}
	return len(data)
}
