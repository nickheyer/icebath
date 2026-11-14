//go:build unix

package state

import (
	"errors"
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
	t.Setenv("PYTHONPATH", "/leak")
	record := filepath.Join(t.TempDir(), "record")
	python := fakeInterpreter(t, `printf '%s\n' "$1" "$2" "PYTHONPATH=${PYTHONPATH-unset}" > `+record+`
printf '%s' "$3" >> `+record+`
printf '{"base_prefix": "/opt/py", "executable": "/opt/py/bin/python3", "python_version": [3, 13, 1], "sqlite_version": [3, 51, 3], "sqlite_version_string": "3.51.3", "sqlite_source_id": "2026-01-01 abc"}\n'`)
	info, err := ProbeSQLiteRuntime(python, 30)
	if err != nil || info == nil {
		t.Fatalf("%v %v", info, err)
	}
	if info.Executable != "/opt/py/bin/python3" || info.BasePrefix != "/opt/py" || info.SQLiteSourceID != "2026-01-01 abc" ||
		info.SQLiteVersion.Cmp(NewVersionTuple(3, 51, 3)) != 0 || info.WALResetVulnerable() {
		t.Fatalf("%+v", info)
	}
	got, _ := os.ReadFile(record)
	want := "-I\n-c\nPYTHONPATH=unset\n" + sqliteProbeScript
	if string(got) != want {
		t.Fatalf("interpreter saw %q", got)
	}
}

func TestProbeSQLiteRuntimeFailuresAreNil(t *testing.T) {
	for name, body := range map[string]string{
		"nonzero exit":   `echo '{}'; exit 3`,
		"not json":       `echo 'sqlite says hi'`,
		"missing key":    `echo '{"executable": "x"}'`,
		"bad version":    `echo '{"executable": "x", "base_prefix": "y", "python_version": "3.1", "sqlite_version": [3], "sqlite_version_string": "3"}'`,
		"invalid utf8":   `printf '\377{}'`,
		"killed by kill": `kill -9 $$`,
	} {
		info, err := ProbeSQLiteRuntime(fakeInterpreter(t, body), 30)
		if info != nil || err != nil {
			t.Fatalf("%s gave %v %v", name, info, err)
		}
	}
	info, err := ProbeSQLiteRuntime(fakeInterpreter(t, `sleep 5`), 0.2)
	if info != nil || err != nil {
		t.Fatalf("timeout gave %v %v", info, err)
	}
}

func TestProbeSQLiteRuntimeRaisesTheIntOverflow(t *testing.T) {
	python := fakeInterpreter(t, `echo '{"executable": "x", "base_prefix": "y", "python_version": [Infinity], "sqlite_version": [3], "sqlite_version_string": "3"}'`)
	info, err := ProbeSQLiteRuntime(python, 30)
	var conv *pyConvError
	if info != nil || !errors.As(err, &conv) || conv.Class != "OverflowError" || !strings.Contains(conv.Msg, "infinity") {
		t.Fatalf("%v %v", info, err)
	}
}
