//go:build !windows

package lsp

import (
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Leaves a POSIX child without a console flag to set
func hideWindow(cmd *exec.Cmd) {}

// Gives the child its own session and process group
func newSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// Python os.access with X_OK
func accessExec(p string) bool { return syscall.Access(p, 1) == nil }

// Plain pipe for a child's stdin, the watcher polling our write end for reader loss
func stdinPipe() (child, ours *os.File, err error) { return os.Pipe() }

// Calls onClose once when the reader of a pipe write end goes away, until the returned stop is called
func watchPipeClose(f *os.File, onClose func()) (stop func()) {
	var fd int
	if rc, err := f.SyscallConn(); err != nil || rc.Control(func(p uintptr) { fd = int(p) }) != nil {
		return func() {}
	}
	wake := make([]int, 2)
	if err := unix.Pipe(wake); err != nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			fds := []unix.PollFd{{Fd: int32(fd)}, {Fd: int32(wake[0]), Events: unix.POLLIN}}
			if _, err := unix.Poll(fds, -1); err != nil {
				if err == unix.EINTR {
					continue
				}
				return
			}
			if fds[1].Revents != 0 {
				return
			}
			if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				onClose()
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			unix.Write(wake[1], []byte{0})
			<-done
			unix.Close(wake[0])
			unix.Close(wake[1])
		})
	}
}

// The stdin watcher reports reader loss, so a process exit alone does not close the pipe
const exitClosesStdin = false
