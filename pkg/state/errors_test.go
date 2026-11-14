package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func pathErr(errno syscall.Errno) error {
	return &os.PathError{Op: "write", Path: "state.db", Err: errno}
}

func opErr(msg string) error {
	return &SQLiteError{Class: SQLiteOperationalError, Msg: msg}
}

func dbErr(msg string, code int, class SQLiteClass) error {
	return &SQLiteError{Class: class, Msg: msg, Code: code, HasCode: code != 0}
}

func TestDiskFullError(t *testing.T) {
	cases := []struct {
		in   any
		want bool
	}{
		{pathErr(syscall.ENOSPC), true},
		{opErr("database or disk is full"), true},
		{"disk full: session storage could not be written", true},
		{"ENOSPC writing state.db", true},
		{"This is often a full disk \u2014 free some space", true},
		{nil, false},
		{pathErr(syscall.EACCES), false},
		{errors.New("network timeout"), false},
		{"session not found", false},
		{"session storage could not be written: permission denied", false},
	}
	for _, c := range cases {
		if got := IsDiskFullError(c.in); got != c.want {
			t.Errorf("IsDiskFullError(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestClassifyPersistenceErrorCategories(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{opErr("database is locked"), "locked"},
		{"SQLITE_BUSY: busy", "locked"},
		{opErr("database or disk is full"), "disk"},
		{"attempt to write a readonly database", "disk"},
		{"read-only file system", "disk"},
		{"no space left on device", "disk"},
		{"disk I/O error", "disk"},
		{"something else entirely", "unknown"},
		{nil, "unknown"},
		{"", "unknown"},
		{dbErr("database disk image is malformed", 0, SQLiteDatabaseError), "corrupt"},
		{"database disk image is malformed", "corrupt"},
		{dbErr("file is not a database", 0, SQLiteDatabaseError), "corrupt"},
		{"malformed database schema", "corrupt"},
		{"database or disk is full", "disk"},
		{"ENOSPC writing state.db", "disk"},
		{"There is not enough space on the disk", "disk"},
		{pathErr(syscall.ENOSPC), "disk"},
		{&SessionCompressionInProgressError{Msg: "Session 'abc' is being compressed by another writer"}, "compression"},
		{&CompressionSessionBusyError{Msg: "Compression lease lost before publication: abc"}, "compression"},
		{"Session 'abc' is being compressed by another writer", "compression"},
		{"Compression lease lost before publication: abc", "compression"},
		{&SessionTurnLeaseLostError{Msg: "Session turn lease lost; refusing transcript write for 'abc'"}, "turn_lease"},
		{"Session turn lease lost; refusing transcript write for 'abc'", "turn_lease"},
		{&StateDBCorruptError{Msg: "quarantined"}, "corrupt"},
		{&CompressionSessionClosedError{SessionID: "abc"}, "compression_closed"},
		{&DeletedWalGenerationError{Msg: DeletedWalGenerationMsg}, "deleted_wal"},
		{&StateDBReplacedError{Msg: StateDBReplacedMsg}, "replaced"},
		{DeletedWalGenerationMsg, "deleted_wal"},
		{fmt.Errorf("flush: %w", dbErr("FOREIGN KEY constraint failed", sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY, SQLiteIntegrityError)), "session_row_missing"},
	}
	for _, c := range cases {
		if got := ClassifyPersistenceError(c.in); got != c.want {
			t.Errorf("ClassifyPersistenceError(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPersistenceErrorCausesMatchClassifier(t *testing.T) {
	if !slices.Contains(PersistenceErrorCauses, "deleted_wal") {
		t.Fatal("deleted_wal missing from causes")
	}
	probes := []any{
		"database is locked",
		"Session 'abc' is being compressed by another writer",
		"Session turn lease lost; refusing transcript write for 'abc'",
		"database disk image is malformed",
		`fts5: corrupt structure record for table "messages_fts"`,
		"FATAL: state.db was replaced underneath the gateway",
		"FATAL: a live process holds a deleted state.db-wal or state.db-shm inode.",
		"database or disk is full",
		"something else entirely",
		nil,
	}
	for _, probe := range probes {
		if got := ClassifyPersistenceError(probe); !slices.Contains(PersistenceErrorCauses, got) {
			t.Errorf("%v classified as %q outside the causes", probe, got)
		}
	}
}

func TestClassifyPersistenceErrorFTSProvenanceOrder(t *testing.T) {
	vtab := dbErr("database disk image is malformed", SQLiteCorruptVtab, SQLiteDatabaseError)
	if got := ClassifyPersistenceError(vtab); got != "fts_index" {
		t.Errorf("vtab = %q", got)
	}
	if got := ClassifyPersistenceError(dbErr("database disk image is malformed", sqlite3.SQLITE_CORRUPT, SQLiteDatabaseError)); got != "corrupt" {
		t.Errorf("corrupt = %q", got)
	}
	if got := ClassifyPersistenceError(dbErr("file is not a database", sqlite3.SQLITE_NOTADB, SQLiteDatabaseError)); got != "corrupt" {
		t.Errorf("notadb = %q", got)
	}
	contradictory := dbErr(`fts5: corrupt structure record for table "messages_fts"`, sqlite3.SQLITE_CONSTRAINT_TRIGGER, SQLiteIntegrityError)
	if IsFTSScopedCorruptionError(contradictory) || ClassifyPersistenceError(contradictory) == "fts_index" {
		t.Error("a contradicting code must outrank FTS prose")
	}
	cases := map[any]string{
		dbErr(`fts5: corrupt structure record for table "messages_fts"`, 0, SQLiteDatabaseError): "fts_index",
		`fts5: corruption found reading blob 2061584302081 from table "messages_fts"`:            "fts_index",
		`fts5: corrupt structure record for table "messages_fts_trigram"`:                        "fts_index",
		"malformed inverted index for FTS5 table main.messages_fts":                              "fts_index",
		"database disk image is malformed":                                                       "corrupt",
		`fts5: syntax error near "x"`:                                                            "unknown",
		"no such table: messages_fts":                                                            "unknown",
	}
	for in, want := range cases {
		if got := ClassifyPersistenceError(in); got != want {
			t.Errorf("ClassifyPersistenceError(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorClassHierarchy(t *testing.T) {
	var busy *CompressionSessionBusyError
	if !errors.As(&SessionCompressionInProgressError{Msg: "x"}, &busy) {
		t.Error("in progress must match its busy parent")
	}
	var unresolved *PruneRowUnresolvedError
	if !errors.As(&PruneRowStaleError{Msg: "x"}, &unresolved) {
		t.Error("stale must match its unresolved parent")
	}
	var replaced *StateDBReplacedError
	if !errors.As(&DeletedWalGenerationError{Msg: "x"}, &replaced) {
		t.Error("deleted wal must match its replaced parent")
	}
	if !isDatabaseError(&StateDBCorruptError{Msg: "x"}) || isOperationalError(&StateDBCorruptError{Msg: "x"}) {
		t.Error("corrupt handle is a plain DatabaseError")
	}
	if got := (&CompressionSessionClosedError{SessionID: "it's"}).Error(); got != `Session "it's" is closed by compression; adopt its live continuation before appending messages` {
		t.Errorf("closed message = %q", got)
	}
}

func TestDriverErrorsReadLikePython(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("SELECT * FROM missing_table")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := errText(err); got != "no such table: missing_table" {
		t.Errorf("errText = %q", got)
	}
	if !isOperationalError(err) || IsSQLiteLockError(err) || IsTransientSQLiteError(err) {
		t.Error("a missing table is a non transient OperationalError")
	}
	if !IsSQLiteLockError(opErr("database is locked")) || !IsTransientSQLiteError(opErr("disk I/O error")) {
		t.Error("lock and disk io text classify as transient")
	}
	if IsTransientSQLiteError(dbErr("database is locked", 0, SQLiteDatabaseError)) {
		t.Error("only OperationalError is transient")
	}
	if !isNoMoreRows(opErr("No more rows available")) {
		t.Error("no more rows text must match")
	}
	if !IsMalformedSchemaError(dbErr("malformed database schema (messages_fts)", 0, SQLiteDatabaseError)) ||
		IsMalformedSchemaError(dbErr("database disk image is malformed", 0, SQLiteDatabaseError)) ||
		!IsMalformedDBError(dbErr("database disk image is malformed", 0, SQLiteDatabaseError)) ||
		IsMalformedDBError(errors.New("database disk image is malformed")) {
		t.Error("malformed predicates need a DatabaseError and their own markers")
	}
}
