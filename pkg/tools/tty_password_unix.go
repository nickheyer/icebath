//go:build !windows

package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

// Reads one line from /dev/tty with echo disabled, empty on any failure
func readHiddenLine() string {
	fd, err := unix.Open("/dev/tty", unix.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer unix.Close(fd)
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return ""
	}
	restore := *old
	hidden := *old
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, &hidden); err != nil {
		return ""
	}
	defer unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, &restore)
	var chars []byte
	buf := make([]byte, 1)
	for {
		n, err := unix.Read(fd, buf)
		if n <= 0 || err != nil || buf[0] == '\n' || buf[0] == '\r' {
			break
		}
		chars = append(chars, buf[0])
	}
	return termDecodeUTF8(chars)
}

// Flushes stdout the way the hermes prompt does after every status line
func flushStdout() {
	os.Stdout.Sync()
}
