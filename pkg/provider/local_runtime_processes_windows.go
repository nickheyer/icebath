//go:build windows

package provider

import (
	"context"
	"errors"
	"os/exec"

	"github.com/nickheyer/icebath/pkg/internal/treekill"
)

// WindowsJob is the owner held kill on close job object that contains a router tree
type WindowsJob = treekill.Job

// Creates an unnamed non inheritable job that kills its tree on close and allows no breakaway
var newWindowsJob = func() (*WindowsJob, error) { return treekill.NewJob(treekill.KillOnClose) }

// Places the suspended process into the job through a handle opened while the pid is still held
var assignWindowsJob = func(job *WindowsJob, cmd *exec.Cmd) error { return job.Assign(cmd.Process.Pid) }

// Resumes every thread of a process started suspended
var resumeProcessFn = treekill.ResumePid

// Starts cmd suspended inside a fresh job then resumes it and tracks the tree, a failed setup kills the child and frees every handle
func SpawnServer(ctx context.Context, cmd *exec.Cmd, owned bool) (*WindowsJob, error) {
	job, err := newWindowsJob()
	if err != nil {
		return nil, err
	}
	enter := func(int) error { return assignWindowsJob(job, cmd) }
	if err := treekill.StartSuspended(cmd, enter, resumeProcessFn); err != nil {
		return nil, errors.Join(err, job.Close())
	}
	treekill.Track(cmd.Process.Pid, job)
	return job, nil
}
