//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"syscall"
)

// POSIX half of the registry plumbing, runtime dir checks and host pid facts

// Accepts only an absolute, owned, non writable real directory
func secureUserRuntimeDir(path string) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return filepath.IsAbs(path) && st.IsDir() && int(sys.Uid) == os.Getuid() && st.Mode().Perm()&0o022 == 0
}

// Reports a unix socket at path owned by this user
func ownedUnixSocket(path string) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Mode()&os.ModeSocket != 0 && int(sys.Uid) == os.Getuid()
}

// Whether pid leads a process group of its own, never this process's group
func leadsOwnGroup(pid int) bool {
	pgid, err := syscall.Getpgid(pid)
	return err == nil && pgid == pid && pgid != syscall.Getpgrp()
}

// The taskkill tree kill is Windows only and never reached behind the isWindowsHost guard
func terminateHostPidWindows(pid int) {
	panic("terminateHostPidWindows is Windows only")
}

// Every POSIX host has a pseudo terminal multiplexer
func ptyAvailable() bool {
	_, err := os.Stat("/dev/ptmx")
	return err == nil
}
