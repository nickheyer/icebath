//go:build !windows

package tools

import (
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Plain pipe for a server's stdin, whose write end is watched for the reader going away
func stdinPipe() (child, ours *os.File, err error) { return os.Pipe() }

// Calls onClose once the reader of a pipe's write end is gone, until the returned stop is called
func watchClose(f *os.File, onClose func()) (stop func()) {
	var fd int
	if raw, err := f.SyscallConn(); err != nil || raw.Control(func(p uintptr) { fd = int(p) }) != nil {
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

// Kills the group a server leads, never this process's own
func killGroup(pid int) {
	if pid > 0 && pid != syscall.Getpgrp() {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// Whether this process may run the file
func executable(path string) bool { return unix.Access(path, unix.X_OK) == nil }
