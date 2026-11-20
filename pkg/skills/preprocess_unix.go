//go:build !windows

package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

// Leaves the child attached to the console since windows_hide_flags is zero off Windows
func hideInlineShellWindow(*exec.Cmd) {}

// Marker for a child chdir into cwd that would fail, empty when the directory is usable
func inlineShellCwdError(cwd string) string {
	st, err := os.Stat(cwd)
	if err == nil && !st.IsDir() {
		err = &fs.PathError{Op: "chdir", Path: cwd, Err: syscall.ENOTDIR}
	}
	if err == nil {
		err = syscall.Access(cwd, 1)
	}
	if err == nil {
		return ""
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return fmt.Sprintf("[inline-shell error: %v]", err)
	}
	if errno == syscall.ENOENT {
		return "[inline-shell error: bash not found]"
	}
	return fmt.Sprintf("[inline-shell error: %s]", spawnErrorText(errno, cwd))
}

// Popen returncode with the negated signal number for a signal killed child
func pyReturnCode(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}
