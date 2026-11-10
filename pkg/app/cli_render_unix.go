//go:build unix

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

var osc11RGBRe = regexp.MustCompile(`rgb:([0-9a-fA-F]+)/([0-9a-fA-F]+)/([0-9a-fA-F]+)`)

// Reads the termios attributes of a descriptor
var termiosGet = func(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, tcGets) }

// Writes termios attributes to a descriptor with the given set request
var termiosSet = func(fd int, req uint, attrs *unix.Termios) error { return unix.IoctlSetTermios(fd, req, attrs) }

// Reports input waiting on the descriptor before the deadline, retrying interrupted polls
func fdReadable(fd int, until time.Time) bool {
	for {
		ms := time.Until(until).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(ms))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && n > 0
	}
}

// Terminal background via OSC 11 as #RRGGBB, fenced by a DA1 sentinel and skipped over SSH
func queryOSC11Background() (string, bool) {
	con := console()
	if !con.StdinIsTTY() || !con.StdoutIsTTY() {
		return "", false
	}
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if os.Getenv(v) != "" {
			return "", false
		}
	}
	in, ok := con.In.(*os.File)
	if !ok {
		return "", false
	}
	fd := int(in.Fd())
	old, err := termiosGet(fd)
	if err != nil {
		return "", false
	}
	cbreak := *old
	cbreak.Lflag &^= unix.ECHO | unix.ICANON
	cbreak.Cc[unix.VMIN] = 1
	cbreak.Cc[unix.VTIME] = 0
	if err := termiosSet(fd, tcSetsFlush, &cbreak); err != nil {
		return "", false
	}
	defer func() {
		_ = termiosSet(fd, tcSetsFlush, old)
		drain := time.Now().Add(50 * time.Millisecond)
		scratch := make([]byte, 64)
		for time.Now().Before(drain) {
			if !fdReadable(fd, drain) {
				break
			}
			if n, err := unix.Read(fd, scratch); err != nil || n == 0 {
				break
			}
		}
	}()
	if _, err := io.WriteString(con.Out, "\x1b]11;?\x1b\\\x1b[c"); err != nil {
		return "", false
	}
	deadline := time.Now().Add(time.Second)
	var buf []byte
	chunk := make([]byte, 64)
	for time.Now().Before(deadline) {
		if !fdReadable(fd, deadline) {
			continue
		}
		n, err := unix.Read(fd, chunk)
		if err != nil || n == 0 {
			break
		}
		buf = append(buf, chunk[:n]...)
		if da1ReplyRe.Match(buf) {
			break
		}
	}
	m := osc11RGBRe.FindSubmatch(buf)
	if m == nil {
		return "", false
	}
	norm := func(h []byte) uint64 {
		v, _ := strconv.ParseUint(string(h), 16, 64)
		bits := uint(len(h) * 4)
		if bits == 0 {
			return 0
		}
		return v * 255 / ((1 << bits) - 1)
	}
	return fmt.Sprintf("#%02X%02X%02X", norm(m[1]), norm(m[2]), norm(m[3])), true
}

// Re applies raw mode when the descriptor drifted back to cooked, false when already raw or not a tty
func healCookedModeDrift(fd int) bool {
	attrs, err := termiosGet(fd)
	if err != nil {
		return false
	}
	if attrs.Lflag&(unix.ICANON|unix.ECHO) == 0 {
		return false
	}
	attrs.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	attrs.Iflag &^= unix.IXON | unix.IXOFF | unix.ICRNL | unix.INLCR | unix.IGNCR
	attrs.Cc[unix.VMIN] = 1
	return termiosSet(fd, tcSetsNow, attrs) == nil
}
