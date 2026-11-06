//go:build !windows

package selfupdate

import (
	"context"
	"errors"
	"syscall"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// POSIX children run unsuspended, the Windows job binding has no counterpart here
func suspendedAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }

func inheritHandleAttr(uintptr) *syscall.SysProcAttr { return nil }

// No update job exists outside Windows, so a command that must join one is refused
func inheritableJobHandle() (uintptr, func(), error) {
	return 0, nil, errors.New("this platform has no update job to hand over")
}

// Runs git leading a session of its own under the owner watch, a timeout or cancel killing its whole group
func runOwnerWatched(ctx context.Context, argv []string, opts ChildOptions, watchOwner bool) (*CompletedProcess, error) {
	attr := &syscall.SysProcAttr{}
	if opts.Attr != nil {
		copied := *opts.Attr
		attr = &copied
	}
	attr.Setsid = true
	opts.Attr = attr
	child, err := popenChild(argv, opts)
	if err != nil {
		return nil, err
	}
	var dismiss func()
	if watchOwner {
		dismiss = updaterecord.OwnerWatch(child.cmd.Process.Pid)
	}
	defer func() {
		if dismiss != nil {
			dismiss()
		}
	}()
	return child.communicate(ctx, opts, func(c *startedChild) bool { killGroup(c); return true }, killGroup)
}

// Kills the process group the child leads, never once the child was reaped and its id may name another session
func killGroup(c *startedChild) {
	if c.wasReaped() {
		return
	}
	_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
}
