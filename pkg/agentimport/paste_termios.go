//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package agentimport

import (
	"bytes"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	"golang.org/x/sys/unix"
)

// Text pasted at a terminal up to Ctrl-D, read with line mode off so long lines are not cut
func readTerminalPaste(c *cliui.Console) (string, error) {
	c.PrintInfo("Press Ctrl-D once it has all pasted (Ctrl-Z then Enter on Windows).")
	fdp, ok := termutil.Fd(c.In)
	if !ok {
		raw, err := io.ReadAll(c.In)
		return string(raw), err
	}
	fd := int(fdp)
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", err
	}
	raw := *saved
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, ioctlSetTermios, saved)
	var chunks [][]byte
	buf := make([]byte, 65536)
	for len(chunks) == 0 || !bytes.Contains(chunks[len(chunks)-1], []byte{0x04}) {
		n, err := unix.Read(fd, buf)
		if err != nil && err != unix.EINTR {
			return "", err
		}
		if err == unix.EINTR {
			continue
		}
		chunks = append(chunks, append([]byte(nil), buf[:n]...))
		if n == 0 {
			break
		}
	}
	all := bytes.Join(chunks, nil)
	if i := bytes.IndexByte(all, 0x04); i >= 0 {
		all = all[:i]
	}
	return strings.ReplaceAll(strings.ToValidUTF8(string(all), "\uFFFD"), "\r", "\n"), nil
}

// Drops the rest of a paste still streaming in, flushing until the input stays quiet for half a second
func discardPendingInput(c *cliui.Console) {
	fdp, ok := termutil.Fd(c.In)
	if !ok || !c.StdinIsTTY() {
		return
	}
	for range 40 {
		fds := []unix.PollFd{{Fd: int32(fdp), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 500)
		for err == unix.EINTR {
			n, err = unix.Poll(fds, 500)
		}
		if err != nil {
			return
		}
		c.FlushStdin()
		if n == 0 {
			return
		}
	}
}
