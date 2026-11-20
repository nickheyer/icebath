package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func tuiResumeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Repository with one commit and a recorded launcher worktree the test then removes by hand
func recordedLauncherTree(t *testing.T) (string, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	tuiResumeGit(t, repo, "init", "-q")
	tuiResumeGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	tree := filepath.Join(repo, ".worktrees", "tui-1")
	tuiResumeGit(t, repo, "worktree", "add", "-q", tree, "-b", "tui/one", "HEAD")
	RecordTUIWorktree(context.Background(), map[string]string{"path": tree, "branch": "tui/one", "repo_root": repo})
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	return repo, tree
}

func TestRecreateTUIWorktreeRebuildsARecordedTreeOnItsBranch(t *testing.T) {
	_, tree := recordedLauncherTree(t)
	got := RecreateTUIWorktree(context.Background(), filepath.Join(tree, "src"), "sess-1")
	if got != tree {
		t.Fatalf("recreated %q want %q", got, tree)
	}
	if st, err := os.Stat(tree); err != nil || !st.IsDir() {
		t.Fatalf("tree missing after recreate: %v", err)
	}
	if RecreateTUIWorktree(context.Background(), tree, "sess-1") != "" {
		t.Fatal("an existing directory was recreated")
	}
}

func TestRecreateTUIWorktreeLeavesUnrecordedDirectoriesAlone(t *testing.T) {
	repo, _ := recordedLauncherTree(t)
	other := filepath.Join(repo, ".worktrees", "never-recorded")
	if RecreateTUIWorktree(context.Background(), other, "sess-2") != "" {
		t.Fatal("an unrecorded tree was created")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("unrecorded path appeared: %v", err)
	}
}
