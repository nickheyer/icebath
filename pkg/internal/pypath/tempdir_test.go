package pypath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGettempdirPrefersTMPDIRAndSkipsUnusableCandidates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if got, err := Gettempdir(); err != nil || got != dir {
		t.Fatalf("Gettempdir = %q, %v, want %q", got, err, dir)
	}
	t.Setenv("TMPDIR", filepath.Join(dir, "missing"))
	t.Setenv("TEMP", dir)
	if got, err := Gettempdir(); err != nil || got != dir {
		t.Fatalf("Gettempdir = %q, %v, want TEMP %q", got, err, dir)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("probe files left behind: %v", entries)
	}
}
