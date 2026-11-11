package tuigateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestGitProbeRootsShareTheOneProcessCache(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required by this case")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	InvalidateGitRoots()
	t.Cleanup(InvalidateGitRoots)
	root := tools.GitProbeRepoRoot(dir)
	if root == "" {
		t.Fatal("tools probe found no root")
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := RepoRoot(dir); got != root {
		t.Fatalf("tuigateway root %q, want the cached %q", got, root)
	}
	InvalidateGitRoots()
	if got := tools.GitProbeRepoRoot(dir); got != "" {
		t.Fatalf("invalidate left the tools cache holding %q", got)
	}
}
