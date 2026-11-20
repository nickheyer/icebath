//go:build !windows

package provider

import (
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Reads one hidden line from the controlling terminal with getpass editing, checking ctx every tenth of a second
func awsReadHiddenLine(ctx context.Context, prompt string) (string, error) {
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
	hidden.Lflag &^= unix.ECHO | unix.ICANON
	hidden.Cc[unix.VMIN], hidden.Cc[unix.VTIME] = 0, 1
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
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			return "", err
		}
		switch c := buf[0]; c {
		case '\n', '\r':
			return string(line), nil
		case restore.Cc[unix.VERASE], '\b':
			_, size := utf8.DecodeLastRune(line)
			line = line[:len(line)-size]
		case restore.Cc[unix.VKILL]:
			line = line[:0]
		case restore.Cc[unix.VEOF]:
			if len(line) == 0 {
				return "", io.EOF
			}
		default:
			line = append(line, c)
		}
	}
}
