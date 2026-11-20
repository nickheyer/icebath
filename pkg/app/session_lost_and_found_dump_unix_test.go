//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoverAttemptsSurviveDumpStderrBeyondPipeBuffer(t *testing.T) {
	tmp := t.TempDir()
	fakeSQLite3 := filepath.Join(tmp, "sqlite3")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-readonly\" ]; then\n" +
		"  i=0\n" +
		"  while [ $i -lt 3000 ]; do\n" +
		"    echo \"lost and found page $i: orphan btree cell, unable to reconstruct row\" >&2\n" +
		"    i=$((i+1))\n" +
		"  done\n" +
		"  echo \"Error: near line 4000: file is not a database\" >&2\n" +
		"  echo \"CREATE TABLE t(x); INSERT INTO t VALUES(1);\"\n" +
		"else\n" +
		"  cat >/dev/null\n" +
		"fi\n"
	if err := os.WriteFile(fakeSQLite3, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(tmp, "source.db")
	if err := os.WriteFile(source, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	attempts, err := lfCLIRecoverAttempts(source, filepath.Join(tmp, "lost_and_found.db"), fakeSQLite3, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= 30*time.Second {
		t.Fatal("the undrained stderr burned the whole salvage timeout")
	}
	last := attempts[len(attempts)-1]
	if code, _ := last.GetOr("dump_returncode", nil).(int64); code != 0 {
		t.Fatalf("dump returncode %v", last.GetOr("dump_returncode", nil))
	}
	if tail, _ := last.GetOr("dump_stderr_tail", "").(string); !strings.Contains(tail, "file is not a database") {
		t.Fatalf("stderr tail %q", tail)
	}
}
