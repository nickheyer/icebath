//go:build windows

package commands

import (
	"errors"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/tools"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
)

// ComSpec command line for the gateway string, a bytes command refused because Windows command lines are text
func gatewayProgram(command string, isBytes bool) (quickProgram, error) {
	if isBytes {
		return quickProgram{}, errors.New("bytes args is not allowed on Windows")
	}
	return ntShellProgram(command, 0)
}

// Hidden window program of the resolved command interpreter running the command line
func ntShellProgram(args string, flags uint32) (quickProgram, error) {
	cmd, err := executil.ShellCommand(args)
	if err != nil {
		return quickProgram{}, err
	}
	cmd.SysProcAttr.CreationFlags = flags
	return quickProgram{path: cmd.Path, argv: []string{cmd.Path}, attr: cmd.SysProcAttr}, nil
}

// Refuses a NUL in the program, its command line, the cwd or the environment, which CreateProcess cannot carry
func finishProgram(prog quickProgram, cwd string, env []string) (quickProgram, error) {
	texts := append([]string{prog.path, prog.attr.CmdLine, cwd}, env...)
	for _, s := range texts {
		if strings.IndexByte(s, 0) >= 0 {
			return prog, errEmbeddedNUL
		}
	}
	return prog, nil
}

// Environment block entries sorted case insensitively by name as CreateProcess wants them
func quickEnvList(child *tools.ChildEnv) []string {
	env := child.Vars
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

// Inherited standard input handle, an empty pipe when the process has none
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

// Spawn failure as CreateProcess reported it
func spawnError(err error, _ string) error {
	return err
}

// TerminateProcess of the shell, access denied ignored once the shell no longer runs
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
