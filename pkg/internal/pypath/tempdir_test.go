package pypath

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestTempdirCandidatesFollowTempfileOrder(t *testing.T) {
	t.Setenv("TMPDIR", "/t1")
	t.Setenv("TEMP", "")
	t.Setenv("TMP", "/t3")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/t1", "/t3", "/tmp", "/var/tmp", "/usr/tmp", cwd}
	if runtime.GOOS == "windows" {
		home, _ := os.UserHomeDir()
		want = []string{"/t1", "/t3", filepath.Join(home, "AppData", "Local", "Temp"), os.ExpandEnv(`${SYSTEMROOT}\Temp`), `c:\temp`, `c:\tmp`, `\temp`, `\tmp`, cwd}
	}
	if got := TempdirCandidates(); !slices.Equal(got, want) {
		t.Fatalf("TempdirCandidates = %q, want %q", got, want)
	}
}
