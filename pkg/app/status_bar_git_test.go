package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentGitBranchReadsHeadAndWorktreePointer(t *testing.T) {
	tmp := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(tmp, "repo")
	write(filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/minimax-inspired/status-bar-git-branch\n")
	resetGitBranchCache()
	if got := currentGitBranch(repo); got != "minimax-inspired/status-bar-git-branch" {
		t.Fatalf("plain repo branch %q", got)
	}
	private := filepath.Join(tmp, "gitdir-store")
	write(filepath.Join(private, "HEAD"), "ref: refs/heads/feature-x\n")
	wt := filepath.Join(tmp, "wt")
	write(filepath.Join(wt, ".git"), "gitdir: "+private+"\n")
	resetGitBranchCache()
	if got := currentGitBranch(wt); got != "feature-x" {
		t.Fatalf("worktree branch %q", got)
	}
	plain := filepath.Join(tmp, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	resetGitBranchCache()
	if got := currentGitBranch(plain); got != "" {
		t.Fatalf("a directory outside any repository gave %q", got)
	}
}
