package app

import (
	"io"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Routes stdout, stderr and piped stdin through UTF-8 replacing wrappers, errors replace on every text stream
func reconfigureStream() {
	c := console()
	stdinTTY, stdoutTTY := c.StdinIsTTY(), c.StdoutIsTTY()
	wrapped := &cliui.Console{In: c.In, Out: &utf8ReplaceWriter{w: c.Out}, Getenv: c.Getenv, TermSize: c.TermSize,
		MaskedSecretPrompt: c.MaskedSecretPrompt, OpenScreen: c.OpenScreen,
		StdinTTY: func() bool { return stdinTTY }, StdoutTTY: func() bool { return stdoutTTY }}
	if !stdinTTY {
		wrapped.In = &utf8ReplaceReader{r: c.In}
	}
	SetStreams(wrapped, &utf8ReplaceWriter{w: stderrWriter()})
}

// Writer printing every invalid UTF-8 byte as a question mark and carrying an incomplete trailing rune
type utf8ReplaceWriter struct {
	w     io.Writer
	carry []byte
}

func (u *utf8ReplaceWriter) Write(p []byte) (int, error) {
	buf := append(u.carry, p...)
	u.carry = nil
	out := make([]byte, 0, len(buf))
	for i := 0; i < len(buf); {
		r, size := utf8.DecodeRune(buf[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(buf[i:]) {
				u.carry = append([]byte(nil), buf[i:]...)
				break
			}
			out = append(out, '?')
			i++
			continue
		}
		out = append(out, buf[i:i+size]...)
		i += size
	}
	if _, err := u.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Reader decoding invalid UTF-8 bytes as U+FFFD and carrying an incomplete trailing rune until more input arrives
type utf8ReplaceReader struct {
	r       io.Reader
	pending []byte
	carry   []byte
	err     error
}

func (u *utf8ReplaceReader) Read(p []byte) (int, error) {
	for len(u.pending) == 0 {
		if u.err != nil {
			err := u.err
			u.err = nil
			return 0, err
		}
		buf := make([]byte, 4096)
		n, err := u.r.Read(buf)
		data := append(u.carry, buf[:n]...)
		u.carry = nil
		var out []byte
		for i := 0; i < len(data); {
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size == 1 {
				if !utf8.FullRune(data[i:]) && err == nil {
					u.carry = append([]byte(nil), data[i:]...)
					break
				}
				out = append(out, "�"...)
				i++
				continue
			}
			out = append(out, data[i:i+size]...)
			i += size
		}
		u.pending = out
		u.err = err
	}
	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}
