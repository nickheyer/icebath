package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepClearsPycacheWhenCheckoutChanged(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git := filepath.Join(repo, ".git")
	if err := os.MkdirAll(filepath.Join(git, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(git, "refs", "heads", "main"), []byte(strings.Repeat("b", 40)+"\n"), 0o644)
	cache := filepath.Join(repo, "icebath_cli", "__pycache__")
	_ = os.MkdirAll(cache, 0o755)
	_ = os.WriteFile(filepath.Join(cache, "main.cpython-311.pyc"), []byte("stale"), 0o644)
	prev := fleetProjectRoot
	fleetProjectRoot = func() string { return repo }
	t.Cleanup(func() { fleetProjectRoot = prev })
	stamp := filepath.Join(repo, bytecodeFingerprintFile)
	_ = os.WriteFile(stamp, []byte("git:refs/heads/main:"+strings.Repeat("a", 40)), 0o644)
	SweepStaleBytecodeIfCheckoutChanged()
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("stale cache kept")
	}
	if raw, _ := os.ReadFile(stamp); !strings.HasSuffix(strings.TrimSpace(string(raw)), strings.Repeat("b", 40)) {
		t.Fatalf("stamp %q", raw)
	}
}
