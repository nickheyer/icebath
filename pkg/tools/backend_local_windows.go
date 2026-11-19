//go:build windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/treekill"
)

// Windows reports no hard link count through FileInfo
func fileLinkCount(st os.FileInfo) uint64 { return 1 }

// Reports whether the directory can be listed
func dirSearchable(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Reports whether the directory can be written by creating and removing a probe file
func dirWritable(p string) bool {
	probe, err := os.CreateTemp(p, ".icebath-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return true
}

// User name of this process
func currentUID() string {
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "?"
}

// Marks the child as the leader of a new process tree without a console window, startCommand gives it its own job
func startNewSession(cmd *exec.Cmd) {
	hideConsoleWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// Starts cmd, a new tree leader inside a fresh tracked job it enters before running a single instruction
func startCommand(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		return cmd.Start()
	}
	job, err := treekill.NewJob(0)
	if err != nil {
		return err
	}
	if err := treekill.StartInJob(cmd, job); err != nil {
		return errors.Join(err, job.Close())
	}
	treekill.Track(cmd.Process.Pid, job)
	return nil
}

// Leader of the tracked tree holding pid
func processGroupOf(pid int) (int, error) { return treekill.LeaderOf(pid) }

// Creation time of a process in centiseconds since the epoch
func processStartTime(pid int) (int64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return creation.Nanoseconds() / 1e7, true
}

// Start time fingerprint of a live process
func ProcessStartTime(pid int) (int64, bool) { return processStartTime(pid) }

// Ends the tree pid leads, its whole job when it is tracked, else every descendant once the start time still matches
func terminateTree(pid int, expectedStart string) error {
	err := treekill.TerminateTree(pid, killedExitCode)
	if !errors.Is(err, treekill.ErrProcessGone) {
		return err
	}
	current, ok := processStartTime(pid)
	if !ok {
		return errNoSuchProcess
	}
	if expectedStart != "" {
		expected, err := strconv.ParseInt(expectedStart, 10, 64)
		if err != nil {
			return fmt.Errorf("refusing to force-kill PID %d; malformed start time", pid)
		}
		if expected <= 0 || current <= 0 || expected != current {
			return fmt.Errorf("refusing to force-kill PID %d; process identity changed", pid)
		}
	}
	if !treekill.KillProcessTree(pid, 0) {
		return fmt.Errorf("could not end the process tree of PID %d", pid)
	}
	return nil
}

// Ends the command's whole tree, its process alone when the tree cannot be ended, then reaps it
func killLocalProcessTree(p *ExecProcess) error {
	if err := terminateTree(p.Pid(), p.pgidStart); err != nil && !errors.Is(err, errNoSuchProcess) {
		p.Kill()
	}
	p.Wait(2)
	return nil
}

// Ends the command's whole tree with no wait for a host that exits next
func forceKillLocalProcessTree(p *ExecProcess) {
	if err := terminateTree(p.Pid(), p.pgidStart); err != nil && !errors.Is(err, errNoSuchProcess) {
		p.Kill()
	}
}
