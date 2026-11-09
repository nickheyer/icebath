package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func twGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return strings.TrimSpace(string(out))
}

func twWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func twAge(t *testing.T, path string, hours float64) {
	t.Helper()
	at := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func twHome(t *testing.T, tmp string) {
	t.Helper()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
}

func twRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	twHome(t, tmp)
	repo := filepath.Join(tmp, "test-repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	twGit(t, repo, "init")
	twGit(t, repo, "config", "user.email", "test@test.com")
	twGit(t, repo, "config", "user.name", "Test")
	twWrite(t, filepath.Join(repo, "README.md"), "# Test Repo\n")
	twGit(t, repo, "add", ".")
	twGit(t, repo, "commit", "-m", "Initial commit")
	twGit(t, repo, "remote", "add", "origin", "https://example.com/test-repo.git")
	twGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	return repo
}

func twRepoNoRemote(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	twHome(t, tmp)
	repo := filepath.Join(tmp, "test-repo-no-remote")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	twGit(t, repo, "init")
	twGit(t, repo, "config", "user.email", "test@test.com")
	twGit(t, repo, "config", "user.name", "Test")
	twWrite(t, filepath.Join(repo, "README.md"), "# Test Repo\n")
	twGit(t, repo, "add", ".")
	twGit(t, repo, "commit", "-m", "Initial commit")
	return repo
}

func twPrune(t *testing.T, repo string) {
	t.Helper()
	if err := PruneStaleWorktrees(context.Background(), repo, 24); err != nil {
		t.Fatalf("prune: %v", err)
	}
}

func twMkLocked(t *testing.T, repo, name string, pid int, dirty, unpushed bool, ageH float64) string {
	t.Helper()
	p := filepath.Join(repo, ".worktrees", name)
	_ = os.MkdirAll(filepath.Join(repo, ".worktrees"), 0o755)
	twGit(t, repo, "worktree", "add", p, "-b", "icebath/"+name, "HEAD")
	if pid != 0 {
		twGit(t, repo, "worktree", "lock", "--reason", fmt.Sprintf("icebath pid=%d", pid), p)
	}
	if unpushed {
		twWrite(t, filepath.Join(p, "work.txt"), "x")
		twGit(t, p, "add", "work.txt")
		twGit(t, p, "commit", "-m", "wip")
	}
	if dirty {
		twWrite(t, filepath.Join(p, "dirty.txt"), "uncommitted")
	}
	twAge(t, p, ageH)
	return p
}

func TestWorktreeLockReapingLiveLockedSurvivesAtAnyAge(t *testing.T) {
	repo := twRepo(t)
	wt := twMkLocked(t, repo, "icebath-live", os.Getpid(), false, false, 100)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("live-locked worktree (this pid) must never be reaped")
	}
}

func TestWorktreeLockReapingDeadLockedCleanIsReaped(t *testing.T) {
	repo := twRepo(t)
	wt := twMkLocked(t, repo, "icebath-dead", 999999, false, false, 100)
	if got := worktreeLockIsLive(repo, wt, 10*time.Second); got != "dead" {
		t.Fatalf("lock state %q", got)
	}
	twPrune(t, repo)
	if gcExists(wt) {
		t.Fatal("dead-locked clean worktree should be unlocked + reaped")
	}
}

func TestWorktreeLockReapingDeadLockedDirtySurvives(t *testing.T) {
	repo := twRepo(t)
	wt := twMkLocked(t, repo, "icebath-deaddirty", 999999, true, false, 100)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("dead-locked worktree with uncommitted work must survive")
	}
}

func TestWorktreeLockReapingDeadLockedUnpushedSurvives(t *testing.T) {
	repo := twRepo(t)
	wt := twMkLocked(t, repo, "icebath-deadunp", 999999, false, true, 100)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("dead-locked worktree with unpushed commits must survive")
	}
}

func TestWorktreeLockReapingUnlockedCleanStaleIsReaped(t *testing.T) {
	repo := twRepo(t)
	wt := twMkLocked(t, repo, "icebath-nolock", 0, false, false, 100)
	twPrune(t, repo)
	if gcExists(wt) {
		t.Fatal("clean unlocked stale worktree should be reaped")
	}
}

func TestWorktreeLockReapingDirtySurvivesOver72h(t *testing.T) {
	repo := twRepo(t)
	wt := twMkLocked(t, repo, "icebath-dirty72", 0, true, false, 100)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("dirty worktree must survive even past the 72h tier")
	}
}

func twMkLockedReason(t *testing.T, repo, name, reason string) string {
	t.Helper()
	p := filepath.Join(repo, ".worktrees", name)
	_ = os.MkdirAll(filepath.Join(repo, ".worktrees"), 0o755)
	twGit(t, repo, "worktree", "add", p, "-b", "icebath/"+name, "HEAD")
	twGit(t, repo, "worktree", "lock", "--reason", reason, p)
	return p
}

func TestWorktreeLockPredicateUnlockedReturnsNone(t *testing.T) {
	repo := twRepo(t)
	p := filepath.Join(repo, ".worktrees", "icebath-x")
	_ = os.MkdirAll(filepath.Join(repo, ".worktrees"), 0o755)
	twGit(t, repo, "worktree", "add", p, "-b", "icebath/icebath-x", "HEAD")
	if got := worktreeLockIsLive(repo, p, 10*time.Second); got != "" {
		t.Fatalf("lock state %q", got)
	}
}

func TestWorktreeLockPredicateForeignLockReasonReturnsDead(t *testing.T) {
	repo := twRepo(t)
	p := twMkLockedReason(t, repo, "icebath-foreign", "some other tool")
	if got := worktreeLockIsLive(repo, p, 10*time.Second); got != "dead" {
		t.Fatalf("lock state %q", got)
	}
}

func TestWorktreeLockPredicateUnparseableHermesLockReasonFailsSafeToLive(t *testing.T) {
	repo := twRepo(t)
	p := twMkLockedReason(t, repo, "icebath-future", "icebath owner=v2 pid=12x")
	if got := worktreeLockIsLive(repo, p, 10*time.Second); got != "live" {
		t.Fatalf("lock state %q", got)
	}
}

func TestWorktreeLockPredicateBadRepoRootFailsSafeToLive(t *testing.T) {
	tmp := t.TempDir()
	twHome(t, tmp)
	if got := worktreeLockIsLive(tmp, filepath.Join(tmp, "x"), 10*time.Second); got != "live" {
		t.Fatalf("lock state %q", got)
	}
}

func twMkTree(t *testing.T, repo, name string, commit, dirty bool, ageH float64) (string, string) {
	t.Helper()
	p := filepath.Join(repo, ".worktrees", name)
	_ = os.MkdirAll(filepath.Join(repo, ".worktrees"), 0o755)
	twGit(t, repo, "worktree", "add", p, "-b", "wt/"+name, "HEAD")
	sha := ""
	if commit {
		twWrite(t, filepath.Join(p, "work.txt"), fmt.Sprintf("work for %s\n", name))
		twGit(t, p, "add", "work.txt")
		twGit(t, p, "commit", "-m", "wip")
		sha = twGit(t, p, "rev-parse", "HEAD")
	}
	if dirty {
		twWrite(t, filepath.Join(p, "dirty.txt"), "uncommitted")
	}
	twAge(t, p, ageH)
	return p, sha
}

func twMergeUpstream(t *testing.T, repo, sha string) {
	t.Helper()
	twGit(t, repo, "-c", "user.email=merger@test.com", "-c", "user.name=Merger", "cherry-pick", sha)
	newHead := twGit(t, repo, "rev-parse", "HEAD")
	if newHead == sha {
		t.Fatal("cherry-pick landed the same sha")
	}
	twGit(t, repo, "update-ref", "refs/remotes/origin/main", newHead)
}

func TestWidenedPrunerNamedCleanStaleTreeIsReaped(t *testing.T) {
	repo := twRepo(t)
	wt, _ := twMkTree(t, repo, "salvage-12345", false, false, 80)
	twPrune(t, repo)
	if gcExists(wt) {
		t.Fatal("clean named tree past 72h soft tier should be reaped")
	}
}

func TestWidenedPrunerNamedTreeGets3xGrace(t *testing.T) {
	repo := twRepo(t)
	wt, _ := twMkTree(t, repo, "salvage-fresh", false, false, 48)
	twPrune(t, repo)
	if !gcExists(wt) {
		t.Fatal("named tree under 72h must be kept (3x scratch timeline)")
	}
}

func TestWidenedPrunerSquashMergedTreeIsReaped(t *testing.T) {
	repo := twRepo(t)
	wt, sha := twMkTree(t, repo, "icebath-merged", true, false, 100)
	twMergeUpstream(t, repo, sha)
	if !worktreeHasUnpushedCommits(wt, 10*time.Second) {
		t.Fatal("precondition: commit unreachable from remotes (the leak this fixes)")
	}
	twPrune(t, repo)
	if gcExists(wt) {
		t.Fatal("worktree whose commits are all patch-equivalent upstream is merged work and should be reaped")
	}
}

func TestWidenedPrunerMergedPredicateFailsSafeWithoutUpstream(t *testing.T) {
	repo := twRepoNoRemote(t)
	p := filepath.Join(repo, ".worktrees", "icebath-noremote")
	_ = os.MkdirAll(filepath.Join(repo, ".worktrees"), 0o755)
	twGit(t, repo, "worktree", "add", p, "-b", "wt/noremote", "HEAD")
	if !worktreeCommitsAllMergedUpstream(p, 30*time.Second, 20, nil) {
		t.Fatal("a tree at the local trunk is merged")
	}
	trunk := twGit(t, repo, "branch", "--show-current")
	twGit(t, repo, "checkout", "-q", "--detach")
	twGit(t, repo, "branch", "-m", trunk, "scratch/not-a-trunk")
	if base, _ := worktreeMergeBaseRef(p, 10*time.Second); base != "" {
		t.Fatalf("merge base %q without a trunk", base)
	}
	if worktreeCommitsAllMergedUpstream(p, 30*time.Second, 20, nil) {
		t.Fatal("nothing to compare against must not count as merged")
	}
	if !worktreeHasUnpushedCommits(p, 10*time.Second) {
		t.Fatal("nothing to compare against must count as unpushed")
	}
}

func TestMergeVerdictCacheCacheHitMatchesUncachedVerdict(t *testing.T) {
	repo := twRepo(t)
	wt, sha := twMkTree(t, repo, "icebath-cachehit", true, false, 100)
	twMergeUpstream(t, repo, sha)
	uncached := worktreeCommitsAllMergedUpstream(wt, 30*time.Second, 20, nil)
	cache := newMergeVerdicts()
	cold := worktreeCommitsAllMergedUpstream(wt, 30*time.Second, 20, cache)
	warm := worktreeCommitsAllMergedUpstream(wt, 30*time.Second, 20, cache)
	if len(cache.keys) == 0 {
		t.Fatal("verdict should have been memoized")
	}
	if !uncached || !cold || !warm {
		t.Fatalf("uncached %v cold %v warm %v", uncached, cold, warm)
	}
}

func TestMergeVerdictCacheNewCommitInvalidatesCachedVerdict(t *testing.T) {
	repo := twRepo(t)
	wt, sha := twMkTree(t, repo, "icebath-moves", true, false, 100)
	twMergeUpstream(t, repo, sha)
	cache := newMergeVerdicts()
	if !worktreeCommitsAllMergedUpstream(wt, 30*time.Second, 20, cache) {
		t.Fatal("merged tree must read as merged")
	}
	keyAfterMerge := strings.Join(append([]string{}, cache.keys...), "\n")
	twWrite(t, filepath.Join(wt, "more.txt"), "unmerged work\n")
	twGit(t, wt, "add", "more.txt")
	twGit(t, wt, "commit", "-m", "new work")
	if worktreeCommitsAllMergedUpstream(wt, 30*time.Second, 20, cache) {
		t.Fatal("new local-only work must not read as merged")
	}
	if strings.Join(cache.keys, "\n") == keyAfterMerge {
		t.Fatal("moved HEAD must produce a new key")
	}
}

func TestMergeVerdictCacheCacheIsBounded(t *testing.T) {
	tmp := t.TempDir()
	twHome(t, tmp)
	path := filepath.Join(tmp, "verdicts.json")
	prevPath, prevMax := worktreeMergeCachePath, worktreeMergeCacheMax
	t.Cleanup(func() { worktreeMergeCachePath, worktreeMergeCacheMax = prevPath, prevMax })
	worktreeMergeCachePath = func() string { return path }
	worktreeMergeCacheMax = 10
	verdicts := newMergeVerdicts()
	for i := 0; i < 50; i++ {
		verdicts.set(fmt.Sprintf("sha%d..sha%d:20", i, i), true)
	}
	saveWorktreeMergeCache(verdicts)
	if got := len(loadWorktreeMergeCache().keys); got != 10 {
		t.Fatalf("cache holds %d verdicts", got)
	}
}

func twBoard(t *testing.T, repo, tag string) map[string]string {
	t.Helper()
	names := map[string]string{}
	for i := 0; i < 3; i++ {
		n := fmt.Sprintf("icebath-merged%s%d", tag, i)
		wt, sha := twMkTree(t, repo, n, true, false, 100)
		twMergeUpstream(t, repo, sha)
		names[n] = wt
	}
	for i := 0; i < 3; i++ {
		n := fmt.Sprintf("icebath-unpushed%s%d", tag, i)
		wt, _ := twMkTree(t, repo, n, true, false, 100)
		names[n] = wt
	}
	for i := 0; i < 2; i++ {
		n := fmt.Sprintf("icebath-dirty%s%d", tag, i)
		wt, _ := twMkTree(t, repo, n, false, true, 100)
		names[n] = wt
	}
	n := "icebath-fresh" + tag
	wt, _ := twMkTree(t, repo, n, true, false, 1)
	names[n] = wt
	for name, p := range names {
		if !gcExists(p) {
			t.Fatalf("fixture worktree %s was not created", name)
		}
	}
	return names
}

func twKinds(board map[string]string) string {
	kinds := map[string]bool{}
	for n, p := range board {
		if !gcExists(p) {
			continue
		}
		for _, kind := range []string{"merged", "unpushed", "dirty", "fresh"} {
			if strings.HasPrefix(n, "icebath-"+kind) {
				kinds[kind] = true
			}
		}
	}
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func twSetCPUCount(t *testing.T, n int) {
	t.Helper()
	prev := worktreeCPUCount
	t.Cleanup(func() { worktreeCPUCount = prev })
	worktreeCPUCount = func() int { return n }
}

func TestPruneParallelEquivalenceSingleAndMultiWorkerAgree(t *testing.T) {
	repo := twRepo(t)
	board := twBoard(t, repo, "a")
	twSetCPUCount(t, 1)
	twPrune(t, repo)
	serial := twKinds(board)
	_ = os.Remove(worktreeMergeCachePath())
	board2 := twBoard(t, repo, "b")
	twSetCPUCount(t, 8)
	twPrune(t, repo)
	parallel := twKinds(board2)
	if parallel != serial {
		t.Fatalf("pool width changed the outcome: serial=%s parallel=%s", serial, parallel)
	}
	if serial != "dirty,fresh,unpushed" {
		t.Fatalf("survivors %s", serial)
	}
}

func TestPruneParallelEquivalencePoolFailureFallsBackToSerial(t *testing.T) {
	repo := twRepo(t)
	wt, sha := twMkTree(t, repo, "icebath-poolfail", true, false, 100)
	twMergeUpstream(t, repo, sha)
	twSetCPUCount(t, 8)
	twPrune(t, repo)
	if gcExists(wt) {
		t.Fatal("serial fallback must still reap the merged tree")
	}
}

func scUpstream(t *testing.T, tmp string) string {
	t.Helper()
	up := filepath.Join(tmp, "upstream")
	if err := os.Mkdir(up, 0o755); err != nil {
		t.Fatal(err)
	}
	twGit(t, up, "init", "-b", "main")
	twGit(t, up, "config", "user.email", "test@test.com")
	twGit(t, up, "config", "user.name", "Test")
	twWrite(t, filepath.Join(up, "README.md"), "# upstream\n")
	twGit(t, up, "add", ".")
	twGit(t, up, "commit", "-m", "A")
	return up
}

func scAdvanceUpstream(t *testing.T, up, name string) {
	t.Helper()
	twWrite(t, filepath.Join(up, name+".txt"), name+"\n")
	twGit(t, up, "add", ".")
	twGit(t, up, "commit", "-m", name)
}

func scShallowClone(t *testing.T, tmp, up string) string {
	t.Helper()
	clone := filepath.Join(tmp, "shallow-clone")
	twGit(t, tmp, "clone", "--depth", "1", "file://"+up, clone)
	twGit(t, clone, "config", "user.email", "test@test.com")
	twGit(t, clone, "config", "user.name", "Test")
	return clone
}

func scStuckWorktree(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	twHome(t, tmp)
	up := scUpstream(t, tmp)
	clone := scShallowClone(t, tmp, up)
	if !repoIsShallow(clone, 5*time.Second) {
		t.Fatal("fixture must start shallow")
	}
	wt := filepath.Join(clone, ".worktrees", "icebath-shallowstuck")
	if err := os.Mkdir(filepath.Join(clone, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	twGit(t, clone, "worktree", "add", wt, "-b", "icebath/icebath-shallowstuck", "HEAD")
	scAdvanceUpstream(t, up, "B")
	twGit(t, clone, "fetch", "--depth", "1", "origin", "main")
	twGit(t, clone, "update-ref", "refs/remotes/origin/main", "FETCH_HEAD")
	twAge(t, wt, 100)
	return up, clone, wt
}

func TestShallowCloneDeepeningShallowDisconnectReproducesFalseUnpushed(t *testing.T) {
	_, _, wt := scStuckWorktree(t)
	if !worktreeHasUnpushedCommits(wt, 10*time.Second) {
		t.Fatal("expected the shallow disconnect to look like unpushed commits — if this stops reproducing, the fixture no longer exercises the bug")
	}
}

func TestShallowCloneDeepeningRepoIsShallowDetection(t *testing.T) {
	gitRepo := twRepo(t)
	tmp := filepath.Dir(gitRepo)
	up := scUpstream(t, tmp)
	clone := scShallowClone(t, tmp, up)
	if !repoIsShallow(clone, 5*time.Second) {
		t.Fatal("shallow clone not detected")
	}
	if repoIsShallow(gitRepo, 5*time.Second) {
		t.Fatal("full repo reported shallow")
	}
	if repoIsShallow(filepath.Join(tmp, "nonexistent"), 5*time.Second) {
		t.Fatal("missing path reported shallow")
	}
}

func TestShallowCloneDeepeningDeepenConnectsHistoryAndClearsFalseUnpushed(t *testing.T) {
	up, clone, wt := scStuckWorktree(t)
	if !worktreeHasUnpushedCommits(wt, 10*time.Second) {
		t.Fatal("fixture: the stuck tree must read as unpushed first")
	}
	twGit(t, up, "config", "uploadpack.allowFilter", "true")
	if !deepenShallowRepo(clone, 600*time.Second) {
		t.Fatal("deepen failed")
	}
	if repoIsShallow(clone, 5*time.Second) {
		t.Fatal("clone still shallow after deepening")
	}
	if got := twGit(t, clone, "config", "--get", "remote.origin.promisor"); got != "true" {
		t.Fatalf("remote.origin.promisor %q", got)
	}
	packs, _ := filepath.Glob(filepath.Join(clone, ".git", "objects", "pack", "pack-*.pack"))
	if len(packs) == 0 {
		t.Fatal("no packs after deepening")
	}
	for _, p := range packs {
		if !gcExists(strings.TrimSuffix(p, ".pack") + ".promisor") {
			t.Fatalf("pack without a promisor marker: %s", p)
		}
	}
	if worktreeHasUnpushedCommits(wt, 10*time.Second) {
		t.Fatal("after deepening, the worktree's HEAD is an ancestor of origin/main and must no longer count as unpushed")
	}
}

func TestShallowCloneDeepeningPrunerDeepensAndReapsStuckWorktree(t *testing.T) {
	_, clone, wt := scStuckWorktree(t)
	twPrune(t, clone)
	if repoIsShallow(clone, 5*time.Second) {
		t.Fatal("pruner should deepen")
	}
	if gcExists(wt) {
		t.Fatal("deepened history proves the tree is merged/public — reap it")
	}
}

func TestShallowCloneDeepeningDeepenOfflineFailsSoftAndPreserves(t *testing.T) {
	_, clone, wt := scStuckWorktree(t)
	tmp := filepath.Dir(clone)
	twGit(t, clone, "remote", "set-url", "origin", "file://"+filepath.Join(tmp, "gone"))
	if deepenShallowRepo(clone, 30*time.Second) {
		t.Fatal("deepen against a missing remote must report failure")
	}
	twPrune(t, clone)
	if !gcExists(wt) {
		t.Fatal("offline deepen failure must fall back to preserving the tree")
	}
}

func TestShallowCloneDeepeningDeepenNoopOnFullClone(t *testing.T) {
	repo := twRepo(t)
	if !deepenShallowRepo(repo, 600*time.Second) {
		t.Fatal("a full clone must deepen as a no-op success")
	}
}

func TestShallowCloneDeepeningRealUnpushedWorkSurvivesDeepening(t *testing.T) {
	_, clone, wt := scStuckWorktree(t)
	twWrite(t, filepath.Join(wt, "real-work.txt"), "novel\n")
	twGit(t, wt, "add", "real-work.txt")
	twGit(t, wt, "commit", "-m", "real unpushed work")
	twAge(t, wt, 100)
	twPrune(t, clone)
	if !gcExists(wt) {
		t.Fatal("genuinely unpushed commit must survive even after deepening")
	}
}
