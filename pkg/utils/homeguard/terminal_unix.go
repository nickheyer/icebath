//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package homeguard

import "golang.org/x/sys/unix"

// Gives up the controlling terminal of this process, so neither it nor a process it starts can prompt on the developer's terminal or wait for input there.
// A session leader keeps its terminal: it is a test child started on a terminal of its own, and giving that up would hang up its session
func leaveTerminal() {
	if sid, err := unix.Getsid(0); err != nil || sid == unix.Getpid() {
		return
	}
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	_ = unix.IoctlSetInt(fd, unix.TIOCNOTTY, 0)
}
