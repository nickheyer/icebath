//go:build linux

package tools

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Reports whether a started process has exited, reaped or not
func glExited(cmd *exec.Cmd) bool { return !proc.Exists(cmd.Process.Pid) }

// Waits for a started process within the budget, false on timeout
func glWaitExit(cmd *exec.Cmd, timeout time.Duration) (int, bool) {
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return cmd.ProcessState.ExitCode(), true
	case <-time.After(timeout):
		return 0, false
	}
}

// Kills a started process and reaps it
func glKill(cmd *exec.Cmd) {
	cmd.Process.Kill()
	cmd.Wait()
}

// Polls cond every 50 milliseconds until true, failing on the deadline with what
func glWaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Fixture environment with one variable replaced
func glEnvWith(key, value string) []string {
	var env []string
	for _, kv := range glEnv() {
		if !strings.HasPrefix(kv, key+"=") {
			env = append(env, kv)
		}
	}
	return append(env, key+"="+value)
}

// Runs git in dir with the given environment, returning its stderr and exit code
func glGitEnvRC(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("git %v: %v", args, err)
	}
	return errOut.String(), cmd.ProcessState.ExitCode()
}

// Starts a git in dir with stderr on a file the children inherit and outlive
func glStartGit(t *testing.T, dir, errPath string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	sink, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, env, sink
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// A real git status that takes index.lock and then blocks on a FIFO .gitignore nobody writes
func glStatusBlockedOnAFifo(t *testing.T, repo string, argv ...string) *exec.Cmd {
	t.Helper()
	junk := filepath.Join(repo, "junk")
	if err := os.Mkdir(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(junk, ".gitignore"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(repo, "a.txt"), now, now); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append(slices.Clone(argv), "status", "--porcelain")...)
	cmd.Dir, cmd.Env = repo, glEnv()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	deadline := time.Now().Add(20 * time.Second)
	for !pathExists(lock) {
		if glExited(cmd) || time.Now().After(deadline) {
			glKill(cmd)
			t.Fatal("git status never took index.lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cmd
}

// A real git commit form holding index.lock while its editor waits, the lock fd closed
func glGitInTheEditor(t *testing.T, repo, tmp string, argv []string) *exec.Cmd {
	t.Helper()
	editor := filepath.Join(tmp, "editor.sh")
	script := "#!/bin/sh\n: > \"$1.ready\"\nwhile [ ! -e \"$1.go\" ]; do sleep 0.02; done\nprintf 'fixture\\n' > \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repo
	cmd.Env = glEnv("GIT_EDITOR="+editor, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	msg := filepath.Join(repo, ".git", "COMMIT_EDITMSG")
	deadline := time.Now().Add(20 * time.Second)
	for !pathExists(msg + ".ready") {
		if glExited(cmd) || time.Now().After(deadline) {
			glKill(cmd)
			t.Fatalf("%v never reached its editor", argv)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd
}

// Lets a waiting editor finish its commit
func glReleaseEditor(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".git", "COMMIT_EDITMSG.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAKilledGitsIndexLockGoesAtOnceAndALiveOnesStays(t *testing.T) {
	repo := glRepo(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	ctx := glKilledUpdateReceipt(t)
	cmd := glStatusBlockedOnAFifo(t, repo)
	released, err := ReleaseDeadIndexLock(ctx, repo)
	liveKept := err == nil && !released && pathExists(lock)
	glKill(cmd)
	if !liveKept {
		t.Fatalf("a live git's lock: released %v err %v", released, err)
	}
	if !pathExists(lock) {
		t.Fatal("premise: a SIGKILLed status strands index.lock")
	}
	if got := updaterecord.ReadLatestReceipt(ctx)["outcome"]; got != "running" {
		t.Fatalf("latest outcome %v", got)
	}
	released, err = glReleaseDeadIndexLock(ctx, repo)
	if err != nil || !released {
		t.Fatalf("released %v %v", released, err)
	}
	if pathExists(lock) {
		t.Fatal("the killed git's lock stays")
	}
}

func TestAKilledUpdatesLockStaysWhileAnyGitWorksInTheCheckout(t *testing.T) {
	for _, form := range []string{"dashed git-commit in the editor", "a reader git in the checkout"} {
		t.Run(form, func(t *testing.T) {
			repo := glRepo(t)
			tmp := t.TempDir()
			lock := filepath.Join(repo, ".git", "index.lock")
			ctx := glKilledUpdateReceipt(t)
			dashed := strings.HasPrefix(form, "dashed")
			var cmd *exec.Cmd
			if dashed {
				bin := filepath.Join(glGit(t, repo, "--exec-path"), "git-commit")
				if !pathExists(bin) {
					t.Skip("this git ships no dashed git-commit")
				}
				cmd = glGitInTheEditor(t, repo, tmp, []string{bin, "-a"})
				if !pathExists(lock) {
					glReleaseEditor(t, repo)
					glKill(cmd)
					t.Fatal("premise: git commit holds index.lock in its editor")
				}
			} else {
				if err := os.WriteFile(lock, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command("git", "cat-file", "--batch")
				cmd.Dir, cmd.Env = repo, glEnv()
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer stdin.Close()
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
			}
			released, err := ReleaseDeadIndexLock(ctx, repo)
			liveKept := err == nil && !released && pathExists(lock)
			glReleaseEditor(t, repo)
			if dashed {
				if rc, ok := glWaitExit(cmd, 20*time.Second); !ok || rc != 0 {
					t.Fatalf("the user's commit failed: exited %v rc %d", ok, rc)
				}
			} else {
				glKill(cmd)
			}
			if !liveKept {
				t.Fatalf("%s: the reclaim deleted a lock a live git may own (released %v err %v)", form, released, err)
			}
			if !dashed {
				released, err := glReleaseDeadIndexLock(ctx, repo)
				if err != nil || !released {
					t.Fatalf("once the checkout's git is gone the killed update's lock goes: lock_exists %v killed_update_owns %v held_open %+v err %v",
						pathExists(lock), killedUpdateOwns(ctx, lock), pm.HeldOpen(lock, repo, true), err)
				}
			}
		})
	}
}

func TestAGitElsewhereExitingMidScanIsGoneNotAHiddenHolder(t *testing.T) {
	repo := glRepo(t)
	tmp := t.TempDir()
	other := filepath.Join(tmp, "other")
	if err := pyshutil.CopyTreeIgnore(repo, other, false, false, pyshutil.IgnorePatterns("*.lock")); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	marker := "icebath.test=exits-mid-scan"
	cmd := glGitInTheEditor(t, other, tmp, []string{"git", "-c", marker, "commit", "-a"})
	var real func([]string, bool) bool
	real = pm.SwapCountsAsHolder(func(args []string, anyGit bool) bool {
		if slices.Contains(args, marker) && !glExited(cmd) {
			glReleaseEditor(t, other)
			glWaitFor(t, "the other repository's git to exit (unreaped)", 20*time.Second, func() bool { return proc.Zombie(cmd.Process.Pid) })
		}
		return real(args, anyGit)
	})
	t.Cleanup(func() { pm.SwapCountsAsHolder(real) })
	held := pm.HeldOpen(lock, repo, false)
	glReleaseEditor(t, other)
	if _, ok := glWaitExit(cmd, 20*time.Second); !ok {
		glKill(cmd)
		t.Fatal("the other repository's commit never exited")
	}
	if !held.Free() {
		t.Fatalf("a git exiting mid scan elsewhere holds nothing, got %+v", held)
	}
}

const glHoldInRefTransaction = `#!/bin/sh
input=$(cat)
case "$1:$input" in
  prepared:*refs/remotes/origin/main*)
    dir=$(git rev-parse --git-dir)
    : > "$dir/held"
    while [ ! -e "$dir/release" ]; do sleep 0.05; done ;;
esac
`

var glFetchArgs = []string{"-c", "maintenance.auto=false", "-c", "gc.auto=0", "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main"}

// Tips of the clone's remote main and the origin's HEAD
func glTips(t *testing.T, clone, repo string) (string, string) {
	t.Helper()
	return glGit(t, clone, "rev-parse", "refs/remotes/origin/main"), glGit(t, repo, "rev-parse", "HEAD")
}

func TestAKilledFetchsRefLockGoesAndTheNextFetchLands(t *testing.T) {
	repo := glRepo(t)
	tmp := t.TempDir()
	clone := filepath.Join(tmp, "clone")
	glGit(t, repo, "branch", "-M", "main")
	glGit(t, tmp, "clone", "-q", "file://"+repo, clone)
	glGit(t, repo, "-c", "maintenance.auto=false", "commit", "-q", "--allow-empty", "-m", "next")
	gitDir := filepath.Join(clone, ".git")
	hook := filepath.Join(gitDir, "hooks", "reference-transaction")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(glHoldInRefTransaction), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(gitDir, "refs", "remotes", "origin", "main.lock")
	ctx := glKilledUpdateReceipt(t)
	errPath := filepath.Join(tmp, "fetch.err")
	cmd := glStartGit(t, clone, errPath, glEnv(), glFetchArgs...)
	release := func() {
		glKill(cmd)
		os.WriteFile(filepath.Join(gitDir, "release"), nil, 0o644)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !pathExists(filepath.Join(gitDir, "held")) {
		if glExited(cmd) || time.Now().After(deadline) {
			body, _ := os.ReadFile(errPath)
			release()
			t.Fatalf("the fetch never reached its ref transaction: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !pathExists(lock) {
		release()
		t.Fatal("premise: the held fetch holds its ref lock")
	}
	released, err := ReleaseDeadFetchLocks(ctx, clone)
	liveKept := err == nil && len(released) == 0 && pathExists(lock)
	release()
	if !liveKept {
		t.Fatalf("the reclaim took a live fetch's lock: %v %v", released, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	stderr, rc := glGitEnvRC(t, clone, glEnv(), glFetchArgs...)
	if rc == 0 || !strings.Contains(stderr, "origin/main") {
		t.Fatalf("premise: %s", stderr)
	}
	released, err = glReleaseDeadFetchLocks(ctx, clone, lock)
	if err != nil || !slices.Equal(released, []string{lock}) {
		t.Fatalf("released %v %v", released, err)
	}
	if stderr, rc := glGitEnvRC(t, clone, glEnv(), glFetchArgs...); rc != 0 {
		t.Fatal(stderr)
	}
	if a, b := glTips(t, clone, repo); a != b {
		t.Fatalf("tips %s %s", a, b)
	}
}

const glHoldInPackTransfer = "#!/bin/sh\n: > \"%s\"\nwhile [ ! -e \"%s\" ]; do sleep 0.05; done\nexec \"$@\"\n"

func TestAKilledShallowFetchsShallowLockGoesAndTheNextFetchLands(t *testing.T) {
	repo := glRepo(t)
	tmp := t.TempDir()
	clone := filepath.Join(tmp, "clone")
	glGit(t, repo, "branch", "-M", "main")
	glGit(t, tmp, "clone", "-q", "--depth", "1", "file://"+repo, clone)
	glGit(t, repo, "-c", "maintenance.auto=false", "commit", "-q", "--allow-empty", "-m", "next")
	held, releasePath := filepath.Join(tmp, "held"), filepath.Join(tmp, "release")
	hook := filepath.Join(tmp, "pack-objects-hook")
	if err := os.WriteFile(hook, []byte(fmt.Sprintf(glHoldInPackTransfer, held, releasePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(tmp, "gitconfig")
	if err := os.WriteFile(config, []byte("[uploadpack]\n\tpackObjectsHook = "+hook+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch := []string{"-c", "maintenance.auto=false", "-c", "gc.auto=0", "fetch", "--depth", "1", "origin", "+refs/heads/main:refs/remotes/origin/main"}
	env := glEnvWith("GIT_CONFIG_GLOBAL", config)
	lock := filepath.Join(clone, ".git", "shallow.lock")
	ctx := glKilledUpdateReceipt(t)
	errPath := filepath.Join(tmp, "fetch.err")
	cmd := glStartGit(t, clone, errPath, env, fetch...)
	release := func() {
		glKill(cmd)
		os.WriteFile(releasePath, nil, 0o644)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !pathExists(held) {
		if glExited(cmd) || time.Now().After(deadline) {
			body, _ := os.ReadFile(errPath)
			release()
			t.Fatalf("the fetch never reached its pack transfer: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !pathExists(lock) {
		release()
		t.Fatal("premise: the shallow fetch holds shallow.lock")
	}
	released, err := ReleaseDeadFetchLocks(ctx, clone)
	liveKept := err == nil && len(released) == 0 && pathExists(lock)
	release()
	if !liveKept {
		t.Fatalf("the reclaim took a live fetch's lock: %v %v", released, err)
	}
	stderr, rc := glGitEnvRC(t, clone, env, fetch...)
	if rc == 0 || !strings.Contains(stderr, "shallow.lock") {
		t.Fatalf("premise: %s", stderr)
	}
	released, err = glReleaseDeadFetchLocks(ctx, clone, lock)
	if err != nil || !slices.Equal(released, []string{lock}) {
		t.Fatalf("released %v %v", released, err)
	}
	if stderr, rc := glGitEnvRC(t, clone, env, fetch...); rc != 0 {
		t.Fatal(stderr)
	}
	if a, b := glTips(t, clone, repo); a != b {
		t.Fatalf("tips %s %s", a, b)
	}
}

func TestALiveHermesShallowLockHolderKeepsItsLock(t *testing.T) {
	repo := glRepo(t)
	ctx := glKilledUpdateReceipt(t)
	held, err := acquireShallowLock(filepath.Join(repo, ".git", "shallow"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	released, err := ReleaseDeadFetchLocks(ctx, repo)
	if err != nil || len(released) != 0 {
		t.Fatalf("released %v %v", released, err)
	}
	if !pathExists(filepath.Join(repo, ".git", "shallow.lock")) {
		t.Fatal("the live holder's lock went")
	}
}
