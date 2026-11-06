package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// A stale unmarked host child tree is replaced by the fresh extraction with its marker
func TestMaterializeHostChildReplacesAStaleUnmarkedTree(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir, err := materializeHostChild()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".complete")
	want, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("left by a crashed extraction"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := materializeHostChild()
	if err != nil {
		t.Fatalf("second extraction over the stale tree failed: %v", err)
	}
	if again != dir {
		t.Fatalf("dir %q, want %q", again, dir)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != string(want) {
		t.Fatalf("marker %q (%v), want %q", got, err, want)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icebath_cli", "plugin_host_child.py")); err != nil {
		t.Fatalf("extracted child missing: %v", err)
	}
	leftovers, _ := filepath.Glob(dir + ".*.tmp")
	if len(leftovers) != 0 {
		t.Fatalf("temporary trees left behind: %v", leftovers)
	}
}
