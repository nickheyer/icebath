//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
)

// Binds the git runner of the tools git lock sweeps to the updater custody run
func init() { tools.GitlockRunGit = GitlockRunGit }

// Runs an updater git for the tools git lock in the update's custody, the run_git the process wiring assigns
func GitlockRunGit(ctx context.Context, gitCmd, args []string, o executil.Options) (*executil.Completed, error) {
	opts := ChildOptions{Dir: o.Dir, Env: o.Env, Capture: true, Timeout: o.Timeout, Check: o.RequireSuccess, Attr: o.Attr}
	if o.Input != nil {
		in := string(o.Input)
		opts.Input = &in
	}
	done, err := RunGit(ctx, gitCmd, args, opts)
	var completed *executil.Completed
	if done != nil {
		completed = &executil.Completed{Args: done.Args, ExitCode: done.ReturnCode, Stdout: []byte(done.Stdout), Stderr: []byte(done.Stderr)}
	}
	if err == nil {
		return completed, nil
	}
	var timeout *updaterecord.GitTimeout
	if errors.As(err, &timeout) {
		return nil, &executil.TimeoutError{Args: timeout.Args, Timeout: time.Duration(int(timeout.Timeout/time.Second)) * time.Second}
	}
	var called *CalledProcessError
	if errors.As(err, &called) {
		return completed, &executil.StatusError{Args: called.Cmd, ExitCode: called.ReturnCode, Stdout: []byte(called.Output), Stderr: []byte(called.Stderr)}
	}
	return completed, err
}
