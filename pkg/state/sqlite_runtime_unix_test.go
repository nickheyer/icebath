//go:build unix

package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writes an executable shell script standing in for a Python interpreter
func fakeInterpreter(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeSQLiteRuntimeRunsTheIsolatedInterpreter(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	binary := fakeInterpreter(t, `printf '%s\n' "$#" "$1" > `+record+`
printf '{"sqlite_version": [3, 51, 3], "sqlite_version_string": "3.51.3", "sqlite_source_id": "2026-01-01 abc", "missing_features": ["FTS5"]}\n'`)
	info, err := ProbeSQLiteRuntime(binary, 30)
	if err != nil || info == nil {
		t.Fatalf("%v %v", info, err)
	}
	if info.SQLiteVersionString != "3.51.3" || info.SQLiteVersion.Cmp(NewVersionTuple(3, 51, 3)) != 0 || info.WALResetVulnerable() ||
		len(info.MissingFeatures) != 1 || info.MissingFeatures[0] != "FTS5" || info.Safe() {
		t.Fatalf("%+v", info)
	}
	got, _ := os.ReadFile(record)
	if want := "1\n" + SQLiteRuntimeVerb + "\n"; string(got) != want {
		t.Fatalf("binary saw %q", got)
	}
}

func TestProbeSQLiteRuntimeFailuresAreNil(t *testing.T) {
	for name, body := range map[string]string{
		"nonzero exit":   `echo '{}'; exit 3`,
		"not json":       `echo 'sqlite says hi'`,
		"missing key":    `echo '{"sqlite_version_string": "3.51.3"}'`,
		"no source id":   `echo '{"sqlite_version": [3], "sqlite_version_string": "3", "missing_features": []}'`,
		"invalid utf8":   `printf '\377{}'`,
		"killed by kill": `kill -9 $$`,
	} {
		info, err := ProbeSQLiteRuntime(fakeInterpreter(t, body), 30)
		if info != nil || err == nil {
			t.Fatalf("%s gave %v %v", name, info, err)
		}
	}
	info, err := ProbeSQLiteRuntime(fakeInterpreter(t, `sleep 5`), 0.2)
	if info != nil || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout gave %v %v", info, err)
	}
}

func TestProbeSQLiteRuntimeRaisesTheIntOverflow(t *testing.T) {
	binary := fakeInterpreter(t, `echo '{"sqlite_version": [Infinity], "sqlite_version_string": "3", "sqlite_source_id": "x", "missing_features": []}'`)
	info, err := ProbeSQLiteRuntime(binary, 30)
	if info != nil || err == nil || !strings.Contains(err.Error(), "unreadable report") {
		t.Fatalf("%v %v", info, err)
	}
}
