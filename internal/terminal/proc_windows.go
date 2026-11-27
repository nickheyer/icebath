//go:build windows

package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/term"
)

// Windows ends a tree through its process list, there are no groups to signal
const groups = false

const noFollow = 0

// Windows processes never report a signal
func signalOf(*os.ProcessState) int { return 0 }

func brokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}

var peekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

// One chunk read once the pipe holds bytes, idle for the timeout otherwise, an error on a broken pipe
func readChunk(f *os.File, buf []byte, timeout time.Duration) (int, bool, error) {
	var available uint32
	if r, _, err := peekNamedPipe.Call(f.Fd(), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0); r == 0 {
		return 0, false, err
	}
	if available == 0 {
		time.Sleep(timeout)
		return 0, false, nil
	}
	n, err := f.Read(buf[:min(int(available), len(buf))])
	if n > 0 {
		return n, true, nil
	}
	if err == nil {
		err = io.EOF
	}
	return 0, false, err
}

// Starts the command as the root of a process tree of its own, without a console window
func NewSession(cmd *exec.Cmd) {
	proc.HideWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

func groupOf(pid int) (int, error) { return pid, nil }

// Creation time of a process in centiseconds
func StartTime(pid int) (int64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0, false
	}
	return created.Nanoseconds() / 1e7, true
}

const stillActive = 259

// Whether the process is still running
func Running(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

func zombie(int) bool { return false }

// Asks the process to end, its windows closed
func terminate(pid int) { taskkill(pid, false, false) }

func kill(pid int) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}

// Ends the process, and its tree when tree is set, through taskkill
func taskkill(pid int, tree, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"/PID", strconv.Itoa(pid)}
	if tree {
		args = append(args, "/T")
	}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.CommandContext(ctx, "taskkill", args...)
	proc.HideWindow(cmd)
	return cmd.Run()
}

// Whether the process the tree hangs off is gone, Windows having no groups to ask
func GroupGone(pgid int) bool { return !Running(pgid) }

// Ends the tree rooted at the process, at once when forced; ErrGone when it is not running
func SignalGroup(pgid int, force bool) error {
	if !Running(pgid) {
		return ErrGone
	}
	return taskkill(pgid, true, force)
}

// No process of the group or pid is left
var ErrGone = errors.New("The process is no longer running.")

func leadsGroup(int) bool { return false }

// Kills the process and every descendant
func killTree(p *HostProc) error {
	pid := p.Pid()
	descendants := term.Descendants(pid)
	if err := taskkill(pid, true, true); err != nil {
		p.Kill()
	}
	for _, d := range descendants {
		kill(d)
	}
	return nil
}

func forceKillTree(p *HostProc) { killTree(p) }

// Windows reports no inode, its creation time standing in for the change time
func fileIdentity(path string) (uint64, int64) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return 0, sys.CreationTime.Nanoseconds()
	}
	return 0, 0
}

func linkCount(os.FileInfo) uint64 { return 1 }

// Whether the folder can be listed
func Searchable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Whether a file can be made in the folder
func Writable(path string) bool {
	probe, err := os.CreateTemp(path, ".icebath-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return true
}

var createPseudoConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole")

// ConPTY exists from Windows 10 1809 on
func ptyAvailable() bool { return createPseudoConsole.Find() == nil }
