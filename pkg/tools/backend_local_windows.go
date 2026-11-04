//go:build windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
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

// Starts the child in a new process group without a console window
func startNewSession(cmd *exec.Cmd) {
	hideConsoleWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// Windows has no process groups to query
func processGroupOf(pid int) (int, error) {
	return pid, nil
}

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

// Terminates a pid through taskkill with the start time identity guard
func terminatePid(pid int, force bool, expectedStart string) error {
	if force {
		if expectedStart == "" {
			return fmt.Errorf("refusing to force-kill PID %d without a process start-time guard", pid)
		}
		current, ok := processStartTime(pid)
		if !ok {
			return fmt.Errorf("refusing to force-kill PID %d; process start time is unavailable", pid)
		}
		expected, err := strconv.ParseInt(expectedStart, 10, 64)
		if err != nil {
			return fmt.Errorf("refusing to force-kill PID %d; malformed start time", pid)
		}
		if expected <= 0 || current <= 0 || expected != current {
			return fmt.Errorf("refusing to force-kill PID %d; process identity changed", pid)
		}
	}
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.Command("taskkill", args...)
	hideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			details := strings.TrimSpace(string(out))
			if details == "" {
				details = fmt.Sprintf("taskkill failed for PID %d", pid)
			}
			return errors.New(details)
		}
		return err
	}
	return nil
}

// Identity checked forced tree kill, falling back to a plain kill
func killLocalProcessTree(p *ExecProcess) error {
	start, ok := processStartTime(p.Pid())
	expected := ""
	if ok {
		expected = strconv.FormatInt(start, 10)
	}
	if err := terminatePid(p.Pid(), true, expected); err != nil {
		p.Kill()
	}
	p.Wait(2)
	return nil
}

// Windows tree kills are already forced
func forceKillLocalProcessTree(p *ExecProcess) {
	killLocalProcessTree(p)
}
