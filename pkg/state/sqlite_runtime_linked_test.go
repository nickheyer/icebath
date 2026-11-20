package state

import (
	"bytes"
	"context"
	"testing"
)

func TestLinkedRuntimeIsFixedSQLiteWithFTS5Trigram(t *testing.T) {
	conn := openWALTestConn(t, ":memory:")
	mustExecConn(t, conn, "CREATE VIRTUAL TABLE docs USING fts5(content, tokenize='trigram')")
	mustExecConn(t, conn, "INSERT INTO docs VALUES ('icebath')")
	row, err := fetchOne(context.Background(), conn, "SELECT count(*) FROM docs WHERE docs MATCH 'eba'")
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != int64(1) {
		t.Fatalf("trigram matches = %v", row[0])
	}
	if WALResetVulnerable(nil) {
		t.Fatalf("linked SQLite %s carries the WAL reset bug", LinkedSQLiteVersion())
	}
}

func TestProbeReportsTheRequestedInterpretersLinkedSQLite(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSQLiteRuntimeReport(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	info, err := sqliteRuntimeFromReport(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if info.SQLiteVersionString != LinkedSQLiteVersion() || info.SQLiteVersion.Cmp(LinkedSQLiteVersionInfo()) != 0 {
		t.Fatalf("report names %s, the binary links %s", info.SQLiteVersionString, LinkedSQLiteVersion())
	}
	if !info.Safe() || len(info.MissingFeatures) != 0 {
		t.Fatalf("linked SQLite is not safe for the store: %+v", info)
	}
}
