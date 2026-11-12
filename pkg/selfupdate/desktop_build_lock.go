package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Pause between checkout and build lock polls of a waiting desktop build
var desktopBuildLockPoll = time.Second

// Advisory lock a desktop dependency install or build holds, after the checkout lock every updater takes
type DesktopBuildLock struct {
	Path        string
	ProjectRoot string
	Out         io.Writer
	handle      *os.File
	checkout    *updaterecord.UpdateLock
}

// Lock keyed by the resolved checkout path under the profile common root
func NewDesktopBuildLock(projectRoot string) *DesktopBuildLock {
	resolved, err := pypath.Resolve(projectRoot)
	if err != nil {
		resolved = projectRoot
	}
	sum := sha256.Sum256([]byte(pypath.Normcase(resolved)))
	key := hex.EncodeToString(sum[:])[:24]
	return &DesktopBuildLock{Path: filepath.Join(home.Root(), "locks", "desktop-build-"+key+".lock"), ProjectRoot: projectRoot, Out: os.Stdout}
}

// Takes the checkout lock then the build lock, false only when another process holds one and wait is unset
func (l *DesktopBuildLock) Acquire(ctx context.Context, wait bool) (bool, error) {
	if l.handle != nil {
		return true, nil
	}
	ok, err := l.acquireCheckout(ctx, wait)
	if err != nil || !ok {
		return false, err
	}
	acquired, err := l.acquireBuildLock(wait)
	if err != nil || !acquired {
		l.releaseCheckout(ctx)
	}
	return acquired, err
}

// Takes or joins the checkout lock before the build lock, refusing another checkout's hold
func (l *DesktopBuildLock) acquireCheckout(ctx context.Context, wait bool) (bool, error) {
	wanted, err := updaterecord.CheckoutLockPath(l.ProjectRoot)
	if err != nil {
		return false, err
	}
	if held, ok := updaterecord.HeldCheckoutPath(); ok && held != wanted {
		heldInfo, herr := os.Stat(held)
		wantedInfo, werr := os.Stat(wanted)
		if werr == nil && herr != nil {
			return false, herr
		}
		if werr != nil || !os.SameFile(heldInfo, wantedInfo) {
			return false, desktopOSError(fmt.Sprintf("this process holds the update lock of another checkout (%s), so it cannot also lock %s for its build", held, l.ProjectRoot))
		}
		return true, nil
	}
	lock := updaterecord.NewUpdateLock("", l.ProjectRoot, false)
	announced := false
	for {
		ok, err := lock.AcquireCheckout(l.ProjectRoot)
		if err != nil {
			return false, err
		}
		if ok {
			break
		}
		if lock.Holder != nil && lock.Holder.Reason != "" {
			return false, desktopOSError(lock.Holder.Reason)
		}
		if !wait {
			return false, nil
		}
		if !announced {
			fmt.Fprintln(l.Out, "→ Waiting for a running Icebath update to release this checkout...")
			announced = true
		}
		time.Sleep(desktopBuildLockPoll)
	}
	l.checkout = lock
	return true, nil
}

// Releases the checkout lock this build took
func (l *DesktopBuildLock) releaseCheckout(ctx context.Context) {
	lock := l.checkout
	l.checkout = nil
	if lock != nil {
		lock.Release(ctx)
	}
}

// Opens and locks the build lock file, queueing behind its holder when waiting
func (l *DesktopBuildLock) acquireBuildLock(wait bool) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o777); err != nil {
		return false, err
	}
	handle, err := os.OpenFile(l.Path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return false, err
	}
	if gateway.TryAcquireFileLock(handle) {
		l.handle = handle
		return true, nil
	}
	if !wait {
		handle.Close()
		return false, nil
	}
	fmt.Fprintln(l.Out, "→ Waiting for another Icebath desktop dependency install or build to finish...")
	fmt.Fprintf(l.Out, "  Lock: %s\n", l.Path)
	supported, err := blockingFileLock(handle)
	if supported {
		if err != nil {
			handle.Close()
			return false, err
		}
		l.handle = handle
		return true, nil
	}
	for {
		if gateway.TryAcquireFileLock(handle) {
			l.handle = handle
			return true, nil
		}
		gateway.ReleaseFileLock(handle)
		time.Sleep(desktopBuildLockPoll)
	}
}

// Releases the build lock then the checkout lock, safe to call more than once
func (l *DesktopBuildLock) Release(ctx context.Context) {
	handle := l.handle
	if handle == nil {
		return
	}
	l.handle = nil
	gateway.ReleaseFileLock(handle)
	handle.Close()
	l.releaseCheckout(ctx)
}

// Takes the lock without waiting, an error when another holder has it
func (l *DesktopBuildLock) Enter(ctx context.Context) error {
	ok, err := l.Acquire(ctx, false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("desktop build lock is already held")
	}
	return nil
}

// OSError a desktop build raises with its message only
type desktopOSError string

func (e desktopOSError) Error() string      { return string(e) }
func (e desktopOSError) PyTypeName() string { return "OSError" }
