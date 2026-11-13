package proc

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Signals the pid once a fresh start stamp read shows it still names id
func signalIdentity(id Identity, sig syscall.Signal) error { return checkedSignal(id, sig) }

// Signals the group id leads once a fresh start stamp read shows the leader still running it
func signalLedGroup(id Identity, sig syscall.Signal) error { return checkedGroupSignal(id, sig) }

// Waits on a kqueue exit note through the runtime poller, polling liveness only when the note cannot be armed
func waitExit(ctx context.Context, id Identity) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return pollExit(ctx, id)
	}
	var change unix.Kevent_t
	unix.SetKevent(&change, id.PID, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		unix.Close(kq)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return pollExit(ctx, id)
	}
	if err := unix.SetNonblock(kq, true); err != nil {
		unix.Close(kq)
		return pollExit(ctx, id)
	}
	f := os.NewFile(uintptr(kq), "kqueue")
	defer f.Close()
	if !id.Running() {
		return nil
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return pollExit(ctx, id)
	}
	stop := context.AfterFunc(ctx, func() { f.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	err = rc.Read(func(fd uintptr) bool {
		events := make([]unix.Kevent_t, 1)
		n, kerr := unix.Kevent(int(fd), nil, events, &unix.Timespec{})
		return kerr == nil && n > 0
	})
	if ctx.Err() != nil && id.Alive() {
		return ctx.Err()
	}
	if err != nil && id.Alive() {
		return pollExit(ctx, id)
	}
	return nil
}
