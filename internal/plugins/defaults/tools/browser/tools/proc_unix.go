//go:build !windows

package tools

import (
	"io/fs"
	"os"
	"syscall"
)

func pidNamespace() (uint64, bool) {
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return uint64(st.Ino), ok
}

// A session of its own, so a shared profile's Chromium outlives the process that started it for its other holders
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// A folder owned by this user, closed to group and other
func ownedPrivate(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsDir() && int(st.Uid) == os.Getuid() && st.Mode&0o077 == 0
}

// Owned by this user with no group or other write bits
func ownedClosed(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && st.Mode&0o022 == 0
}

// Asks a child to end
func ask(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// A session of its own, so the user's debug browser outlives this process
func debugDetached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// SO_REUSEADDR on a probe socket before it binds
func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) }); err != nil {
		return err
	}
	return serr
}
