//go:build unix

package app

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"time"

	"golang.org/x/sys/unix"
)

var osc11RGBRe = regexp.MustCompile(`rgb:([0-9a-fA-F]+)/([0-9a-fA-F]+)/([0-9a-fA-F]+)`)

// Reads the termios attributes of a descriptor
var termiosGet = func(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, tcGets) }

// Writes termios attributes to a descriptor with the given set request
var termiosSet = func(fd int, req uint, attrs *unix.Termios) error { return unix.IoctlSetTermios(fd, req, attrs) }

// Reports input waiting on the descriptor before the deadline, retrying interrupted polls and returning poll failures
func fdReadable(fd int, until time.Time) (bool, error) {
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
		if err != nil {
			return false, err
		}
		if n > 0 && fds[0].Revents&unix.POLLNVAL != 0 {
			return false, unix.EBADF
		}
		return n > 0, nil
	}
}

// Reads up to the buffer length from the descriptor, retrying interrupted reads
func readRetrying(fd int, buf []byte) (int, error) {
	for {
		n, err := unix.Read(fd, buf)
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

// Color component scaled from its hex digit width onto 0 to 255
func osc11Component(h []byte) int64 {
	v, _ := new(big.Int).SetString(string(h), 16)
	bits := uint(len(h) * 4)
	if bits == 0 {
		return 0
	}
	full := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(1))
	return new(big.Int).Quo(v.Mul(v, big.NewInt(255)), full).Int64()
}

// Terminal background via OSC 11 as #RRGGBB, fenced by a DA1 sentinel and skipped over SSH, a poll failure returned
func queryOSC11Background() (string, bool, error) {
	con := console()
	if !con.StdinIsTTY() || !con.StdoutIsTTY() {
		return "", false, nil
	}
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if os.Getenv(v) != "" {
			return "", false, nil
		}
	}
	in, ok := con.In.(*os.File)
	if !ok {
		return "", false, nil
	}
	fd := int(in.Fd())
	old, err := termiosGet(fd)
	if err != nil {
		return "", false, nil
	}
	cbreak := *old
	defer func() {
		_ = termiosSet(fd, tcSetsFlush, old)
		drain := time.Now().Add(50 * time.Millisecond)
		scratch := make([]byte, 64)
		for time.Now().Before(drain) {
			if ready, err := fdReadable(fd, drain); err != nil || !ready {
				break
			}
			if n, err := readRetrying(fd, scratch); err != nil || n == 0 {
				break
			}
		}
	}()
	cbreak.Lflag &^= unix.ECHO | unix.ICANON
	cbreak.Cc[unix.VMIN] = 1
	cbreak.Cc[unix.VTIME] = 0
	if err := termiosSet(fd, tcSetsFlush, &cbreak); err != nil {
		return "", false, nil
	}
	if _, err := io.WriteString(con.Out, "\x1b]11;?\x1b\\\x1b[c"); err != nil {
		return "", false, nil
	}
	deadline := time.Now().Add(time.Second)
	var buf []byte
	chunk := make([]byte, 64)
	for time.Now().Before(deadline) {
		ready, err := fdReadable(fd, deadline)
		if err != nil {
			return "", false, err
		}
		if !ready {
			continue
		}
		n, err := readRetrying(fd, chunk)
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
		return "", false, nil
	}
	return fmt.Sprintf("#%02X%02X%02X", osc11Component(m[1]), osc11Component(m[2]), osc11Component(m[3])), true, nil
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
