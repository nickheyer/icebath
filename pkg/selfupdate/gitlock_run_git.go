package selfupdate

import (
	"context"
	"errors"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Runs an updater git for the tools git lock in the update's custody, the run_git the process wiring assigns
func GitlockRunGit(ctx context.Context, gitCmd, args []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
	opts := ChildOptions{Dir: o.Dir, Env: o.Env, Capture: true, Timeout: o.Timeout, Check: o.Check, Attr: o.Attr}
	if o.Input != nil {
		in := string(o.Input)
		opts.Input = &in
	}
	done, err := RunGit(ctx, gitCmd, args, opts)
	var completed *pysubprocess.Completed
	if done != nil {
		completed = &pysubprocess.Completed{Args: done.Args, Returncode: done.ReturnCode, Stdout: []byte(done.Stdout), Stderr: []byte(done.Stderr)}
	}
	if err == nil {
		return completed, nil
	}
	var timeout *updaterecord.GitTimeout
	if errors.As(err, &timeout) {
		return nil, &pysubprocess.TimeoutExpired{Cmd: timeout.Args, Timeout: int(timeout.Timeout / time.Second)}
	}
	var called *CalledProcessError
	if errors.As(err, &called) {
		return completed, &pysubprocess.CalledProcessError{Cmd: called.Cmd, Returncode: called.ReturnCode, Stdout: []byte(called.Output), Stderr: []byte(called.Stderr)}
	}
	return completed, err
}
