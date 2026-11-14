package tools

import (
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Swaps the git runner and the negative TTL, invalidating the cache before and after
func swapTUIGit(t *testing.T, run func(string, ...string) string, negTTL time.Duration) {
	t.Helper()
	oldTTL := tuiGitNegTTL
	oldRun := setTUIRunGit(run)
	tuiGitNegTTL = negTTL
	GitProbeInvalidate()
	t.Cleanup(func() {
		setTUIRunGit(oldRun)
		tuiGitNegTTL = oldTTL
		GitProbeInvalidate()
	})
}

func TestRepoRootCacheDoesNotFreezeANotYetRepo(t *testing.T) {
	cwd := "/tmp/baby pics"
	var root atomic.Value
	root.Store("")
	swapTUIGit(t, func(c string, _ ...string) string {
		if c == cwd {
			return root.Load().(string)
		}
		return ""
	}, 0)
	if got := GitProbeRepoRoot(cwd); got != "" {
		t.Fatalf("pre init %q", got)
	}
	root.Store(cwd)
	if got := GitProbeRepoRoot(cwd); got != cwd {
		t.Fatalf("re-probe %q", got)
	}
	root.Store("")
	if got := GitProbeRepoRoot(cwd); got != cwd {
		t.Fatalf("cached %q", got)
	}
}

func TestRepoRootNegativeIsTrustedWithinTTL(t *testing.T) {
	var calls atomic.Int32
	swapTUIGit(t, func(string, ...string) string { calls.Add(1); return "" }, time.Hour)
	GitProbeRepoRoot("/x")
	GitProbeRepoRoot("/x")
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	GitProbeInvalidate()
	GitProbeRepoRoot("/x")
	if calls.Load() != 2 {
		t.Fatalf("after invalidate calls %d", calls.Load())
	}
}

func TestRootCacheIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	swapTUIGit(t, func(string, ...string) string {
		calls.Add(1)
		<-release
		return "/repo"
	}, time.Hour)
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Go(func() { results[i] = GitProbeRepoRoot("/repo/sub") })
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	for _, r := range results {
		if r != "/repo" {
			t.Fatalf("results %v", results)
		}
	}
}

func TestRootCacheLeaderPanicOpensTheGate(t *testing.T) {
	c := newTUIRootCache()
	func() {
		defer func() { recover() }()
		c.resolve("k", func() string { panic("boom") })
	}()
	if len(c.inflight) != 0 || c.neg["k"].IsZero() {
		t.Fatalf("inflight %v neg %v", c.inflight, c.neg)
	}
}

func TestGitProbeBranchFallsBackToShortHead(t *testing.T) {
	var seen [][]string
	swapTUIGit(t, func(_ string, args ...string) string {
		seen = append(seen, args)
		if args[0] == "rev-parse" {
			return "abc1234"
		}
		return ""
	}, time.Hour)
	if got := GitProbeBranch("/r"); got != "abc1234" || len(seen) != 2 || !slices.Equal(seen[0], []string{"branch", "--show-current"}) {
		t.Fatalf("%q %v", got, seen)
	}
}

func TestGitProbeResolveFoldsLinkedWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required")
	}
	GitProbeInvalidate()
	t.Cleanup(GitProbeInvalidate)
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(tmp, "main")
	cleanGit(t, "init", "-q", "-b", "trunk", main)
	writeFileT(t, filepath.Join(main, "f"), "x\n")
	cleanGit(t, "-C", main, "add", ".")
	cleanGit(t, "-C", main, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-qm", "init")
	wt := filepath.Join(tmp, "wt")
	cleanGit(t, "-C", main, "worktree", "add", "-q", "-b", "feature", wt)
	got := GitProbeResolve(wt)
	if got["repo_root"] != filepath.ToSlash(main) || got["worktree_root"] != filepath.ToSlash(wt) {
		t.Fatalf("worktree %v", got)
	}
	if got := GitProbeResolve(main); got["repo_root"] != got["worktree_root"] || got["repo_root"] != filepath.ToSlash(main) {
		t.Fatalf("main %v", got)
	}
	if b := GitProbeBranch(wt); b != "feature" {
		t.Fatalf("branch %q", b)
	}
	if GitProbeResolve(tmp) != nil || GitProbeCommonRepoRoot(tmp) != "" || GitProbeRepoRoot(filepath.Join(tmp, "gone")) != "" {
		t.Fatal("non repo resolved")
	}
	if GitProbeRunGit("") != "" || GitProbeRepoRoot("") != "" {
		t.Fatal("empty cwd probed")
	}
}

func TestGitProbeWarmRootsResolvesEveryDistinctCwd(t *testing.T) {
	var mu sync.Mutex
	var probed []string
	swapTUIGit(t, func(c string, args ...string) string {
		if args[0] == "rev-parse" && args[1] == "--show-toplevel" {
			mu.Lock()
			probed = append(probed, c)
			mu.Unlock()
		}
		return ""
	}, time.Hour)
	if err := GitProbeWarmRoots([]string{" /b ", "/a", "", "/b", "  "}, 8); err != nil {
		t.Fatal(err)
	}
	slices.Sort(probed)
	if !slices.Equal(probed, []string{"/a", "/b"}) {
		t.Fatalf("probed %v", probed)
	}
	if err := GitProbeWarmRoots([]string{"/c", "/d"}, 0); err == nil || err.Error() != "max_workers must be greater than 0" {
		t.Fatalf("err %v", err)
	}
	if err := GitProbeWarmRoots([]string{"/e"}, 0); err != nil {
		t.Fatalf("single %v", err)
	}
}

func TestBaselineUnhardenedGitFiresSinks(t *testing.T) {
	repo, marker := makeMaliciousRepo(t, t.TempDir())
	exec.Command("git", "-C", repo, "diff", "HEAD").Run()
	fired := firedSinks(marker)
	if !slices.Contains(fired, "fsmonitor") || !slices.Contains(fired, "extdiff") {
		t.Fatalf("fired %v", fired)
	}
}

func TestGatewayGitProbeIsSafe(t *testing.T) {
	repo, marker := makeMaliciousRepo(t, t.TempDir())
	GitProbeBranch(repo)
	GitProbeRunGit(repo, "status", "--porcelain")
	if fired := firedSinks(marker); len(fired) != 0 {
		t.Fatalf("fired %v", fired)
	}
}
