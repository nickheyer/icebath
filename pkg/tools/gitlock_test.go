package tools

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Installs a plain git runner as the updater runner and the serve obligation hooks the process wiring assigns
func installGitlockDoubles() {
	GitlockRunGit = func(ctx context.Context, gitCmd, args []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		return gitlockSubprocessRun(ctx, append(slices.Clone(gitCmd), args...), o)
	}
	isReminder := func(row any) bool {
		m, ok := row.(map[string]any)
		return ok && (m["kind"] == "serve" || m["kind"] == "dashboard") && m["supervisor"] == "manual-serve"
	}
	rows := func(v any) []any {
		items, _ := v.([]any)
		return items
	}
	updaterecord.SetServeObligationHooks(updaterecord.ServeObligationHooks{
		IsReminderRow: isReminder,
		RetainReceiptManualServes: func(_ context.Context, receipt map[string]any) []any {
			pending := []any{}
			plan, _ := receipt["plan"].(map[string]any)
			for _, row := range slices.Concat(rows(plan["runtimes"]), rows(receipt["pending_manual_serves"]), rows(receipt["carried_manual_serves"])) {
				if isReminder(row) {
					pending = append(pending, row)
				}
			}
			return pending
		},
	})
}

// Environment of the fixture git calls, global and system config ignored
func glEnv(extra ...string) []string {
	return append(append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1"), extra...)
}

// Runs git in dir with a fixture identity and returns the completed process
func glGitRC(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	cmd.Dir, cmd.Env = dir, glEnv()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out.String()), errOut.String(), cmd.ProcessState.ExitCode()
}

// Runs git in dir and fails the test on a nonzero exit
func glGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, errOut, rc := glGitRC(t, dir, args...)
	if rc != 0 {
		t.Fatalf("git %v exited %d: %s", args, rc, errOut)
	}
	return out
}

// Creates or truncates path and backdates its mtime by age
func glTouch(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// A tiny two commit repository without detached maintenance
func glRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(root, 0o755)
	glGit(t, root, "init", "-q")
	quiet := []string{"-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "-q", "-m"}
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o644)
	glGit(t, root, "add", "a.txt")
	glGit(t, root, append(quiet, "first")...)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("two\n"), 0o644)
	glGit(t, root, "add", "b.txt")
	glGit(t, root, append(quiet, "second")...)
	return root
}

// Pins the git process guard to answer running
func glGuard(t *testing.T, running bool) {
	t.Helper()
	prev := checkpointGitProcRunning
	checkpointGitProcRunning = func(*Env) bool { return running }
	t.Cleanup(func() { checkpointGitProcRunning = prev })
}

// Thread safe log sink for the gitlock records
type glLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *glLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *glLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Captures the default logger at debug level for the test
func glCaptureLog(t *testing.T) *glLogBuf {
	t.Helper()
	buf := &glLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

const glStale = (StaleLockMinAgeSeconds + 60) * time.Second

func TestClearRemovesStaleShallowLock(t *testing.T) {
	repo := glRepo(t)
	glGuard(t, false)
	lock := filepath.Join(repo, ".git", "shallow.lock")
	glTouch(t, lock, glStale)
	if removed := ClearStaleGitLocks(repo, nil); !slices.Contains(removed, lock) || pathExists(lock) {
		t.Fatalf("removed %v", removed)
	}
}

func TestClearRemovesAllStaleLockKinds(t *testing.T) {
	repo := glRepo(t)
	glGuard(t, false)
	for _, name := range LockNames {
		glTouch(t, filepath.Join(repo, ".git", name), glStale)
	}
	if removed := ClearStaleGitLocks(repo, nil); len(removed) != len(LockNames) {
		t.Fatalf("removed %v", removed)
	}
	for _, name := range LockNames {
		if pathExists(filepath.Join(repo, ".git", name)) {
			t.Fatal(name)
		}
	}
}

// The gitlock half of the graph refresh case, the debris sweep and both guard refusals
func TestGraphRefreshClearsObjectStoreDebrisAndLeavesTheIndexLock(t *testing.T) {
	repo := glRepo(t)
	gitDir := filepath.Join(repo, ".git")
	var debris []string
	for _, rel := range []string{"info/commit-graph.lock", "info/commit-graphs/commit-graph-chain.lock", "info/commit-graphs/tmp_graph_x1",
		"pack/multi-pack-index.lock", "pack/multi-pack-index.d/multi-pack-index-chain.lock"} {
		debris = append(debris, filepath.Join(gitDir, "objects", filepath.FromSlash(rel)))
	}
	glGuard(t, false)
	for _, p := range append(slices.Clone(debris), filepath.Join(gitDir, "index.lock")) {
		glTouch(t, p, glStale)
	}
	if err := refreshCommitGraph(context.Background(), repo, gitDir); err != nil {
		t.Fatal(err)
	}
	for _, p := range debris {
		if pathExists(p) {
			t.Fatal(p)
		}
	}
	if !pathExists(filepath.Join(gitDir, "index.lock")) {
		t.Fatal("index.lock removed")
	}
	glGit(t, repo, "commit-graph", "verify")
	if got := ClearStaleGitLocks(repo, nil); !slices.Equal(got, []string{filepath.Join(gitDir, "index.lock")}) {
		t.Fatalf("index lock sweep %v", got)
	}
	checkpointGitProcRunning = gitProcRunning
	lock := debris[1]
	execPath := glGit(t, repo, "--exec-path")
	dashed, _ := filepath.Glob(filepath.Join(execPath, "git-cat-file*"))
	if len(dashed) == 0 {
		t.Skip("this git ships no dashed executables")
	}
	blind := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(blind, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"git", "nice"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(blind, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stamp := filepath.Join(gitDir, commitGraphFreshStamp)
	for _, tc := range []string{"dashed git running", "no process list"} {
		glGit(t, repo, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "-q", "--allow-empty", "-m", tc)
		glTouch(t, lock, glStale)
		os.Remove(stamp)
		if tc == "dashed git running" {
			live := exec.Command(dashed[0], "--batch")
			stdin, _ := live.StdinPipe()
			if err := live.Start(); err != nil {
				t.Fatal(err)
			}
			refreshCommitGraph(context.Background(), repo, gitDir)
			stdin.Close()
			live.Wait()
		} else {
			prevPsutil := gitlockPsutil
			gitlockPsutil = false
			t.Cleanup(func() { gitlockPsutil = prevPsutil })
			t.Setenv("PATH", blind)
			refreshCommitGraph(context.Background(), repo, gitDir)
		}
		if !pathExists(lock) {
			t.Fatalf("%s: lock removed", tc)
		}
		if pathExists(stamp) {
			t.Fatalf("%s: fresh stamp written", tc)
		}
	}
}

func TestClearSkipsSweepWhileGitRunning(t *testing.T) {
	repo := glRepo(t)
	glGuard(t, true)
	lock := filepath.Join(repo, ".git", "shallow.lock")
	glTouch(t, lock, glStale)
	if removed := ClearStaleGitLocks(repo, nil); len(removed) != 0 || !pathExists(lock) {
		t.Fatalf("removed %v", removed)
	}
}

func TestClearKeepsYoungLock(t *testing.T) {
	repo := glRepo(t)
	glGuard(t, false)
	lock := filepath.Join(repo, ".git", "shallow.lock")
	glTouch(t, lock, time.Second)
	if removed := ClearStaleGitLocks(repo, nil); len(removed) != 0 || !pathExists(lock) {
		t.Fatalf("removed %v", removed)
	}
}

func TestClearNoopOnNonRepo(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "not-a-repo")
	os.MkdirAll(bare, 0o755)
	if got := ClearStaleGitLocks(bare, nil); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestClearNoopWithNoLocks(t *testing.T) {
	if got := ClearStaleGitLocks(glRepo(t), nil); len(got) != 0 {
		t.Fatal(got)
	}
}

const glCrashPosix = "remote: Enumerating objects: 12, done.\n" +
	"BUG: builtin/pack-objects.c:4842: should_include_obj should only be called on existing objects\n" +
	"error: pack-objects died of signal 6\n" +
	"fatal: could not finish pack-objects to repack local links\n" +
	"fatal: index-pack failed\n"

const glCrashWindows = "remote: Enumerating objects: 12, done.\n" +
	"BUG: builtin/pack-objects.c:4967: should_include_obj should only be called on existing objects\n" +
	"fatal: could not finish pack-objects to repack local links\n" +
	"fatal: index-pack failed\n"

const glCrashGit255 = "BUG: builtin/pack-objects.c:5004: should_include_obj should only be called on existing objects\n" +
	"fatal: fetch-pack: invalid index-pack output\n"

func TestCrashRecognizerRejectsUnrelatedFailures(t *testing.T) {
	for _, s := range []string{
		"fatal: Authentication failed for 'https://github.com/example.git'",
		"error: pack-objects died of signal 6",
		"BUG: builtin/pack-objects.c:4842: should_include_obj should only be called on existing objects\nfatal: index-pack failed\n",
		"fatal: fetch-pack: invalid index-pack output\n",
		"",
	} {
		if IsPartialClonePackObjectsCrash(s) {
			t.Fatalf("recognized %q", s)
		}
	}
}

func TestRecoveryMarksUnmarkedPacksAndRetriesTheSameFetch(t *testing.T) {
	for name, crash := range map[string]string{"posix": glCrashPosix, "windows": glCrashWindows, "git-255": glCrashGit255} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			packDir := filepath.Join(root, ".git", "objects", "pack")
			os.MkdirAll(packDir, 0o755)
			for _, f := range []string{"pack-local.pack", "pack-fetched.pack", "pack-fetched.promisor"} {
				os.WriteFile(filepath.Join(packDir, f), nil, 0o644)
			}
			var calls [][2][]string
			runner := func(gitCmd, args []string) (*pysubprocess.Completed, error) {
				calls = append(calls, [2][]string{slices.Clone(gitCmd), slices.Clone(args)})
				if len(calls) == 1 {
					return &pysubprocess.Completed{Args: append(gitCmd, args...), Returncode: 1, Stderr: []byte(crash)}, nil
				}
				return &pysubprocess.Completed{Args: append(gitCmd, args...)}, nil
			}
			res, err := FetchWithPartialCloneRecovery(runner, []string{"git"}, []string{"fetch", "origin", "main"}, root)
			if err != nil || res.Returncode != 0 || len(calls) != 2 {
				t.Fatalf("%v %v %v", res, err, calls)
			}
			for _, c := range calls {
				if !slices.Equal(c[0], []string{"git"}) || !slices.Equal(c[1], []string{"fetch", "origin", "main"}) {
					t.Fatalf("call %v", c)
				}
			}
			if !pathExists(filepath.Join(packDir, "pack-local.promisor")) {
				t.Fatal("unmarked pack not marked")
			}
		})
	}
}

// Packs in a checkout's pack directory
func glPacks(repo string) []string {
	packs, _ := filepath.Glob(filepath.Join(repo, ".git", "objects", "pack", "pack-*.pack"))
	return packs
}

// Seed, upstream and a tree:0 clone of an upstream with the given number of commits
func glBloblessClone(t *testing.T, dir string, commits int) (string, string, string) {
	t.Helper()
	seed, up, clone := filepath.Join(dir, "seed"), filepath.Join(dir, "up.git"), filepath.Join(dir, "clone")
	os.MkdirAll(dir, 0o755)
	glGit(t, dir, "init", "-q", "-b", "main", seed)
	for i := range commits {
		sub := filepath.Join(seed, "d"+string(rune('0'+i%3)))
		os.MkdirAll(sub, 0o755)
		os.WriteFile(filepath.Join(sub, "f.txt"), []byte("v"+strconv.Itoa(i)+"\n"), 0o644)
		glGit(t, seed, "add", "-A")
		glGit(t, seed, "commit", "-qm", "c"+strconv.Itoa(i))
	}
	glGit(t, dir, "clone", "-q", "--bare", seed, up)
	glGit(t, up, "config", "uploadpack.allowFilter", "true")
	glGit(t, up, "config", "uploadpack.allowAnySHA1InWant", "true")
	glGit(t, dir, "clone", "-q", "--filter=tree:0", "--no-checkout", "file://"+filepath.ToSlash(up), clone)
	return seed, up, clone
}

// Blobless clone that lazy fetched one pack per object it was asked for
func glPartialClone(t *testing.T) string {
	_, _, clone := glBloblessClone(t, t.TempDir(), 3)
	glGit(t, clone, "config", "gc.autoPackLimit", "2")
	glGit(t, clone, "config", "maintenance.auto", "false")
	for _, name := range []string{"d0/f.txt", "d1/f.txt", "d2/f.txt"} {
		glGit(t, clone, "cat-file", "-p", glGit(t, clone, "rev-parse", "HEAD:"+name))
	}
	return clone
}

func TestGitsOwnGCDoesNotLazyFetchTheTreesOfCommitsABloomGraphHasNotSeen(t *testing.T) {
	dir := t.TempDir()
	seed, up, clone := glBloblessClone(t, dir, 12)
	full := filepath.Join(dir, "full")
	glGit(t, dir, "clone", "-q", "file://"+filepath.ToSlash(up), full)
	glGit(t, full, "commit-graph", "write", "--reachable", "--changed-paths")
	graph := filepath.Join(clone, ".git", "objects", "info")
	entries, _ := filepath.Glob(filepath.Join(full, ".git", "objects", "info", "commit-graph*"))
	for _, e := range entries {
		dst := filepath.Join(graph, filepath.Base(e))
		var err error
		if isDirFollow(e) {
			err = os.CopyFS(dst, os.DirFS(e))
		} else if raw, rerr := os.ReadFile(e); rerr == nil {
			err = os.WriteFile(dst, raw, 0o644)
		} else {
			err = rerr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		os.WriteFile(filepath.Join(seed, "new.txt"), []byte("n"+strconv.Itoa(i)+"\n"), 0o644)
		glGit(t, seed, "add", "-A")
		glGit(t, seed, "commit", "-qm", "n"+strconv.Itoa(i))
	}
	glGit(t, seed, "push", "-q", up, "main")
	glGit(t, clone, "-c", "maintenance.auto=false", "fetch", "-q", "origin")
	glGit(t, clone, "-c", "maintenance.auto=false", "log", "-p")
	glGit(t, clone, "config", "gc.autoPackLimit", "3")
	if n := len(glPacks(clone)); n <= 3 {
		t.Fatalf("premise: %d packs", n)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	SettlePartialCloneMaintenance(context.Background(), clone)
	glGit(t, clone, "-c", "gc.autoDetach=false", "gc", "--auto")
	if n := len(glPacks(clone)); n != 1 {
		t.Fatalf("%d packs after gc", n)
	}
}

func TestMaintenanceKeysLeaveGitsOwnFoldRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	lazyPacks := func(name string, keys bool) int {
		_, _, clone := glBloblessClone(t, filepath.Join(dir, name), 3)
		for _, kv := range [][2]string{{"gc.autoPackLimit", "2"}, {"gc.autoDetach", "false"}, {"maintenance.autoDetach", "false"}} {
			glGit(t, clone, "config", kv[0], kv[1])
		}
		if keys {
			for _, key := range []string{"maintenance.auto", "gc.writeCommitGraph", "fetch.writeCommitGraph"} {
				glGit(t, clone, "config", key, "false")
			}
			DisableTree0AutoMaintenance(context.Background(), clone)
		}
		for _, p := range []string{"d0/f.txt", "d1/f.txt", "d2/f.txt"} {
			glGit(t, clone, "cat-file", "-p", glGit(t, clone, "rev-parse", "HEAD:"+p))
		}
		return len(glPacks(clone))
	}
	if keyed, stock := lazyPacks("keys", true), lazyPacks("stock", false); keyed > stock {
		t.Fatalf("keys %d stock %d", keyed, stock)
	}
}

func TestAnOperatorsOwnMaintenanceAutoFalseIsNeverErased(t *testing.T) {
	repo := glRepo(t)
	glGit(t, repo, "config", "maintenance.auto", "false")
	DisableTree0AutoMaintenance(context.Background(), repo)
	DisableTree0AutoMaintenance(context.Background(), repo)
	if got := glGit(t, repo, "config", "--local", "--get", "maintenance.auto"); got != "false" {
		t.Fatal(got)
	}
}

func TestACheckoutTheFirstCutConfiguredFoldsAgain(t *testing.T) {
	clone := glPartialClone(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	glGit(t, clone, "config", "gc.auto", "0")
	glGit(t, clone, "config", "fetch.writeCommitGraph", "false")
	if n := len(glPacks(clone)); n <= 2 {
		t.Fatalf("premise: %d packs", n)
	}
	SettlePartialCloneMaintenance(context.Background(), clone)
	glGit(t, clone, "-c", "gc.autoDetach=false", "gc", "--auto")
	if n := len(glPacks(clone)); n != 1 {
		t.Fatalf("%d packs", n)
	}
	if left, _, _ := glGitRC(t, clone, "config", "--local", "--get-regexp", `^(maintenance\.auto|gc\.auto)$`); left != "" {
		t.Fatalf("left %q", left)
	}
}

func TestNonPartialCheckoutIsLeftAlone(t *testing.T) {
	repo := glRepo(t)
	SettlePartialCloneMaintenance(context.Background(), repo)
	if keys, _, _ := glGitRC(t, repo, "config", "--local", "--get-regexp", "maintenance|writecommitgraph"); keys != "" {
		t.Fatalf("a full clone keeps git's stock maintenance: %q", keys)
	}
}

// Checkout skeleton with an empty pack directory
func glPackRepo(t *testing.T) (string, string) {
	root := t.TempDir()
	pack := filepath.Join(root, ".git", "objects", "pack")
	os.MkdirAll(pack, 0o755)
	return root, pack
}

const glStalePack = (StaleTmpPackMinAgeSeconds + 60) * time.Second

func TestRemovesOldTmpPackDebris(t *testing.T) {
	repo, pack := glPackRepo(t)
	glGuard(t, false)
	var debris []string
	for _, name := range []string{"tmp_pack_AbCd12", "tmp_idx_XyZ", "tmp_rev_Q1", "tmp_mtimes_M8"} {
		p := filepath.Join(pack, name)
		glTouch(t, p, glStalePack)
		debris = append(debris, p)
	}
	if removed := ClearStaleTmpPacks(repo, nil); len(removed) != 4 {
		t.Fatal(removed)
	}
	for _, p := range debris {
		if pathExists(p) {
			t.Fatal(p)
		}
	}
}

func TestSparesFreshDebrisAndRealPacks(t *testing.T) {
	repo, pack := glPackRepo(t)
	glGuard(t, false)
	fresh := filepath.Join(pack, "tmp_pack_fresh")
	os.WriteFile(fresh, []byte("y"), 0o644)
	realPack, realIdx := filepath.Join(pack, "pack-abc123.pack"), filepath.Join(pack, "pack-abc123.idx")
	glTouch(t, realPack, 10*24*time.Hour)
	glTouch(t, realIdx, 10*24*time.Hour)
	if removed := ClearStaleTmpPacks(repo, nil); len(removed) != 0 || !pathExists(fresh) || !pathExists(realPack) || !pathExists(realIdx) {
		t.Fatal(removed)
	}
}

func TestSkipsSweepWhileGitIsRunning(t *testing.T) {
	repo, pack := glPackRepo(t)
	glGuard(t, true)
	p := filepath.Join(pack, "tmp_pack_old")
	glTouch(t, p, glStalePack)
	if removed := ClearStaleTmpPacks(repo, nil); len(removed) != 0 || !pathExists(p) {
		t.Fatal(removed)
	}
}

func TestBareRepoPackDirIsSwept(t *testing.T) {
	glGuard(t, false)
	root := t.TempDir()
	debris := filepath.Join(root, "objects", "pack", "tmp_pack_killedGc")
	glTouch(t, debris, glStalePack)
	if removed := ClearStaleTmpPacks(root, nil); !slices.Equal(removed, []string{debris}) || pathExists(debris) {
		t.Fatal(removed)
	}
}

func TestNoGitDirIsANoop(t *testing.T) {
	if got := ClearStaleTmpPacks(t.TempDir(), nil); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestUnlinkFailureIsSurfacedAtWarning(t *testing.T) {
	repo, pack := glPackRepo(t)
	glGuard(t, false)
	stuck := filepath.Join(pack, "tmp_pack_stuck")
	glTouch(t, stuck, glStalePack)
	prev := gitlockRemove
	gitlockRemove = func(p string) error {
		if filepath.Base(p) == "tmp_pack_stuck" {
			return &os.PathError{Op: "unlink", Path: p, Err: syscall.EACCES}
		}
		return prev(p)
	}
	t.Cleanup(func() { gitlockRemove = prev })
	logs := glCaptureLog(t)
	if got := ClearStaleTmpPacks(repo, nil); len(got) != 0 {
		t.Fatal(got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "tmp_pack_stuck") {
		t.Fatalf("logs %q", logs.String())
	}
}
