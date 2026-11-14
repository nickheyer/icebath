package state

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
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
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("the probe test runs the host python3 interpreter: %v", err)
	}
	info, err := ProbeSQLiteRuntime(python, 30)
	if err != nil || info == nil {
		t.Fatalf("probe = %v, %v", info, err)
	}
	out, err := exec.Command(python, "-I", "-c", "import json, sqlite3, sys\n"+
		"with sqlite3.connect(':memory:') as c: sid = c.execute('SELECT sqlite_source_id()').fetchone()[0]\n"+
		"print(json.dumps([sys.executable, sys.base_prefix, list(sys.version_info[:3]), list(sqlite3.sqlite_version_info), sqlite3.sqlite_version, sid]))").Output()
	if err != nil {
		t.Fatal(err)
	}
	var want []any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if resolve(info.Executable) != resolve(want[0].(string)) || resolve(info.BasePrefix) != resolve(want[1].(string)) {
		t.Fatalf("paths = %q %q want %v", info.Executable, info.BasePrefix, want[:2])
	}
	for i, got := range []VersionTuple{info.PythonVersion, info.SQLiteVersion} {
		expected, err := versionTuple(want[2+i])
		if err != nil || got.Cmp(expected) != 0 {
			t.Fatalf("version %d = %v want %v (%v)", i, got, want[2+i], err)
		}
	}
	if info.SQLiteVersionString != want[4] || info.SQLiteSourceID != want[5] {
		t.Fatalf("sqlite = %q %q want %v", info.SQLiteVersionString, info.SQLiteSourceID, want[4:])
	}
}
