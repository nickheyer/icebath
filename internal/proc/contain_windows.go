package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A started child held in a kill on close job, so the child and every descendant end together
type Tree struct {
	cmd  *exec.Cmd
	root Identity
	mu   sync.Mutex
	job  windows.Handle
}

// New job object with the given limit flags
func newJob(flags uint32) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = flags
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("set job limits: %w", err)
	}
	return job, nil
}

// Resumes every thread of a process created suspended
func resumeThreads(pid int) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("thread snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := 0
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if int(entry.OwnerProcessID) != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open thread %d: %w", entry.ThreadID, err)
		}
		_, err = windows.ResumeThread(th)
		windows.CloseHandle(th)
		if err != nil {
			return fmt.Errorf("resume thread %d: %w", entry.ThreadID, err)
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("pid %d has no thread to resume", pid)
	}
	return nil
}

// Assigns a suspended process to job, then lets its first instruction run inside it
func assignAndResume(job windows.Handle, p *os.Process) error {
	var assignErr error
	if err := p.WithHandle(func(h uintptr) { assignErr = windows.AssignProcessToJobObject(job, windows.Handle(h)) }); err != nil {
		return err
	}
	if assignErr != nil {
		return fmt.Errorf("assign pid %d to job: %w", p.Pid, assignErr)
	}
	return resumeThreads(p.Pid)
}

// Starts cmd suspended inside a new kill on close job, resuming it only once the job holds it
func startInJob(cmd *exec.Cmd, flags uint32) (windows.Handle, Identity, error) {
	job, err := newJob(flags)
	if err != nil {
		return 0, Identity{}, err
	}
	attrs(cmd).CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return 0, Identity{}, err
	}
	root, err := Identify(cmd.Process.Pid)
	if err == nil {
		err = assignAndResume(job, cmd.Process)
	}
	if err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		windows.CloseHandle(job)
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			waitErr = nil
		}
		return 0, Identity{}, errors.Join(err, killErr, waitErr)
	}
	return job, root, nil
}

// Starts cmd suspended, places it in a job that ends the whole tree when closed, then resumes it
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	job, root, err := startInJob(cmd, windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	if err != nil {
		return nil, err
	}
	return &Tree{cmd: cmd, root: root, job: job}, nil
}

// Terminates every process in the job at once
func (t *Tree) Kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return fmt.Errorf("pid %d: %w", t.root.PID, ErrNotFound)
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return fmt.Errorf("terminate job: %w", err)
	}
	return nil
}

// Waits for the child to exit, ends whatever it left in the job, then collects the child and its output
func (t *Tree) Wait() error {
	waitErr, endErr := t.wait()
	return errors.Join(waitErr, endErr)
}

// The child's own Wait result apart from a failure to end its leftovers
func (t *Tree) wait() (error, error) {
	exitErr := t.root.Wait(context.Background())
	t.mu.Lock()
	termErr := windows.TerminateJobObject(t.job, 1)
	closeErr := windows.CloseHandle(t.job)
	t.job = 0
	t.mu.Unlock()
	if termErr != nil {
		termErr = fmt.Errorf("end leftovers of pid %d: %w", t.root.PID, termErr)
	}
	return t.cmd.Wait(), errors.Join(exitErr, termErr, closeErr)
}
