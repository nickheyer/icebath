//go:build windows

package tools

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func pidNamespace() (uint64, bool) { return 0, false }

func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// Windows folders are checked as folders only
func ownedPrivate(info fs.FileInfo) bool { return info.Mode().IsDir() }

func ownedClosed(fs.FileInfo) bool { return true }

func ask(p *os.Process) error { return p.Kill() }

// A detached process in a group of its own, so the user's debug browser outlives this process
func debugDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}

// SO_REUSEADDR on a probe socket before it binds
func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
