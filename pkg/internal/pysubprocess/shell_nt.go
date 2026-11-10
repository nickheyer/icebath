package pysubprocess

import (
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// ErrNtShellNotFound is the FileNotFoundError Popen raises when neither ComSpec nor SystemRoot names a shell
var ErrNtShellNotFound = errors.New("shell not found: neither %ComSpec% nor %SystemRoot% is set")

// Executable and command line Popen hands CreateProcess for shell=True, an empty executable leaving CreateProcess to search
func NtShell(args string, getenv func(string) string) (string, string, error) {
	executable := ""
	comspec := getenv("ComSpec")
	if comspec == "" {
		comspec = pypath.NtJoin(getenv("SystemRoot"), "System32", "cmd.exe")
		if !pypath.NtIsAbs(comspec) {
			return "", "", ErrNtShellNotFound
		}
	}
	if pypath.NtIsAbs(comspec) {
		executable = comspec
	}
	return executable, comspec + ` /c "` + args + `"`, nil
}

// Search places CreateProcess walks for a module named without a directory
type NtSearch struct {
	AppDir     string
	Cwd        string
	SkipCwd    bool
	SystemDir  string
	WindowsDir string
	Path       string
}

// Module CreateProcess starts for a command line without an application name, false when no candidate exists
func NtCommandLineModule(cmdline string, s NtSearch, isFile func(string) bool) (string, bool) {
	if strings.HasPrefix(cmdline, `"`) {
		if end := strings.IndexByte(cmdline[1:], '"'); end >= 0 {
			return ntFindExe(cmdline[1:1+end], s, isFile)
		}
	}
	i := 0
	for {
		for i < len(cmdline) && cmdline[i] != ' ' && cmdline[i] != '\t' {
			i++
		}
		if found, ok := ntFindExe(cmdline[:i], s, isFile); ok {
			return found, true
		}
		if i >= len(cmdline) {
			return "", false
		}
		for i < len(cmdline) && (cmdline[i] == ' ' || cmdline[i] == '\t') {
			i++
		}
	}
}

// SearchPath with the .exe default extension over the CreateProcess search order
func ntFindExe(name string, s NtSearch, isFile func(string) bool) (string, bool) {
	if name == "" {
		return "", false
	}
	if _, ext := pypath.NtSplitext(pypath.NtBasename(name)); ext == "" {
		name += ".exe"
	}
	if strings.ContainsAny(name, `\/:`) {
		full := name
		if !pypath.NtIsAbs(name) {
			full = pypath.NtJoin(s.Cwd, name)
		}
		return full, isFile(full)
	}
	dirs := []string{s.AppDir}
	if !s.SkipCwd {
		dirs = append(dirs, s.Cwd)
	}
	dirs = append(dirs, s.SystemDir, pypath.NtJoin(s.WindowsDir, "System"), s.WindowsDir)
	dirs = append(dirs, strings.Split(s.Path, ";")...)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if full := pypath.NtJoin(dir, name); isFile(full) {
			return full, true
		}
	}
	return "", false
}
