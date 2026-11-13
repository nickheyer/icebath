package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Pidfd of the incarnation id, ErrReused or ErrNotFound when the pid no longer names it
func openPidfd(id Identity) (int, error) {
	fd, err := unix.PidfdOpen(id.PID, 0)
	if err != nil {
		return -1, signalError(id.PID, err)
	}
	start, err := Birth(id.PID)
	if err == nil && start != id.Start {
		err = fmt.Errorf("pid %d: %w", id.PID, ErrReused)
	}
	if err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// Signals through a pidfd opened and checked against id, so a pid reused after the check is never hit
func signalIdentity(id Identity, sig syscall.Signal) error {
	fd, err := openPidfd(id)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrReused), errors.Is(err, ErrAccessDenied):
		return err
	case err != nil:
		return checkedSignal(id, sig)
	}
	defer unix.Close(fd)
	return signalError(id.PID, unix.PidfdSendSignal(fd, sig, nil, 0))
}

// Signals the group id leads through its pidfd, which kernels before 6.9 refuse, the checked group number serving there
func signalLedGroup(id Identity, sig syscall.Signal) error {
	fd, err := openPidfd(id)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrReused), errors.Is(err, ErrAccessDenied):
		return err
	case err != nil:
		return checkedGroupSignal(id, sig)
	}
	defer unix.Close(fd)
	if led, err := unix.Getpgid(id.PID); err != nil {
		return signalError(id.PID, err)
	} else if led != id.PID {
		return fmt.Errorf("pid %d no longer leads its process group", id.PID)
	}
	err = unix.PidfdSendSignal(fd, sig, nil, unix.PIDFD_SIGNAL_PROCESS_GROUP)
	if errors.Is(err, unix.EINVAL) {
		return checkedGroupSignal(id, sig)
	}
	return groupError(id.PID, err)
}

// Waits on a pidfd through the runtime poller, polling liveness only on kernels without pidfds
func waitExit(ctx context.Context, id Identity) error {
	fd, err := openPidfd(id)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrReused):
		return nil
	case err != nil:
		return pollExit(ctx, id)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return pollExit(ctx, id)
	}
	f := os.NewFile(uintptr(fd), "pidfd")
	defer f.Close()
	rc, err := f.SyscallConn()
	if err != nil {
		return pollExit(ctx, id)
	}
	stop := context.AfterFunc(ctx, func() { f.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	err = rc.Read(func(fd uintptr) bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, perr := unix.Poll(fds, 0)
		return perr == nil && n > 0
	})
	if ctx.Err() != nil && id.Alive() {
		return ctx.Err()
	}
	if err != nil && id.Alive() {
		return pollExit(ctx, id)
	}
	return nil
}
