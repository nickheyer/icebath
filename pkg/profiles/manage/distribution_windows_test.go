package manage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestGitSourceRemovesReadOnlyGitMetadata(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "--quiet", origin)
	writeFile(t, filepath.Join(origin, profiles.ManifestFilename), "name: demo\nversion: 1.0.0\n")
	run("-C", origin, "add", ".")
	run("-C", origin, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "init")
	var sawReadOnly []bool
	old := distGitClone
	t.Cleanup(func() { distGitClone = old })
	distGitClone = func(_ context.Context, _ string, dest string) error {
		run("clone", "--quiet", origin, dest)
		found, readOnly := false, false
		filepath.WalkDir(filepath.Join(dest, ".git", "objects"), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				found = true
				if st, err := os.Stat(p); err == nil && st.Mode().Perm()&0o200 == 0 {
					readOnly = true
				}
			}
			return nil
		})
		sawReadOnly = append(sawReadOnly, found && readOnly)
		return nil
	}
	work := filepath.Join(tmp, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	staged, _, err := stageSource(context.Background(), "https://example.invalid/demo.git", work)
	if err != nil {
		t.Fatal(err)
	}
	if len(sawReadOnly) != 1 || !sawReadOnly[0] || exists(filepath.Join(staged, ".git")) {
		t.Fatalf("%v", sawReadOnly)
	}
}
