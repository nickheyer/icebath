//go:build windows

package executil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Path of the command interpreter: %ComSpec%, else cmd.exe under %SystemRoot%\System32, a relative ComSpec looked up in the system directories and PATH
func Shell() (string, error) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		if !filepath.IsAbs(comspec) {
			return "", fmt.Errorf("%w: ComSpec is unset and SystemRoot is not an absolute directory", ErrNoShell)
		}
	}
	if filepath.IsAbs(comspec) {
		return comspec, nil
	}
	return findShell(comspec)
}

// Command that runs a command line through the command interpreter as ComSpec /c "command", without a console window
func ShellCommand(command string) (*exec.Cmd, error) {
	shell, err := Shell()
	if err != nil {
		return nil, err
	}
	program := shell
	if strings.ContainsAny(shell, " \t") {
		program = `"` + shell + `"`
	}
	cmd := exec.Command(shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CmdLine: program + ` /c "` + command + `"`}
	return cmd, nil
}

// Absolute path of a relative ComSpec, .exe added when it has no extension: one naming a directory resolves from the current directory, a bare name is searched in the system directory, the Windows directory and the absolute PATH entries
func findShell(name string) (string, error) {
	if filepath.Ext(name) == "" {
		name += ".exe"
	}
	if strings.ContainsAny(name, `\/:`) {
		if path, err := filepath.Abs(name); err == nil && isFile(path) {
			return path, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	dirs := make([]string, 0, 8)
	if dir, err := windows.GetSystemDirectory(); err == nil {
		dirs = append(dirs, dir)
	}
	if dir, err := windows.GetWindowsDirectory(); err == nil {
		dirs = append(dirs, dir)
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		if path := filepath.Join(dir, name); isFile(path) {
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
