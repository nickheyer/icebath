package pm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Reports whether some goroutine is inside a dependency lock wait, its lock file already opened
func sttLockWaiterOpened() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "pm.RuntimeLock(") && strings.Contains(g, "pm.LockFD(") {
			return true
		}
	}
	return false
}

// Polls until the condition holds or the timeout passes
func sttWaitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// Waits on a channel close for at most the timeout
func sttWaitClosed(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Orphan state of a provably deleted scratch clone under a home whose installs root is the test's
type sttRace struct {
	ctx      context.Context
	installs string
	checkout string
	held     chan struct{}
	done     chan struct{}
	exited   chan struct{}
	failures []string
	mu       sync.Mutex
}

// Builds the orphan state the reclaim race tests collect
func sttNewRace(t *testing.T) *sttRace {
	t.Helper()
	tmp := isolateHome(t)
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	r := &sttRace{
		ctx:      context.Background(),
		installs: filepath.Join(tmp, "home", "installs"),
		checkout: filepath.Join(tmp, "home", "cache", "scratch", "clone"),
		held:     make(chan struct{}),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	state := filepath.Join(r.installs, InstallKey(r.checkout))
	if state != InstallStateDir(r.ctx, r.checkout) {
		t.Fatalf("installs root is not %s", r.installs)
	}
	writeFile(t, filepath.Join(state, "inputs", ".project-root"), []byte(r.checkout), 0o666)
	sttMkdir(t, filepath.Join(state, "environments", "gen", "venv"))
	return r
}

// Records a failure seen off the test's own control flow
func (r *sttRace) fail(msg string) {
	r.mu.Lock()
	r.failures = append(r.failures, msg)
	r.mu.Unlock()
}

// Launcher A waits for the state's install lock, reports holding it, then keeps it until done
func (r *sttRace) launcherA() {
	defer close(r.exited)
	got, release, err := RuntimeLock(r.ctx, r.checkout, WaitForever)
	if err != nil {
		r.fail("launcher a: " + err.Error())
		return
	}
	if got {
		close(r.held)
	}
	sttWaitClosed(r.done, 30*time.Second)
	release()
}

// Recreates the checkout mid reclaim and starts launcher A, returning once its lock file is open
func (r *sttRace) leasedHook(real func(string) (bool, error)) func(string) (bool, error) {
	return func(path string) (bool, error) {
		if err := os.MkdirAll(r.checkout, 0o777); err != nil {
			r.fail(err.Error())
		}
		go r.launcherA()
		if !sttWaitFor(sttLockWaiterOpened, 30*time.Second) {
			r.fail("launcher a never opened the lock file")
		}
		return real(path)
	}
}

// Collects, waits for launcher A to hold the lock and proves launcher B is excluded
func (r *sttRace) collectAndCheckExclusion(t *testing.T) {
	t.Helper()
	defer func() {
		close(r.done)
		sttWaitClosed(r.exited, 30*time.Second)
	}()
	if _, err := CollectOrphanInstallStates(r.installs); err != nil {
		t.Fatal(err)
	}
	if !sttWaitClosed(r.held, 30*time.Second) {
		t.Fatalf("launcher a never held the lock %v", r.failures)
	}
	got, release, err := RuntimeLock(r.ctx, r.checkout, 0)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got {
		t.Fatal("two launchers hold the same state's install lock")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failures) > 0 {
		t.Fatal(r.failures)
	}
}

func TestLauncherWaitingOnAReclaimedStateLockExcludesLaterLaunchers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	r := sttNewRace(t)
	realLeased := leased
	t.Cleanup(func() { leased = realLeased })
	leased = r.leasedHook(realLeased)
	r.collectAndCheckExclusion(t)
}

func TestLockFileRemovalNeverLandsAfterAWaitingLauncherGotTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	r := sttNewRace(t)
	released := make(chan struct{})
	var releaseOnce sync.Once
	realLeased, realUnlink, realInstallLock := leased, unlinkStateFile, installLock
	t.Cleanup(func() { leased, unlinkStateFile, installLock = realLeased, realUnlink, realInstallLock })
	installLock = func(state string) func() {
		release := realInstallLock(state)
		if release == nil {
			releaseOnce.Do(func() { close(released) })
			return nil
		}
		return func() {
			release()
			releaseOnce.Do(func() { close(released) })
		}
	}
	unlinkStateFile = func(path string) error {
		select {
		case <-released:
			if filepath.Base(path) == ".install.lock" && !sttWaitClosed(r.held, 30*time.Second) {
				r.fail("launcher a never took the released lock")
			}
		default:
		}
		return realUnlink(path)
	}
	leased = r.leasedHook(realLeased)
	r.collectAndCheckExclusion(t)
}

func TestUnopenableLockPathFailsInsteadOfRetrying(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	tmp := isolateHome(t)
	t.Setenv(home.EnvHome, tmp)
	ctx := context.Background()
	installs := filepath.Join(tmp, "installs")
	checkout := filepath.Join(tmp, "clone")
	if InstallsRoot(ctx) != installs {
		t.Fatalf("installs root %s", InstallsRoot(ctx))
	}
	lock := filepath.Join(installs, InstallKey(checkout), ".install.lock")
	sttMkdir(t, filepath.Dir(lock))
	if err := os.Symlink(filepath.Join(tmp, "gone", "target"), lock); err != nil {
		t.Fatal(err)
	}
	outcome := make(chan error, 1)
	go func() {
		_, release, err := RuntimeLock(ctx, checkout, 0)
		if release != nil {
			release()
		}
		outcome <- err
	}()
	select {
	case err := <-outcome:
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("got %v, want a missing file error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("lock path retries an open that can never succeed")
	}
}
