//go:build !windows

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
)

// Runs the desktop app attached to this terminal and returns its exit status, Ctrl-C closing it
func runDesktop(w io.Writer, exe string, args, env []string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	fmt.Fprintln(w, "→ Launching Icebath Desktop: "+exe)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	err := cmd.Wait()
	select {
	case <-interrupts:
		fmt.Fprintln(w, "\n✓ Icebath Desktop closed.")
		return 0, nil
	default:
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}
