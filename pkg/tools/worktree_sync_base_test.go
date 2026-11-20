package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

func wsbRun(t *testing.T, cwd string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	rc := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%v: %v", args, err)
		}
		rc = exit.ExitCode()
	}
	return strings.TrimSpace(string(out)), rc
}

func wsbCommit(t *testing.T, repo, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsbRun(t, repo, "git", "add", ".")
	wsbRun(t, repo, "git", "commit", "-m", msg)
}

func wsbHead(t *testing.T, repo string) string {
	t.Helper()
	head, _ := wsbRun(t, repo, "git", "rev-parse", "HEAD")
	return head
}

func wsbRemoteAndClone(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	if err := os.Mkdir(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	wsbRun(t, seed, "git", "init")
	wsbRun(t, seed, "git", "config", "user.email", "t@t.com")
	wsbRun(t, seed, "git", "config", "user.name", "T")
	wsbRun(t, seed, "git", "checkout", "-b", "main")
	wsbCommit(t, seed, "README.md", "base commit")
	wsbRun(t, tmp, "git", "init", "--bare", remote)
	wsbRun(t, seed, "git", "remote", "add", "origin", remote)
	wsbRun(t, seed, "git", "push", "origin", "main")
	wsbRun(t, remote, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	clone := filepath.Join(tmp, "clone")
	wsbRun(t, tmp, "git", "clone", remote, clone)
	wsbRun(t, clone, "git", "config", "user.email", "t@t.com")
	wsbRun(t, clone, "git", "config", "user.name", "T")
	staleLocalHead := wsbHead(t, clone)
	wsbCommit(t, seed, "feature.txt", "remote-only commit")
	wsbRun(t, seed, "git", "push", "origin", "main")
	remoteHead := wsbHead(t, seed)
	if remoteHead == staleLocalHead {
		t.Fatal("fixture: the remote never advanced past the clone")
	}
	return clone, remoteHead, staleLocalHead
}

func wsbPlainRepo(t *testing.T, name string) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	repo := filepath.Join(tmp, name)
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	wsbRun(t, repo, "git", "init")
	wsbRun(t, repo, "git", "config", "user.email", "t@t.com")
	wsbRun(t, repo, "git", "config", "user.name", "T")
	return repo
}

func TestResolveWorktreeBaseResolvesToFetchedUpstream(t *testing.T) {
	clone, remoteHead, staleLocalHead := wsbRemoteAndClone(t)
	baseRef, label := resolveWorktreeBase(clone, 5, 300)
	if baseRef != "origin/main" {
		t.Fatalf("base %q label %q", baseRef, label)
	}
	if !strings.Contains(label, "fetched") {
		t.Fatalf("label %q", label)
	}
	resolved, _ := wsbRun(t, clone, "git", "rev-parse", baseRef)
	if resolved != remoteHead || resolved == staleLocalHead {
		t.Fatalf("resolved %s remote %s stale %s", resolved, remoteHead, staleLocalHead)
	}
}

func TestResolveWorktreeBaseResolvesRemoteTipOnTagPinnedNarrowClone(t *testing.T) {
	clone, remoteHead, _ := wsbRemoteAndClone(t)
	tmp := filepath.Dir(clone)
	wsbRun(t, clone, "git", "tag", "v0")
	wsbRun(t, clone, "git", "push", "origin", "v0")
	remote, _ := wsbRun(t, clone, "git", "remote", "get-url", "origin")
	narrow := filepath.Join(tmp, "narrow")
	wsbRun(t, tmp, "git", "clone", "-q", "--single-branch", "--branch", "v0", remote, narrow)
	baseRef, label := resolveWorktreeBase(narrow, 5, 300)
	if baseRef != "origin/main" || label != "origin/main (fetched)" {
		t.Fatalf("base %q label %q", baseRef, label)
	}
	if resolved, _ := wsbRun(t, narrow, "git", "rev-parse", baseRef); resolved != remoteHead {
		t.Fatalf("resolved %s want %s", resolved, remoteHead)
	}
}

func TestResolveWorktreeBaseFallsBackToHeadWithoutRemote(t *testing.T) {
	repo := wsbPlainRepo(t, "no-remote")
	wsbCommit(t, repo, "README.md", "only commit")
	baseRef, label := resolveWorktreeBase(repo, 5, 300)
	if baseRef != "HEAD" || !strings.Contains(label, "HEAD") {
		t.Fatalf("base %q label %q", baseRef, label)
	}
}

func wsbSpyFetches(t *testing.T, stall bool) *[][]string {
	t.Helper()
	prev := resolveBaseGitFn
	t.Cleanup(func() { resolveBaseGitFn = prev })
	calls := &[][]string{}
	resolveBaseGitFn = func(args []string, cwd string, timeout time.Duration, env map[string]string) (*worktreeGitResult, error) {
		if slices.Contains(args, "fetch") {
			*calls = append(*calls, append([]string{}, args...))
			if stall {
				return nil, &pysubprocess.TimeoutExpired{Cmd: append([]string{"git"}, args...), Timeout: int(timeout / time.Second)}
			}
		}
		return worktreeGit(args, cwd, timeout, env)
	}
	return calls
}

func TestResolveWorktreeBaseStartupCostFreshFetchHeadSkipsTheFetch(t *testing.T) {
	clone, remoteHead, _ := wsbRemoteAndClone(t)
	wsbRun(t, clone, "git", "fetch", "origin", "main")
	calls := wsbSpyFetches(t, false)
	baseRef, label := resolveWorktreeBase(clone, 5, 300)
	if baseRef != "origin/main" {
		t.Fatalf("base %q", baseRef)
	}
	if !strings.Contains(label, "fetched") || !strings.Contains(label, "ago") {
		t.Fatalf("label %q", label)
	}
	if len(*calls) != 0 {
		t.Fatalf("fresh FETCH_HEAD must skip the network fetch: %v", *calls)
	}
	if resolved, _ := wsbRun(t, clone, "git", "rev-parse", baseRef); resolved != remoteHead {
		t.Fatalf("resolved %s want %s", resolved, remoteHead)
	}
}

func TestResolveWorktreeBaseStartupCostStaleFetchHeadRefetches(t *testing.T) {
	clone, _, _ := wsbRemoteAndClone(t)
	wsbRun(t, clone, "git", "fetch", "origin", "main")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(clone, ".git", "FETCH_HEAD"), old, old); err != nil {
		t.Fatal(err)
	}
	baseRef, label := resolveWorktreeBase(clone, 5, 300)
	if baseRef != "origin/main" || label != "origin/main (fetched)" {
		t.Fatalf("base %q label %q", baseRef, label)
	}
}

func TestResolveWorktreeBaseStartupCostFetchTimeoutFallsBackToCachedRef(t *testing.T) {
	clone, _, staleLocalHead := wsbRemoteAndClone(t)
	fetches := wsbSpyFetches(t, true)
	start := time.Now()
	baseRef, label := resolveWorktreeBase(clone, 5, 300)
	elapsed := time.Since(start)
	if baseRef != "origin/main" {
		t.Fatalf("base %q", baseRef)
	}
	if !strings.Contains(label, "cached") || !strings.Contains(label, "timed out") {
		t.Fatalf("label %q", label)
	}
	if len(*fetches) != 1 {
		t.Fatalf("timeout must not cascade into a second fetch: %v", *fetches)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("fallback path took %.1fs — must be fast", elapsed.Seconds())
	}
	if resolved, _ := wsbRun(t, clone, "git", "rev-parse", baseRef); resolved != staleLocalHead {
		t.Fatalf("resolved %s want %s", resolved, staleLocalHead)
	}
}

func TestResolveWorktreeBaseStartupCostFetchTimeoutWithoutCachedRefFallsBackToHead(t *testing.T) {
	repo := wsbPlainRepo(t, "broken-upstream")
	tmp := filepath.Dir(repo)
	wsbRun(t, repo, "git", "checkout", "-b", "main")
	wsbCommit(t, repo, "README.md", "base")
	wsbRun(t, repo, "git", "remote", "add", "origin", filepath.Join(tmp, "nonexistent.git"))
	wsbRun(t, repo, "git", "config", "branch.main.remote", "origin")
	wsbRun(t, repo, "git", "config", "branch.main.merge", "refs/heads/main")
	baseRef, label := resolveWorktreeBase(repo, 5, 300)
	if baseRef != "HEAD" || !strings.Contains(label, "HEAD") {
		t.Fatalf("base %q label %q", baseRef, label)
	}
}

func TestSetupWorktreeSyncBaseSyncTrueBranchesFromRemoteTip(t *testing.T) {
	clone, remoteHead, staleLocalHead := wsbRemoteAndClone(t)
	info := SetupWorktree(context.Background(), clone, true, "", nil)
	if info == nil {
		t.Fatal("setup failed")
	}
	wtHead := wsbHead(t, info["path"])
	if wtHead != remoteHead {
		t.Fatalf("worktree should start from the fetched remote tip: %s want %s", wtHead, remoteHead)
	}
	if wtHead == staleLocalHead {
		t.Fatal("worktree started from the stale local HEAD")
	}
	if _, err := os.Stat(filepath.Join(info["path"], "feature.txt")); err != nil {
		t.Fatalf("remote-only file missing: %v", err)
	}
}

func TestSetupWorktreeSyncBaseSyncFalseBranchesFromLocalHead(t *testing.T) {
	clone, _, staleLocalHead := wsbRemoteAndClone(t)
	info := SetupWorktree(context.Background(), clone, false, "", nil)
	if info == nil {
		t.Fatal("setup failed")
	}
	if wtHead := wsbHead(t, info["path"]); wtHead != staleLocalHead {
		t.Fatalf("worktree head %s want the local %s", wtHead, staleLocalHead)
	}
	if _, err := os.Stat(filepath.Join(info["path"], "feature.txt")); err == nil {
		t.Fatal("remote-only file present in a local HEAD worktree")
	}
}

func TestSetupWorktreeSyncBaseDefaultIsSyncTrue(t *testing.T) {
	clone, remoteHead, _ := wsbRemoteAndClone(t)
	info := SetupWorktree(context.Background(), clone, true, "", nil)
	if info == nil {
		t.Fatal("setup failed")
	}
	if wtHead := wsbHead(t, info["path"]); wtHead != remoteHead {
		t.Fatalf("worktree head %s want %s", wtHead, remoteHead)
	}
}
