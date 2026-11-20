package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// State database of fifty named sessions
func validSessionsDB(t *testing.T, path string) string {
	t.Helper()
	c := testConn(t, path, "")
	mustExec(t, c, "CREATE TABLE sessions (id INTEGER PRIMARY KEY, name TEXT)")
	for i := range 50 {
		if _, err := c.c.ExecContext(t.Context(), "INSERT INTO sessions (name) VALUES (?)", "s"+string(rune('0'+i%10))); err != nil {
			t.Fatal(err)
		}
	}
	closeQuietly(c)
	return path
}

func TestOversizedDBProbeCatchesMalformedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	c := testConn(t, path, "")
	mustExec(t, c, "CREATE TABLE t (a INTEGER)")
	closeQuietly(c)
	raw, _ := os.ReadFile(path)
	for i := 100; i < min(len(raw), 4096); i++ {
		raw[i] = 0xFF
	}
	os.WriteFile(path, raw, 0o644)
	one := int64(1)
	res := VerifySQLiteIntegrity(path, IntegrityOptions{MaxBytes: &one})
	if res.Valid || !strings.Contains(res.Message, "probe") {
		t.Fatalf("result %+v", res)
	}
}

func TestRestoreFlowEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	live := validSessionsDB(t, filepath.Join(tmp, "state.db"))
	snap := filepath.Join(tmp, "state-snapshots", "20260721-pre-update", "state.db")
	os.MkdirAll(filepath.Dir(snap), 0o755)
	raw, _ := os.ReadFile(live)
	os.WriteFile(snap, raw, 0o644)
	os.WriteFile(live, make([]byte, len(raw)), 0o644)
	if VerifySQLiteIntegrity(live, IntegrityOptions{}).Valid || !VerifySQLiteIntegrity(snap, IntegrityOptions{}).Valid {
		t.Fatal("zeroed live or valid snapshot misjudged")
	}
	os.WriteFile(live, raw, 0o644)
	if res := VerifySQLiteIntegrity(live, IntegrityOptions{}); !res.Valid || res.Message != "integrity check passed" {
		t.Fatalf("restored %+v", res)
	}
	if got := queryOne(t, live, "SELECT COUNT(*) FROM sessions"); got != "50" {
		t.Fatalf("rows %s", got)
	}
}

func TestVerifySQLiteIntegrityReportsEachStage(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "missing.db")
	if res := VerifySQLiteIntegrity(missing, IntegrityOptions{}); res.Valid || res.Message != "not found: "+missing || res.Size != nil {
		t.Fatalf("missing %+v", res)
	}
	small := filepath.Join(tmp, "small.db")
	os.WriteFile(small, []byte("tiny"), 0o644)
	if res := VerifySQLiteIntegrity(small, IntegrityOptions{}); res.Message != "too small (4 bytes) to be a valid SQLite database" || *res.Size != 4 {
		t.Fatalf("small %+v", res)
	}
	junk := filepath.Join(tmp, "junk.db")
	os.WriteFile(junk, []byte(strings.Repeat("A", 200)), 0o644)
	if res := VerifySQLiteIntegrity(junk, IntegrityOptions{}); res.Message != "missing SQLite header magic (got '41414141414141414141414141414141')" {
		t.Fatalf("junk %+v", res)
	}
	good := validSessionsDB(t, filepath.Join(tmp, "good.db"))
	if res := VerifySQLiteIntegrity(good, IntegrityOptions{SkipPragma: true}); !res.Valid || res.Message != "header check passed" {
		t.Fatalf("header only %+v", res)
	}
	one := int64(1)
	res := VerifySQLiteIntegrity(good, IntegrityOptions{MaxBytes: &one})
	if !res.Valid || !strings.HasSuffix(res.Message, " bytes exceeds max_bytes 1; skipped PRAGMA integrity_check (header + schema probe passed)") {
		t.Fatalf("probe %+v", res)
	}
}

func TestIsZeroedSQLiteFileDetectsNulHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	os.WriteFile(p, make([]byte, 4096), 0o644)
	if !state.IsZeroedStateDB(p, 100, false) {
		t.Fatal("all NUL file not zeroed")
	}
}
