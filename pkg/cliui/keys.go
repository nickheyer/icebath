package cliui

import (
	"errors"
	"os"
	"os/signal"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// Terminal size changed while a prompt waited for input
type resizeEvent struct{}

// Text pasted as one block between the bracketed paste markers
type pasteEvent string

// How often a waiting prompt checks the terminal size
const resizePoll = 100 * time.Millisecond

// Decoder of terminal input into key presses, pastes and resizes for one prompt
type keyReader struct {
	c          *Console
	dec        uv.EventDecoder
	buf        []byte
	pasting    bool
	paste      strings.Builder
	sig        chan os.Signal
	rows, cols int
}

// Key reader over the console input that also hears an interrupt signal sent to the process
func (c *Console) newKeyReader() *keyReader {
	k := &keyReader{c: c}
	k.rows, k.cols, _ = c.termSize()
	if c.stdinTTY() {
		k.sig = make(chan os.Signal, 1)
		signal.Notify(k.sig, os.Interrupt)
	}
	return k
}

// Stops hearing the interrupt signal and returns the bytes read past the last event to the input
func (k *keyReader) close() {
	if k.sig != nil {
		signal.Stop(k.sig)
	}
	k.c.startPump()
	k.c.pump.unread(k.buf)
	k.buf = nil
}

// Next key press, paste or resize, with an interrupt or the end of input as the error
func (k *keyReader) next() (any, error) {
	for {
		if len(k.buf) == 0 {
			b, err := k.c.readByte(resizePoll, k.sig)
			if errors.Is(err, errTimeout) {
				if rows, cols, serr := k.c.termSize(); serr == nil && (rows != k.rows || cols != k.cols) {
					k.rows, k.cols = rows, cols
					return resizeEvent{}, nil
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			k.buf = append(k.buf, b)
		}
		k.buf = k.c.gatherBurst(k.buf)
		n, ev := k.dec.Decode(k.buf)
		if n <= 0 {
			n = 1
		}
		k.buf = k.buf[n:]
		switch e := ev.(type) {
		case uv.PasteStartEvent:
			k.pasting = true
			k.paste.Reset()
		case uv.PasteEndEvent:
			k.pasting = false
			return pasteEvent(k.paste.String()), nil
		case uv.PasteEvent:
			return pasteEvent(normalizeNewlines(e.Content)), nil
		case uv.KeyPressEvent:
			key := uv.Key(e)
			if !k.pasting {
				return key, nil
			}
			switch {
			case key.Code == uv.KeyEnter || isCtrl(key, 'j'):
				k.paste.WriteByte('\n')
			case key.Code == uv.KeyTab:
				k.paste.WriteByte('\t')
			case key.Text != "":
				k.paste.WriteString(key.Text)
			}
		}
	}
}

// Carriage returns and CRLF pairs as line feeds
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Collects bytes that belong to the same key burst so escape sequences decode whole
func (c *Console) gatherBurst(buf []byte) []byte {
	for {
		wait := time.Duration(0)
		if incompleteEscape(buf) {
			wait = 50 * time.Millisecond
		}
		b, err := c.readByte(wait, nil)
		if err != nil {
			return buf
		}
		buf = append(buf, b)
	}
}

// Reports whether the buffer ends inside an escape sequence
func incompleteEscape(buf []byte) bool {
	i := len(buf) - 1
	for i >= 0 && buf[i] != 0x1b {
		i--
	}
	if i < 0 {
		return false
	}
	tail := buf[i:]
	if len(tail) == 1 {
		return true
	}
	if tail[1] != '[' && tail[1] != 'O' {
		return false
	}
	if len(tail) == 2 {
		return true
	}
	last := tail[len(tail)-1]
	if tail[1] == 'O' {
		return false
	}
	return !(last >= 0x40 && last <= 0x7e) || (len(tail) == 3 && last == '[')
}

// Reports a key press of ctrl with the letter and no other modifier
func isCtrl(k uv.Key, letter rune) bool {
	return k.Code == letter && k.Mod&uv.ModCtrl != 0 && k.Mod&uv.ModAlt == 0
}
