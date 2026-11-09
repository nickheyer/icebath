package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shGitResult(t *testing.T, cwd string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	rc := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("git %v: %v", args, err)
		}
		rc = exit.ExitCode()
	}
	return out.String(), errOut.String(), rc
}

func shGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	out, errOut, rc := shGitResult(t, cwd, "", args...)
	if rc != 0 {
		t.Fatalf("git %v exited %d: %s", args, rc, errOut)
	}
	return out
}

func shRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	root := filepath.Join(tmp, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	shGit(t, root, "init", "-q", "-b", "main")
	shGit(t, root, "config", "user.email", "t@t")
	shGit(t, root, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shGit(t, root, "add", "-A")
	shGit(t, root, "commit", "-qm", "init")
	return root
}

func shSimulateTimedOutAdd(t *testing.T, repo string) string {
	t.Helper()
	wt := filepath.Join(repo, ".worktrees", "icebath-dead00")
	shGit(t, repo, "worktree", "add", wt, "-b", "icebath/icebath-dead00")
	shGit(t, repo, "worktree", "lock", wt, "--reason", "icebath pid=999999")
	entries, err := os.ReadDir(wt)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			if err := os.Remove(filepath.Join(wt, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	return wt
}

func TestCleanupFailedWorktreeAddSweepsDirAdminEntryAndBranch(t *testing.T) {
	repo := shRepo(t)
	wt := shSimulateTimedOutAdd(t, repo)
	admin := filepath.Join(repo, ".git", "worktrees", "icebath-dead00")
	if !gcExists(admin) || !gcExists(filepath.Join(admin, "locked")) {
		t.Fatal("fixture: the admin entry is not locked")
	}
	cleanupFailedWorktreeAdd(repo, wt, "icebath/icebath-dead00")
	if gcExists(wt) {
		t.Fatal("partial worktree dir must be removed")
	}
	if gcExists(admin) {
		t.Fatal("LOCKED admin entry must be removed")
	}
	if branches := shGit(t, repo, "branch", "--list", "icebath/icebath-dead00"); strings.TrimSpace(branches) != "" {
		t.Fatalf("orphaned branch must be deleted: %q", branches)
	}
}

func TestCleanupFailedWorktreeAddRetrySucceedsAfterCleanup(t *testing.T) {
	repo := shRepo(t)
	wt := shSimulateTimedOutAdd(t, repo)
	cleanupFailedWorktreeAdd(repo, wt, "icebath/icebath-dead00")
	if _, errOut, rc := shGitResult(t, repo, "", "worktree", "add", wt, "-b", "icebath/icebath-dead00"); rc != 0 {
		t.Fatalf("retry failed: %s", errOut)
	}
}

func TestCleanupFailedWorktreeAddNoopWhenNothingExists(t *testing.T) {
	repo := shRepo(t)
	cleanupFailedWorktreeAdd(repo, filepath.Join(repo, ".worktrees", "never-existed"), "icebath/never-existed")
}

func shPackCount(t *testing.T, repo string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(repo, ".git", "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

func shMakePacks(t *testing.T, repo string, n int) int {
	t.Helper()
	packDir := filepath.Join(repo, ".git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("p%d.txt", i)), []byte(fmt.Sprintf("%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		shGit(t, repo, "add", "-A")
		shGit(t, repo, "commit", "-qm", fmt.Sprintf("c%d", i))
		sha := strings.TrimSpace(shGit(t, repo, "rev-parse", "HEAD^{commit}"))
		if _, errOut, rc := shGitResult(t, repo, sha+"\n", "pack-objects", "-q", filepath.Join(packDir, fmt.Sprintf("tpack%d", i))); rc != 0 {
			t.Fatalf("pack-objects: %s", errOut)
		}
	}
	return shPackCount(t, repo)
}

func shSetThreshold(t *testing.T, threshold int) {
	t.Helper()
	prev := packSprawlThreshold
	t.Cleanup(func() { packSprawlThreshold = prev })
	packSprawlThreshold = threshold
}

func TestMaintainPackHealthRepacksAtThreshold(t *testing.T) {
	repo := shRepo(t)
	made := shMakePacks(t, repo, 12)
	shSetThreshold(t, 2)
	if made <= 2 {
		t.Fatalf("fixture failed to produce sprawl (made=%d)", made)
	}
	maintainPackHealth(repo)
	after := shPackCount(t, repo)
	if after >= made {
		t.Fatalf("pack count must strictly decrease (made=%d, after=%d)", made, after)
	}
	if !gcExists(filepath.Join(repo, ".git", "objects", "pack", "multi-pack-index")) {
		t.Fatal("no multi-pack-index after the repack")
	}
}

func TestMaintainPackHealthNoopBelowThreshold(t *testing.T) {
	repo := shRepo(t)
	made := shMakePacks(t, repo, 2)
	shSetThreshold(t, 50)
	maintainPackHealth(repo)
	if got := shPackCount(t, repo); got != made {
		t.Fatalf("below threshold must be a no-op: %d packs, made %d", got, made)
	}
}

func TestMaintainPackHealthFailSoftOnMissingPackDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	maintainPackHealth(filepath.Join(tmp, "not-a-repo"))
}

func TestMaintainPackHealthPassRefreshesAStaleGraphInATreelessCloneWithoutFetching(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	src := filepath.Join(tmp, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	shGit(t, src, "init", "-q")
	shGit(t, src, "config", "uploadpack.allowFilter", "true")
	commits := func(start, n int) {
		for i := start; i < start+n; i++ {
			dir := filepath.Join(src, fmt.Sprintf("d%d", i%3))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), []byte(fmt.Sprintf("%d\n", i)), 0o644); err != nil {
				t.Fatal(err)
			}
			shGit(t, src, "add", "-A")
			shGit(t, src, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "-qm", fmt.Sprintf("c%d", i))
		}
	}
	commits(0, 8)
	clone := filepath.Join(tmp, "clone")
	shGit(t, tmp, "clone", "-q", "--filter=tree:0", "--no-checkout", "file://"+filepath.ToSlash(src), clone)
	shGit(t, clone, "commit-graph", "write", "--reachable", "--split", "--changed-paths", "--no-progress")
	commits(8, 8)
	shGit(t, clone, "-c", "fetch.writeCommitGraph=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "fetch", "-q", "origin")
	shGit(t, clone, "update-ref", "HEAD", "FETCH_HEAD")
	info := filepath.Join(clone, ".git", "objects", "info")
	old := time.Now().Add(-3 * 24 * time.Hour)
	graphs, err := os.ReadDir(filepath.Join(info, "commit-graphs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, graph := range graphs {
		if err := os.Chtimes(filepath.Join(info, "commit-graphs", graph.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	linked := filepath.Join(tmp, "linked")
	shGit(t, clone, "worktree", "add", "-q", "--no-checkout", "--detach", linked, "HEAD")
	packsBefore, _ := filepath.Glob(filepath.Join(clone, ".git", "objects", "pack", "*.pack"))
	maintainPackHealth(linked)
	chain, err := os.Stat(filepath.Join(info, "commit-graphs", "commit-graph-chain"))
	if err != nil {
		t.Fatal(err)
	}
	if chain.ModTime().Before(time.Now().Add(-300 * time.Second)) {
		t.Fatalf("commit-graph-chain not rewritten, mtime %s", chain.ModTime())
	}
	if st, err := os.Stat(filepath.Join(clone, ".git", "icebath-commit-graph.fresh")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("fresh stamp missing: %v", err)
	}
	packsAfter, _ := filepath.Glob(filepath.Join(clone, ".git", "objects", "pack", "*.pack"))
	if strings.Join(packsAfter, "\n") != strings.Join(packsBefore, "\n") {
		t.Fatalf("the write lazy-fetched: %v -> %v", packsBefore, packsAfter)
	}
	shGit(t, clone, "commit-graph", "verify")
}

func TestRepackStampedeOneRepackPerClonePerInterval(t *testing.T) {
	repo := shRepo(t)
	shSetThreshold(t, 0)
	prevRun, prevInterval := runBoundedRepackFn, repackMinInterval
	t.Cleanup(func() { runBoundedRepackFn, repackMinInterval = prevRun, prevInterval })
	var runs []string
	runBoundedRepackFn = func(root string) { runs = append(runs, root) }
	for i := 0; i < 3; i++ {
		maintainPackHealth(repo)
	}
	if len(runs) != 1 || runs[0] != repo {
		t.Fatalf("N launches inside the interval must produce exactly one repack: %v", runs)
	}
	repackMinInterval = 0
	maintainPackHealth(repo)
	if len(runs) != 2 {
		t.Fatalf("a stale stamp must hand the slot to the next launch: %v", runs)
	}
}
