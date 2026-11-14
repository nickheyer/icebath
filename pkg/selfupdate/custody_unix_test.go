//go:build !windows

package selfupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Records whether the checkout lock file is among this process's open fds, then hands over
const cuRecorder = `#!/bin/sh
lock="$ICEBATH_TEST_LOCK"; out="$ICEBATH_TEST_OUT"
held=no
for fd in /proc/$$/fd/*; do
  [ "$(readlink "$fd")" = "$lock" ] && held=yes
done
echo "$held" >> "$out"
exec "$@"
`

func cuGitEnv(t *testing.T) {
	for key, value := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e"} {
		t.Setenv(key, value)
	}
}

func cuGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return pystr.Strip(string(out))
}

// A fresh repository under the test's temp dir with one committed file
func cuRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	cuGitEnv(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cuGit(t, root, "init", "-q", "-b", "main")
	cuWrite(t, filepath.Join(root, "f.txt"), "one\n", 0o644)
	cuGit(t, root, "add", "f.txt")
	cuGit(t, root, "commit", "-qm", "one")
	return root, tmp
}

func cuWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func cuRead(path string) string {
	raw, _ := os.ReadFile(path)
	return strings.TrimPrefix(string(raw), "\xef\xbb\xbf")
}

func cuLock(t *testing.T, marker, root string) *updaterecord.UpdateLock {
	t.Helper()
	lock := updaterecord.NewUpdateLock(marker, root, true)
	if ok, err := lock.Acquire(context.Background()); !ok || err != nil {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	return lock
}

func cuLockPath(t *testing.T, root string) string {
	t.Helper()
	path, err := updaterecord.CheckoutLockPath(root)
	if err != nil {
		t.Fatal(err)
	}
	real, err := pypath.Realpath(path, pypath.Lax)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func cuNeedProcFD(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc fd listing")
	}
}

func cuNeedLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX sessions and /proc")
	}
}

func cuAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	return cuState(pid) != "Z"
}

func cuState(pid int) string {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if runtime.GOOS != "linux" {
			return "R"
		}
		return "Z"
	}
	text := string(stat)
	return text[strings.LastIndexByte(text, ')')+2:][:1]
}

func cuGoneWithin(pid int, within time.Duration) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !cuAlive(pid) {
			return true
		}
	}
	return !cuAlive(pid)
}

func cuWaitFor(t *testing.T, within time.Duration, ok func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(within); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal(what)
		}
	}
}

func cuExists(path string) bool { _, err := os.Stat(path); return err == nil }

// Starts command as the build runner does, under a held checkout lock released once it started
func cuLaunchInCustody(t *testing.T, repo, tmp string, command []string, newSession bool, env []string) *exec.Cmd {
	t.Helper()
	lock := cuLock(t, filepath.Join(tmp, "marker"), repo)
	defer lock.Release(context.Background())
	contained, err := ContainedCommand(context.Background(), command, true, repo)
	if err != nil {
		t.Fatal(err)
	}
	if contained.Argv[1] != CustodyReapEntry {
		t.Fatalf("not under the reap launcher: %v", contained.Argv)
	}
	cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
	cmd.ExtraFiles, cmd.Env = contained.ExtraFiles, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: newSession}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func cuWait(t *testing.T, cmd *exec.Cmd, within time.Duration) int {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%v did not exit within %v", cmd.Args, within)
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

func cuWriterArgv(late, delay string, detach bool) []string {
	flag := "0"
	if detach {
		flag = "1"
	}
	return cuChildArgv("writer", late, delay, flag)
}

func cuNodeArgv(code, sleep, touch string, writer []string) []string {
	return cuChildArgv("node", append([]string{code, sleep, touch}, writer...)...)
}

func TestCustodyEveryUpdaterGitRunnerForbidsDetachedChildren(t *testing.T) {
	repo, _ := cuRepo(t)
	runners := map[string]func(args []string) string{
		"RunGit": func(args []string) string {
			out, err := RunGit(context.Background(), []string{"git"}, args, ChildOptions{Dir: repo, Capture: true, Text: true})
			if err != nil {
				t.Fatal(err)
			}
			return out.Stdout
		},
		"updaterecord.RunGitQuery": func(args []string) string {
			out, err := updaterecord.RunGitQuery(context.Background(), []string{"git", "-C", repo}, args, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			return out.Stdout
		},
	}
	for name, run := range runners {
		for _, key := range []string{"gc.autoDetach", "maintenance.auto"} {
			if got := pystr.Strip(run([]string{"config", "--get", key})); got != "false" {
				t.Errorf("%s may fork a detached gc or maintenance child: %s=%q", name, key, got)
			}
		}
	}
}

func TestCustodyLockFDReachesLocalMutatorsOnly(t *testing.T) {
	cuNeedProcFD(t)
	repo, tmp := cuRepo(t)
	recorder := filepath.Join(tmp, "recorder.sh")
	cuWrite(t, recorder, cuRecorder, 0o755)
	remote := filepath.Join(tmp, "remote.git")
	if err := exec.Command("git", "clone", "-q", "--bare", repo, remote).Run(); err != nil {
		t.Fatal(err)
	}
	cuGit(t, repo, "remote", "add", "origin", remote)
	cuGit(t, repo, "config", "filter.rec.clean", recorder+" cat")
	cuWrite(t, filepath.Join(repo, ".git", "info", "attributes"), "f.txt filter=rec\n", 0o644)
	outFetch, outStash := filepath.Join(tmp, "fetch.out"), filepath.Join(tmp, "stash.out")
	lock := cuLock(t, filepath.Join(tmp, "marker"), repo)
	t.Setenv("ICEBATH_TEST_LOCK", cuLockPath(t, repo))
	t.Setenv("ICEBATH_TEST_OUT", outFetch)
	ctx := context.Background()
	fetched, err := RunGit(ctx, []string{"git"}, []string{"fetch", "--upload-pack=" + recorder + " git-upload-pack", "origin"}, ChildOptions{Dir: repo, Capture: true, Text: true})
	if err != nil || fetched.ReturnCode != 0 {
		t.Fatal(err, fetched)
	}
	t.Setenv("ICEBATH_TEST_OUT", outStash)
	cuWrite(t, filepath.Join(repo, "f.txt"), "two\n", 0o644)
	stashed, err := RunGit(ctx, []string{"git"}, []string{"stash", "push", "-m", "custody"}, ChildOptions{Dir: repo, Capture: true, Text: true})
	lock.Release(ctx)
	if err != nil || stashed.ReturnCode != 0 {
		t.Fatal(err, stashed)
	}
	if got := strings.Fields(cuRead(outFetch)); !slices.Equal(got, []string{"no"}) {
		t.Fatalf("a fetch child held the lock: %v", got)
	}
	if !slices.Contains(strings.Fields(cuRead(outStash)), "yes") {
		t.Fatal("a local mutator's child lost the checkout lock")
	}
}

func TestCustodyABackgroundHookNeverKeepsACompletedUpdateLocked(t *testing.T) {
	repo, tmp := cuRepo(t)
	base := cuGit(t, repo, "rev-parse", "HEAD")
	cuWrite(t, filepath.Join(repo, "f.txt"), "two\n", 0o644)
	cuGit(t, repo, "commit", "-qam", "two")
	target := cuGit(t, repo, "rev-parse", "HEAD")
	cuGit(t, repo, "reset", "-q", "--hard", base)
	pidFile := filepath.Join(tmp, "hook-child.pid")
	cuWrite(t, filepath.Join(repo, ".git", "hooks", "post-merge"), "#!/bin/sh\nsleep 60 </dev/null >/dev/null 2>&1 &\necho $! > "+pidFile+"\n", 0o755)
	ctx := context.Background()
	lock := cuLock(t, filepath.Join(tmp, "marker"), repo)
	merged, err := RunGit(ctx, []string{"git"}, []string{"merge", "--ff-only", target}, ChildOptions{Dir: repo, Capture: true, Text: true, Timeout: 30 * time.Second})
	lock.Release(ctx)
	defer func() {
		if pid, err := strconv.Atoi(pystr.Strip(cuRead(pidFile))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	if err != nil || merged.ReturnCode != 0 {
		t.Fatal(err, merged)
	}
	if cuRead(filepath.Join(repo, "f.txt")) != "two\n" {
		t.Fatal("the merge did not land")
	}
	contender := updaterecord.NewUpdateLock(filepath.Join(tmp, "next-marker"), repo, true)
	ok, err := contender.Acquire(ctx)
	defer contender.Release(ctx)
	if !ok || err != nil {
		t.Fatalf("a background git hook kept the completed update's checkout locked: %v", err)
	}
}

// Kills an updater mid fetch and proves the fetch it started died with it
func cuAssertFetchDiesWithKilledOwner(t *testing.T, noDeathSignal bool) {
	cuNeedLinux(t)
	repo, tmp := cuRepo(t)
	remote := filepath.Join(tmp, "remote.git")
	if err := exec.Command("git", "clone", "-q", "--bare", repo, remote).Run(); err != nil {
		t.Fatal(err)
	}
	cuGit(t, repo, "remote", "add", "origin", remote)
	started := filepath.Join(tmp, "upload-pack.pid")
	blocker := filepath.Join(tmp, "block.sh")
	cuWrite(t, blocker, "#!/bin/sh\necho $$ > "+started+".self\necho $PPID > "+started+"\nsleep 60\nexec git-upload-pack \"$@\"\n", 0o755)
	argv := cuChildArgv("fetch-owner", repo, filepath.Join(tmp, "marker"), blocker)
	owner := exec.Command(argv[0], argv[1:]...)
	if noDeathSignal {
		owner.Env = append(os.Environ(), "ICEBATH_CUSTODY_TEST_NO_DEATHSIGNAL=1")
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Process.Kill()
		if pid, err := strconv.Atoi(pystr.Strip(cuRead(started + ".self"))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	cuWaitFor(t, 15*time.Second, func() bool { return pystr.Strip(cuRead(started)) != "" }, "the fetch never started its upload pack")
	fetchPID, _ := strconv.Atoi(pystr.Strip(cuRead(started)))
	if pystr.Strip(cuRead("/proc/"+strconv.Itoa(fetchPID)+"/comm")) != "git" {
		fetchPID = cuParent(fetchPID)
	}
	if !strings.Contains(cuRead("/proc/"+strconv.Itoa(fetchPID)+"/cmdline"), "git") {
		t.Fatalf("pid %d is not the fetch", fetchPID)
	}
	_ = owner.Process.Kill()
	cuWait(t, owner, 10*time.Second)
	if !cuGoneWithin(fetchPID, 5*time.Second) {
		t.Fatal("git fetch outlived its killed update owner")
	}
}

func TestCustodyNetworkGitDiesWithItsKilledOwner(t *testing.T) {
	cuAssertFetchDiesWithKilledOwner(t, false)
}

func TestCustodyNetworkGitDiesWithItsKilledOwnerWithoutAParentDeathSignal(t *testing.T) {
	cuAssertFetchDiesWithKilledOwner(t, true)
}

func TestCustodyAPartialCloneMoveNeverFetchesUnderTheLockFD(t *testing.T) {
	cuNeedProcFD(t)
	repo, tmp := cuRepo(t)
	recorder := filepath.Join(tmp, "recorder.sh")
	cuWrite(t, recorder, cuRecorder, 0o755)
	origin := filepath.Join(tmp, "origin.git")
	if err := exec.Command("git", "clone", "-q", "--bare", repo, origin).Run(); err != nil {
		t.Fatal(err)
	}
	cuGit(t, origin, "config", "uploadpack.allowFilter", "true")
	cuGit(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	clone := filepath.Join(tmp, "clone")
	if err := exec.Command("git", "clone", "-q", "--filter=blob:none", "file://"+origin, clone).Run(); err != nil {
		t.Fatal(err)
	}
	cuWrite(t, filepath.Join(repo, "new.txt"), "only on the remote\n", 0o644)
	cuGit(t, repo, "add", "new.txt")
	cuGit(t, repo, "commit", "-qm", "two")
	cuGit(t, repo, "push", "-q", origin, "main")
	cuGit(t, clone, "fetch", "-q", "origin")
	cuGit(t, clone, "config", "remote.origin.uploadpack", recorder+" git-upload-pack")
	missing := 0
	for _, o := range strings.Fields(cuGit(t, clone, "rev-list", "--objects", "--missing=print", "origin/main")) {
		if strings.HasPrefix(o, "?") {
			missing++
		}
	}
	if missing == 0 {
		t.Fatal("fixture: the new blob must be missing locally")
	}
	out := filepath.Join(tmp, "upload-pack.out")
	t.Setenv("ICEBATH_TEST_LOCK", cuLockPath(t, clone))
	t.Setenv("ICEBATH_TEST_OUT", out)
	ctx := context.Background()
	lock := cuLock(t, filepath.Join(tmp, "marker"), clone)
	moved, err := RunGit(ctx, []string{"git"}, []string{"reset", "--hard", "origin/main"}, ChildOptions{Dir: clone, Capture: true, Text: true})
	lock.Release(ctx)
	if err != nil || moved.ReturnCode != 0 {
		t.Fatal(err, moved)
	}
	if cuRead(filepath.Join(clone, "new.txt")) != "only on the remote\n" {
		t.Fatal("the move did not land")
	}
	served := strings.Fields(cuRead(out))
	if len(served) == 0 || slices.Contains(served, "yes") {
		t.Fatalf("an upload-pack (a lazy fetch) ran holding the checkout lock: %v", served)
	}
	if !slices.Contains(moved.Args, "credential.helper=") {
		t.Fatal(moved.Args)
	}
}

func TestCustodyRunGitHandsTheLockFDToLocalMutatorsOnly(t *testing.T) {
	cuNeedProcFD(t)
	if updaterecord.IsLocalMutator([]string{"pull"}) {
		t.Fatal("pull is never a local mutator")
	}
	for _, tc := range []struct {
		args  []string
		holds bool
	}{{[]string{"gc", "--auto"}, true}, {[]string{"rev-parse", "HEAD"}, false}} {
		t.Run(tc.args[0], func(t *testing.T) {
			repo, tmp := cuRepo(t)
			git := filepath.Join(tmp, "git-recorder.sh")
			cuWrite(t, git, strings.Replace(cuRecorder, `exec "$@"`, `exec git "$@"`, 1), 0o755)
			out := filepath.Join(tmp, "git.out")
			t.Setenv("ICEBATH_TEST_LOCK", cuLockPath(t, repo))
			t.Setenv("ICEBATH_TEST_OUT", out)
			ctx := context.Background()
			lock := cuLock(t, filepath.Join(tmp, "marker"), repo)
			ran, err := RunGit(ctx, []string{git}, tc.args, ChildOptions{Dir: repo, Capture: true, Text: true})
			lock.Release(ctx)
			if err != nil || ran.ReturnCode != 0 {
				t.Fatal(err, ran)
			}
			want := []string{"no"}
			if tc.holds {
				want = []string{"yes"}
			}
			if got := strings.Fields(cuRead(out)); !slices.Equal(got, want) {
				t.Fatalf("%v held %v", tc.args, got)
			}
		})
	}
}

func TestCustodyATimedOutChildNeverWaitsOnAGrandchildHoldingItsPipes(t *testing.T) {
	saveWin, saveHeld, savePopen := custodyWindows, custodyHeld, custodyPopen
	t.Cleanup(func() { custodyWindows, custodyHeld, custodyPopen = saveWin, saveHeld, savePopen })
	custodyWindows = func() bool { return true }
	custodyHeld = func() bool { return true }
	custodyPopen = func(_ context.Context, argv []string, _ bool, opts ChildOptions) (*startedChild, error) {
		return popenChild(argv, opts)
	}
	start := time.Now()
	_, err := Run(context.Background(), []string{"sh", "-c", "sleep 20 & sleep 20"}, false, ChildOptions{Capture: true, Timeout: 500 * time.Millisecond})
	var timeout *updaterecord.GitTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if time.Since(start) >= 15*time.Second {
		t.Fatal("Run waited on a grandchild after the timeout")
	}
}

func TestCustodyNoBuildDescendantOutlivesTheBuildCall(t *testing.T) {
	cuNeedProcFD(t)
	repo, tmp := cuRepo(t)
	late := filepath.Join(tmp, "late")
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writer := strings.Join(cuWriterArgv(late, "2", false), "' '")
	node := filepath.Join(bin, "node")
	cuWrite(t, node, strings.Replace(cuRecorder, `exec "$@"`, "'"+writer+"' >/dev/null 2>&1 &\nexit 1", 1), 0o755)
	t.Setenv("ICEBATH_TEST_LOCK", cuLockPath(t, repo))
	t.Setenv("ICEBATH_TEST_OUT", filepath.Join(tmp, "node.out"))
	ctx := context.Background()
	lock := cuLock(t, filepath.Join(tmp, "marker"), repo)
	contained, err := ContainedCommand(ctx, []string{node, filepath.Join(repo, "build.mjs")}, true, repo)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
	cmd.Dir, cmd.ExtraFiles = repo, contained.ExtraFiles
	_, err = pm.RunContained(cmd, "probe", pm.ContainOptions{Indent: "  "})
	cerr := contained.Close()
	lock.Release(ctx)
	var failed *pm.CalledProcessError
	if !errors.As(err, &failed) || cerr != nil {
		t.Fatalf("want the build's CalledProcessError, got %v %v", err, cerr)
	}
	time.Sleep(3 * time.Second)
	if cuExists(late) {
		t.Fatal("a build descendant kept writing after the build call returned")
	}
}

func TestCustodyAGroupKillOfTheCallerStopsTheWholeBuild(t *testing.T) {
	cuNeedLinux(t)
	repo, tmp := cuRepo(t)
	lateNode, lateChild := filepath.Join(tmp, "late-node"), filepath.Join(tmp, "late-child")
	launcher := cuLaunchInCustody(t, repo, tmp, cuNodeArgv("0", "2", lateNode, cuWriterArgv(lateChild, "2", false)), true, nil)
	time.Sleep(time.Second)
	_ = syscall.Kill(-launcher.Process.Pid, syscall.SIGKILL)
	cuWait(t, launcher, 10*time.Second)
	time.Sleep(3 * time.Second)
	if cuExists(lateNode) {
		t.Fatal("node survived a group kill of its caller")
	}
	if cuExists(lateChild) {
		t.Fatal("a build descendant survived a group kill of its caller")
	}
}

// Kills or interrupts an owner whose build left a detached heartbeat writer and proves no contender shares the checkout with it
func cuAssertCustodyOutlivesTheCaller(t *testing.T, mode string, command []string, sig syscall.Signal, beat, ready string, env []string, settle time.Duration) {
	tmp := filepath.Dir(beat)
	repo := filepath.Join(tmp, "repo")
	argv := cuChildArgv(mode, append([]string{repo, filepath.Join(tmp, "marker")}, command...)...)
	owner := exec.Command(argv[0], argv[1:]...)
	owner.Env = append(os.Environ(), env...)
	owner.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Process.Kill()
		if pid, err := strconv.Atoi(strings.Fields(cuRead(beat) + " x")[0]); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	cuWaitFor(t, 20*time.Second, func() bool { return cuExists(beat) && (ready == "" || cuExists(ready)) }, "the build never started")
	_ = syscall.Kill(-owner.Process.Pid, sig)
	cuWait(t, owner, 10*time.Second)
	ctx := context.Background()
	contender := updaterecord.NewUpdateLock(filepath.Join(tmp, "next-marker"), repo, true)
	cuWaitFor(t, settle, func() bool { ok, _ := contender.Acquire(ctx); return ok }, "the checkout stayed locked after the caller was stopped")
	defer contender.Release(ctx)
	seen := cuRead(beat)
	time.Sleep(time.Second)
	if cuRead(beat) != seen {
		t.Fatal("a detached build writer kept writing under the next owner")
	}
}

func TestCustodyAGroupKillOfTheCallerKeepsCustodyUntilADetachedWriterIsGone(t *testing.T) {
	cuNeedLinux(t)
	_, tmp := cuRepo(t)
	beat := filepath.Join(tmp, "beat")
	node := cuNodeArgv("0", "60", "", cuChildArgv("heartbeat", beat))
	cuAssertCustodyOutlivesTheCaller(t, "contained-owner", node, syscall.SIGKILL, beat, "", nil, 20*time.Second)
}

func TestCustodyACtrlCKeepsCustodyUntilADetachedWriterIsGone(t *testing.T) {
	cuNeedLinux(t)
	for _, tc := range []struct{ name, verbose, cleanup string }{{"verbose-slow-cleanup", "1", "2"}, {"captured-instant-exit", "0", "0"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, tmp := cuRepo(t)
			beat, ready := filepath.Join(tmp, "beat"), filepath.Join(tmp, "ready")
			build := cuChildArgv("ctrl-c-build", append([]string{tc.cleanup, ready}, cuChildArgv("heartbeat", beat)...)...)
			cuAssertCustodyOutlivesTheCaller(t, "build-owner", build, syscall.SIGINT, beat, ready, []string{"ICEBATH_VERBOSE=" + tc.verbose}, 30*time.Second)
		})
	}
}

func TestCustodyADetachedGrandchildOutlivingNodeDiesBeforeTheBuildReturns(t *testing.T) {
	cuNeedLinux(t)
	for _, tc := range []struct {
		name string
		env  []string
	}{{"subreaper", nil}, {"ps-walk", append(os.Environ(), "ICEBATH_CUSTODY_TEST_NO_SUBREAPER=1")}} {
		t.Run(tc.name, func(t *testing.T) {
			repo, tmp := cuRepo(t)
			late := filepath.Join(tmp, "late")
			launcher := cuLaunchInCustody(t, repo, tmp, cuNodeArgv("0", "1.2", "", cuWriterArgv(late, "2", true)), false, tc.env)
			if code := cuWait(t, launcher, 30*time.Second); code != 0 {
				t.Fatalf("launcher exited %d", code)
			}
			time.Sleep(3 * time.Second)
			if cuExists(late) {
				t.Fatal("a detached build descendant kept writing after the build call returned")
			}
		})
	}
}

func TestCustodyATerminatedLauncherTakesNodeAndItsDescendantsWithIt(t *testing.T) {
	cuNeedLinux(t)
	repo, tmp := cuRepo(t)
	late := filepath.Join(tmp, "late")
	launcher := cuLaunchInCustody(t, repo, tmp, cuNodeArgv("0", "30", "", cuWriterArgv(late, "2", false)), true, nil)
	time.Sleep(time.Second)
	_ = launcher.Process.Signal(syscall.SIGTERM)
	if code := cuWait(t, launcher, 20*time.Second); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("launcher exited %d", code)
	}
	time.Sleep(3 * time.Second)
	if cuExists(late) {
		t.Fatal("a build descendant kept writing after its launcher was terminated")
	}
}

func TestCustodyAContainedBuildRunsInCheckoutCustody(t *testing.T) {
	cuNeedProcFD(t)
	repo, tmp := cuRepo(t)
	npm := filepath.Join(tmp, "npm")
	cuWrite(t, npm, strings.Replace(cuRecorder, `exec "$@"`, "exit 0", 1), 0o755)
	out := filepath.Join(tmp, "npm.out")
	t.Setenv("ICEBATH_TEST_LOCK", cuLockPath(t, repo))
	t.Setenv("ICEBATH_TEST_OUT", out)
	ctx := context.Background()
	lock := cuLock(t, filepath.Join(tmp, "marker"), repo)
	contained, err := ContainedCommand(ctx, []string{npm, "run", "build"}, true, repo)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
	cmd.ExtraFiles = contained.ExtraFiles
	_, err = pm.RunContained(cmd, "desktop build", pm.ContainOptions{})
	cerr := contained.Close()
	lock.Release(ctx)
	if err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	if got := strings.Fields(cuRead(out)); !slices.Equal(got, []string{"yes"}) {
		t.Fatalf("the build ran without the checkout lock: %v", got)
	}
}

// A repository with a bare remote and an upload pack helper that records its pid and sleeps
func cuFetchRig(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	cuGitEnv(t)
	tmp := t.TempDir()
	repo, remote := filepath.Join(tmp, "repo"), filepath.Join(tmp, "remote.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cuGit(t, repo, "init", "-q", "-b", "main")
	cuGit(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	if err := exec.Command("git", "clone", "-q", "--bare", repo, remote).Run(); err != nil {
		t.Fatal(err)
	}
	cuGit(t, repo, "remote", "add", "origin", remote)
	helperPID := filepath.Join(tmp, "helper.pid")
	helper := filepath.Join(tmp, "helper.sh")
	cuWrite(t, helper, "#!/bin/sh\necho $$ > "+helperPID+".tmp && mv "+helperPID+".tmp "+helperPID+"\nsleep 60\n", 0o755)
	t.Cleanup(func() {
		if pid, err := strconv.Atoi(pystr.Strip(cuRead(helperPID))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return repo, helper, helperPID
}

func cuWaitPID(t *testing.T, path string) int {
	t.Helper()
	cuWaitFor(t, 20*time.Second, func() bool { return cuExists(path) }, "the fetch never started its helper")
	pid, err := strconv.Atoi(pystr.Strip(cuRead(path)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestCustodyATimedOutFetchTakesItsHelpersWithIt(t *testing.T) {
	repo, helper, helperPID := cuFetchRig(t)
	saveHeld, saveDeath := custodyHeld, custodyDeathSignal
	t.Cleanup(func() { custodyHeld, custodyDeathSignal = saveHeld, saveDeath })
	custodyHeld = func() bool { return true }
	custodyDeathSignal = func() *syscall.SysProcAttr { return nil }
	_, err := RunGit(context.Background(), []string{"git"}, []string{"fetch", "--upload-pack=" + helper, "origin"}, ChildOptions{Dir: repo, Capture: true, Timeout: 2 * time.Second})
	var timeout *updaterecord.GitTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if !cuGoneWithin(cuWaitPID(t, helperPID), 5*time.Second) {
		t.Fatal("the fetch's helper outlived the timed-out fetch")
	}
}

func TestCustodyAKilledOwnerTakesTheFetchHelpersWithIt(t *testing.T) {
	repo, helper, helperPID := cuFetchRig(t)
	argv := cuChildArgv("fetch-owner", repo, filepath.Join(filepath.Dir(repo), "marker"), helper)
	owner := exec.Command(argv[0], argv[1:]...)
	owner.Env = append(os.Environ(), "ICEBATH_CUSTODY_TEST_NO_DEATHSIGNAL=1")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill() }()
	pid := cuWaitPID(t, helperPID)
	_ = owner.Process.Kill()
	cuWait(t, owner, 10*time.Second)
	if !cuGoneWithin(pid, 10*time.Second) {
		t.Fatal("the fetch's helper outlived its killed update owner")
	}
}

func TestCustodyAReapedGitNeverSignalsTheGroupItsPIDNowNames(t *testing.T) {
	git := exec.Command("true")
	git.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := git.Run(); err != nil {
		t.Fatal(err)
	}
	decoy := exec.Command("sleep", "60")
	decoy.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = decoy.Process.Kill(); _ = decoy.Wait() }()
	git.Process.Pid = decoy.Process.Pid
	killGroup(&startedChild{cmd: git, argv: git.Args, reaped: true})
	if cuGoneWithin(decoy.Process.Pid, 500*time.Millisecond) {
		t.Fatal("a reaped handle signalled another session's group")
	}
}

// Parent pid of pid read from /proc
func cuParent(pid int) int {
	for _, line := range strings.Split(cuRead("/proc/"+strconv.Itoa(pid)+"/status"), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			parent, _ := strconv.Atoi(strings.Fields(line)[1])
			return parent
		}
	}
	return 0
}

func TestPauseUpdateStartedFromARelaunchedGatewayDoesNotShareTheClaim(t *testing.T) {
	for _, tc := range []struct {
		host   []string
		adopts bool
	}{
		{[]string{"icebath", "gateway", "run"}, false},
		{[]string{"icebath", "--profile", "work", "gateway", "run"}, false},
		{[]string{"/opt/icebath/icebath", "-p", "work", "gateway", "run"}, false},
		{[]string{"icebath", "--profile", "work", "dashboard"}, false},
		{[]string{"icebath", "status"}, true},
	} {
		t.Run(strings.Join(tc.host, " "), func(t *testing.T) {
			argv := cuChildArgv("claim-holder", tc.host...)
			holder := exec.Command(argv[0], argv[1:]...)
			holder.Env = append(os.Environ(), "ICEBATH_HOME="+t.TempDir())
			out, err := holder.Output()
			if err != nil {
				t.Fatalf("holder failed: %v %s", err, out)
			}
			want := "False True"
			if tc.adopts {
				want = "True False"
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("probe under %v answered %q, want %q", tc.host, got, want)
			}
		})
	}
}
