//go:build !windows

package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// A hook's command leads a process group of its own, so a timeout ends its children with it
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Interpreter a script needs before it, none on this system, which runs scripts by their shebang
func (r *Registrant) interpreter(context.Context, []string) ([]string, error) {
	return nil, nil
}

// Program of a bare command on the PATH of its environment, a match that may not be run reported over one that is missing
func executable(name string, env map[string]string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path, ok := env["PATH"]
	if !ok {
		path = ":/bin:/usr/bin"
	}
	var refused error
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		if dir == "" {
			candidate = name
		}
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() || unix.Access(candidate, unix.X_OK) != nil {
			if refused == nil {
				refused = &os.PathError{Op: "exec", Path: candidate, Err: syscall.EACCES}
			}
			continue
		}
		return candidate, nil
	}
	if refused != nil {
		return "", refused
	}
	return "", &os.PathError{Op: "exec", Path: name, Err: syscall.ENOENT}
}

// No such failure on this system
func badFormat(error) bool { return false }

// Whether the file may be run, or read when it is a script an interpreter runs
func usable(path string, run bool) bool {
	mode := uint32(unix.R_OK)
	if run {
		mode = unix.X_OK
	}
	return unix.Access(path, mode) == nil
}
