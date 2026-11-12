package pm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Snapshot of a committed checkout holds its code and none of the inert top level directories
func TestSnapshotLeavesOutEveryInertTopLevelDirectory(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("the snapshot archives a revision with git")
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "checkout")
	kept := []string{"go.mod", "cmd/icebath/main.go", "pkg/pm/lock.json", "pkg/skills/bundled/x/references/a.md"}
	for _, rel := range kept {
		writeFile(t, filepath.Join(repo, filepath.FromSlash(rel)), []byte(rel), 0o644)
	}
	for _, name := range inertSnapshotDirs {
		writeFile(t, filepath.Join(repo, name, "inert.txt"), []byte(name), 0o644)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "one"}} {
		cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	dest := filepath.Join(tmp, "snapshot")
	if err := bundleSnapshot(context.Background(), repo, "HEAD", dest, inertSnapshotDirs); err != nil {
		t.Fatal(err)
	}
	for _, name := range inertSnapshotDirs {
		if _, err := os.Lstat(filepath.Join(dest, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is in the snapshot (%v)", name, err)
		}
	}
	for _, rel := range kept {
		if data, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel))); err != nil || string(data) != rel {
			t.Errorf("%s holds %q (%v)", rel, data, err)
		}
	}
}
