//go:build !windows

package provider

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Writes the prompt to the controlling terminal and reads one line with echo off
func awsReadHiddenLine(prompt string) (string, error) {
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return "", errors.New("provider: no controlling terminal to read the MFA code from")
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, []byte(prompt)); err != nil {
		return "", err
	}
	old, err := unix.IoctlGetTermios(fd, awsIoctlGetTermios)
	if err != nil {
		return "", err
	}
	restore, hidden := *old, *old
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, awsIoctlSetTermiosFlush, &hidden); err != nil {
		return "", err
	}
	defer func() {
		_ = unix.IoctlSetTermios(fd, awsIoctlSetTermiosFlush, &restore)
		_, _ = unix.Write(fd, []byte("\n"))
	}()
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := unix.Read(fd, buf)
		if n <= 0 || err != nil || buf[0] == '\n' || buf[0] == '\r' {
			break
		}
		line = append(line, buf[0])
	}
	return string(line), nil
}
