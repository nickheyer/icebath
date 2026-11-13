package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Neither ComSpec nor SystemRoot names a command interpreter
var ErrNoShell = errors.New("shell not found: neither %ComSpec% nor %SystemRoot% is set")

// Process attributes of cmd, created when unset
func attrs(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// Makes the child lead a new console process group, which Ctrl+C from the shared console no longer reaches
func NewGroup(cmd *exec.Cmd) { attrs(cmd).CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP }

// Gives the child a hidden console that every console descendant inherits, so none of them flashes a window
func Hide(cmd *exec.Cmd) { attrs(cmd).CreationFlags |= windows.CREATE_NO_WINDOW }

// Whether a detached child escapes this process's job, true outside any job and when the job permits breakaway
func BreakawayAllowed() (bool, error) {
	var inJob int32
	if r, _, err := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob))); r == 0 {
		return false, fmt.Errorf("job membership: %w", err)
	}
	if inJob == 0 {
		return true, nil
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return false, fmt.Errorf("job limits: %w", err)
	}
	return info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0, nil
}

// Starts cmd in a new hidden console process group, breaking away from this process's job whenever the job permits
func StartDetached(cmd *exec.Cmd) error {
	a := attrs(cmd)
	a.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
	breakaway, err := BreakawayAllowed()
	if err != nil {
		return err
	}
	if breakaway {
		a.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	return cmd.Start()
}

// Command that runs line through the ComSpec interpreter as cmd /c with the line passed verbatim and no window
func ShellCommand(line string) (*exec.Cmd, error) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		if !filepath.IsAbs(comspec) {
			return nil, ErrNoShell
		}
	}
	cmdline := comspec + ` /c "` + line + `"`
	path := comspec
	if !filepath.IsAbs(comspec) {
		found, err := commandLineModule(cmdline)
		if err != nil {
			return nil, err
		}
		path = found
	}
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CmdLine: cmdline}
	return cmd, nil
}

// Directories CreateProcess searches for a module named without a directory, in its order
func moduleSearchDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if _, skip := os.LookupEnv("NoDefaultCurrentDirectoryInExePath"); !skip {
		if cwd, err := os.Getwd(); err == nil {
			dirs = append(dirs, cwd)
		}
	}
	if sys, err := windows.GetSystemDirectory(); err == nil {
		dirs = append(dirs, sys)
	}
	if win, err := windows.GetWindowsDirectory(); err == nil {
		dirs = append(dirs, filepath.Join(win, "System"), win)
	}
	return append(dirs, strings.Split(os.Getenv("PATH"), ";")...)
}

// Whether path names an existing file that is not a directory
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// Module CreateProcess loads for name, the .exe extension supplied when the name has none
func findModule(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if filepath.Ext(filepath.Base(name)) == "" {
		name += ".exe"
	}
	if strings.ContainsAny(name, `\/:`) {
		full := name
		if !filepath.IsAbs(name) {
			cwd, err := os.Getwd()
			if err != nil {
				return "", false
			}
			full = filepath.Join(cwd, name)
		}
		return full, isFile(full)
	}
	for _, dir := range moduleSearchDirs() {
		if dir == "" {
			continue
		}
		if full := filepath.Join(dir, name); isFile(full) {
			return full, true
		}
	}
	return "", false
}

// Module CreateProcess starts for a command line without an application name, trying each blank delimited prefix
func commandLineModule(cmdline string) (string, error) {
	missing := &fs.PathError{Op: "fork/exec", Path: cmdline, Err: windows.ERROR_FILE_NOT_FOUND}
	if strings.HasPrefix(cmdline, `"`) {
		if end := strings.IndexByte(cmdline[1:], '"'); end >= 0 {
			if found, ok := findModule(cmdline[1 : 1+end]); ok {
				return found, nil
			}
			return "", missing
		}
	}
	i := 0
	for {
		for i < len(cmdline) && cmdline[i] != ' ' && cmdline[i] != '\t' {
			i++
		}
		if found, ok := findModule(cmdline[:i]); ok {
			return found, nil
		}
		if i >= len(cmdline) {
			return "", missing
		}
		for i < len(cmdline) && (cmdline[i] == ' ' || cmdline[i] == '\t') {
			i++
		}
	}
}
