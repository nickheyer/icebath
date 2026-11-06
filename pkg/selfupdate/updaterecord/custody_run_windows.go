package updaterecord

import (
	"context"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Runs git suspended until bound to the update job while the lock is held, plainly otherwise
func runGitPlatform(ctx context.Context, argv []string, timeout time.Duration, held bool) (*GitResult, error) {
	if !held {
		return runCaptured(ctx, argv, timeout, childSpawn{killTree: killTreeOf, interrupt: killChild})
	}
	return runCaptured(ctx, argv, timeout, childSpawn{
		attr: &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED},
		started: func(cmd *exec.Cmd) (func(), error) {
			if err := BindChildToUpdateTree(cmd.Process.Pid); err != nil {
				_ = cmd.Process.Kill()
				return nil, refuseChild(ctx, argv, err)
			}
			if err := ResumeSuspendedChild(cmd.Process); err != nil {
				return nil, err
			}
			custodyRun.Lock()
			custodyRun.ran = true
			custodyRun.Unlock()
			return nil, nil
		},
		killTree:  killTreeOf,
		interrupt: killChild,
	})
}

// Windows has no parent death signal, the update job kills its children with their owner
func DeathSignalAttr() *syscall.SysProcAttr { return nil }
