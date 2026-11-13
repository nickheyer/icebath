//go:build !windows

package plugins

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Hook child leads its own process group so a timeout reaps its descendants
func shellHookProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Resolves a bare command over the child PATH the way CPython execvpe ranks ENOENT below other errors
func shellHookExecutable(name string, env map[string]string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathVar, ok := env["PATH"]
	if !ok {
		pathVar = ":/bin:/usr/bin"
	}
	var saved error
	for _, dir := range filepath.SplitList(pathVar) {
		candidate := filepath.Join(dir, name)
		if dir == "" {
			candidate = name
		}
		fi, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if fi.IsDir() || unix.Access(candidate, unix.X_OK) != nil {
			if saved == nil {
				saved = &os.PathError{Op: "exec", Path: candidate, Err: syscall.EACCES}
			}
			continue
		}
		return candidate, nil
	}
	if saved != nil {
		return "", saved
	}
	return "", &os.PathError{Op: "exec", Path: name, Err: syscall.ENOENT}
}

// Popen.returncode of a finished child, negative signal numbers for signalled exits
func shellHookReturnCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return ps.ExitCode()
}

// Takes LOCK_EX on the sibling lock file, the release unlocking before the handle closes
func lockShellHookAllowlist(p string) (func(), error) {
	handle, err := os.OpenFile(p+".lock", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(handle.Fd()), unix.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EINTR) {
			_ = handle.Close()
			return nil, err
		}
	}
	return func() {
		flockUnlock(handle)
		_ = handle.Close()
	}, nil
}

// Drops the flock held on the handle, swallowing OS errors
func flockUnlock(handle *os.File) {
	_ = unix.Flock(int(handle.Fd()), unix.LOCK_UN)
}

// POSIX hosts never raise WinError 193
func isBadExeFormat(err error) bool { return false }

// os.access with X_OK when exec is set, else R_OK
func shellHookAccess(path string, exec bool) bool {
	mode := uint32(unix.R_OK)
	if exec {
		mode = unix.X_OK
	}
	return unix.Access(path, mode) == nil
}
