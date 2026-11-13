//go:build !windows

package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// A started child leading its own process group, so the child and every descendant can be ended at once
type Tree struct {
	cmd  *exec.Cmd
	root Identity
}

// Starts cmd leading a new process group, or a new session when the caller already asked for one
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	attr := attrs(cmd)
	if attr.Setpgid && attr.Pgid != 0 {
		return nil, fmt.Errorf("a contained child must lead its own process group, not join group %d", attr.Pgid)
	}
	attr.Setpgid = !attr.Setsid
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	root, err := Identify(cmd.Process.Pid)
	if err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			waitErr = nil
		}
		return nil, errors.Join(fmt.Errorf("identify started pid %d: %w", cmd.Process.Pid, err), killErr, waitErr)
	}
	return &Tree{cmd: cmd, root: root}, nil
}

// Kills the child, its descendants including those that left the group, and every group member
func (t *Tree) Kill() error { return SignalTree(t.root, syscall.SIGKILL) }

// Waits for the child to exit, kills its group leftovers while the uncollected child holds the number, then collects it
func (t *Tree) Wait() error {
	waitErr, endErr := t.wait()
	return errors.Join(waitErr, endErr)
}

// The child's own Wait result apart from a failure to end its leftovers
func (t *Tree) wait() (error, error) {
	exitErr := t.root.Wait(context.Background())
	endErr := signalLedGroup(t.root, syscall.SIGKILL)
	if errors.Is(endErr, ErrNotFound) && !GroupExists(t.root.PID) {
		endErr = nil
	}
	return t.cmd.Wait(), errors.Join(exitErr, endErr)
}
