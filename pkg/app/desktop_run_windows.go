//go:build windows

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// Starts the desktop app detached from this console, outside the job object when the job allows it
func runDesktop(w io.Writer, exe string, args, env []string) (int, error) {
	start := func(flags uint32) error {
		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe)
		cmd.Env = append(os.Environ(), env...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	detached := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	err := start(detached | windows.CREATE_BREAKAWAY_FROM_JOB)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		err = start(detached)
	}
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(w, "✓ Icebath Desktop launched in a detached window; you can close this shell.")
	return 0, nil
}
