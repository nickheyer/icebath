//go:build windows

package pysubprocess

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// Command Popen runs for shell=True, ComSpec /c with the quoted string and a hidden window
func ShellCommand(command string) (*exec.Cmd, error) {
	path, cmdline, err := WindowsShell(command)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CmdLine: cmdline}
	return cmd, nil
}

// Program path and command line of shell=True, a relative ComSpec found the way CreateProcess searches
func WindowsShell(args string) (string, string, error) {
	executable, cmdline, err := NtShell(args, os.Getenv)
	if err != nil || executable != "" {
		return executable, cmdline, err
	}
	path, err := WindowsCommandLineModule(cmdline)
	return path, cmdline, err
}

// Module CreateProcess starts for a command line without an application name, ERROR_FILE_NOT_FOUND when none exists
func WindowsCommandLineModule(cmdline string) (string, error) {
	if found, ok := NtCommandLineModule(cmdline, windowsSearch(), isRegularFile); ok {
		return found, nil
	}
	return "", &fs.PathError{Op: "fork/exec", Path: cmdline, Err: windows.ERROR_FILE_NOT_FOUND}
}

// Search places of this process, the NoDefaultCurrentDirectoryInExePath variable dropping the current directory
func windowsSearch() NtSearch {
	s := NtSearch{Path: os.Getenv("PATH")}
	if exe, err := os.Executable(); err == nil {
		s.AppDir = filepath.Dir(exe)
	}
	s.Cwd, _ = os.Getwd()
	_, s.SkipCwd = os.LookupEnv("NoDefaultCurrentDirectoryInExePath")
	s.SystemDir, _ = windows.GetSystemDirectory()
	s.WindowsDir, _ = windows.GetWindowsDirectory()
	return s
}

func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
