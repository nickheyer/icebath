//go:build windows

package commands

import (
	"errors"
	"os"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// ComSpec command line for the gateway string, bytes refused as Popen refuses them on Windows
func gatewayProgram(command string, isBytes bool) (quickProgram, error) {
	if isBytes {
		return quickProgram{}, pyError("bytes args is not allowed on Windows")
	}
	return ntShellProgram(command, 0)
}

// ComSpec command line for the CLI command with CREATE_NO_WINDOW, a sequence joined by list2cmdline
func cliProgram(command any) (quickProgram, error) {
	var args string
	switch c := command.(type) {
	case string:
		args = c
	case []byte:
		return quickProgram{}, pyError("bytes args is not allowed on Windows")
	default:
		items, err := pyListItems(command)
		if err != nil {
			return quickProgram{}, err
		}
		parts := make([]string, len(items))
		for i, item := range items {
			switch v := item.(type) {
			case string:
				parts[i] = v
			case []byte:
				parts[i] = string(v)
			default:
				return quickProgram{}, notPathLike(item)
			}
		}
		args = pysubprocess.List2cmdline(parts)
	}
	return ntShellProgram(args, windows.CREATE_NO_WINDOW)
}

// Hidden window ComSpec program, an empty path left for CreateProcess to search once the strings convert
func ntShellProgram(args string, flags uint32) (quickProgram, error) {
	executable, cmdline, err := pysubprocess.NtShell(args, os.Getenv)
	if err != nil {
		return quickProgram{}, err
	}
	return quickProgram{path: executable, argv: []string{executable},
		attr: &syscall.SysProcAttr{HideWindow: true, CmdLine: cmdline, CreationFlags: flags}}, nil
}

// ValueError CreateProcess raises for a NUL in its strings, then the search a relative ComSpec leaves to it
func finishProgram(prog quickProgram, cwd string, env []string) (quickProgram, error) {
	texts := append([]string{prog.path, prog.attr.CmdLine, cwd}, env...)
	for _, s := range texts {
		if strings.IndexByte(s, 0) >= 0 {
			return prog, pyError("embedded null character")
		}
	}
	if prog.path != "" {
		return prog, nil
	}
	path, err := pysubprocess.WindowsCommandLineModule(prog.attr.CmdLine)
	if err != nil {
		return prog, pyError(pypath.OSErrorText(err))
	}
	prog.path, prog.argv = path, []string{path}
	return prog, nil
}

// Environment block entries sorted case insensitively by name as CreateProcess wants them
func quickEnvList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := strings.ToUpper(keys[i]), strings.ToUpper(keys[j])
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + env[k]
	}
	return out
}

// Inherited standard input handle, an empty pipe when the process has none as Popen creates one
func quickStdin() (*os.File, func()) {
	if h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE); err == nil && h != 0 && os.Stdin != nil {
		return os.Stdin, func() {}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, func() {}
	}
	w.Close()
	return r, func() { r.Close() }
}

// Python str of a CreateProcess failure, the WinError code and message without a filename
func spawnErrorText(err error, _ string) string {
	return pypath.OSErrorText(err)
}

// Popen.kill as TerminateProcess, a PermissionError ignored once the shell no longer runs
func killShell(q *quickProc) error {
	if closed(q.exited) {
		return nil
	}
	err := q.proc.Kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	active, qerr := stillActive(q.proc.Pid)
	if qerr != nil {
		return qerr
	}
	if active {
		return err
	}
	return nil
}

// Whether GetExitCodeProcess still reports STILL_ACTIVE for the pid, with the error a failed query raises
func stillActive(pid int) (bool, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false, err
	}
	return code == 259, nil
}

// Windows has no process group to signal, taskkill /T already walked the tree
func killQuickGroup(int) {}

// After a timeout kill communicate drains both pipes to EOF and waits for the shell without a bound
func reapAfterTimeoutKill(q *quickProc) {
	<-q.stdout.done
	<-q.stderr.done
	<-q.exited
}
