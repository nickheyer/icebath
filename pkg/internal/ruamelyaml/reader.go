package ruamelyaml

import (
	"bytes"
	"errors"
	"unicode/utf8"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
)

// Document handed to a loader, a Python str of code points or a Python bytes object
type Input struct {
	Text    []rune
	Bytes   []byte
	IsBytes bool
}

// Python str input, surrogates of the WTF-8 text kept as their own code points
func TextInput(s string) Input { return Input{Text: []rune(s)} }

// Python bytes input
func BytesInput(b []byte) Input { return Input{Bytes: b, IsBytes: true} }

// Character stream of the pure loader, decoded once with a trailing NUL
type reader struct {
	name     string
	buffer   []rune
	pointer  int
	index    int
	line     int
	column   int
	encoding string
}

// First character outside the printable YAML set and its position, false when every character is allowed
func firstNonPrintable(data []rune) (int, rune, bool) {
	for i, r := range data {
		switch {
		case r == 0x09 || r == 0x0A || r == 0x0D || (r >= 0x20 && r <= 0x7E) || r == 0x85:
		case r >= 0xA0 && r <= 0xD7FF, r >= 0xE000 && r <= 0xFFFD, r >= 0x10000 && r <= 0x10FFFF:
		default:
			return i, r, true
		}
	}
	return 0, 0, false
}

// Reader over a str or bytes input, the ReaderError of a failed decode or a non printable character
func newReader(in Input) (*reader, error) {
	r := &reader{}
	var data []rune
	if !in.IsBytes {
		r.name = "<unicode string>"
		data = in.Text
	} else {
		r.name = "<byte string>"
		raw := in.Bytes
		switch {
		case bytes.HasPrefix(raw, []byte{0xff, 0xfe}):
			r.encoding = "utf-16-le"
		case bytes.HasPrefix(raw, []byte{0xfe, 0xff}):
			r.encoding = "utf-16-be"
		default:
			r.encoding = "utf-8"
		}
		if r.encoding == "utf-8" {
			if pos, reason, bad := utf8Failure(raw); bad {
				return nil, &ReaderError{Name: r.name, Position: pos, Character: int(raw[pos]), Encoding: "utf-8", Reason: reason}
			}
			data = []rune(string(raw))
		} else {
			text, err := charsetutil.Decode(raw, r.encoding)
			if err != nil {
				var de *charsetutil.DecodeError
				if errors.As(err, &de) {
					return nil, &ReaderError{Name: r.name, Position: de.Offset, Character: int(raw[de.Offset]), Encoding: r.encoding, Reason: de.Reason}
				}
				return nil, err
			}
			data = []rune(text)
		}
	}
	if pos, ch, bad := firstNonPrintable(data); bad {
		return nil, &ReaderError{Name: r.name, Position: pos, Character: int(ch), Encoding: "unicode", Reason: "special characters are not allowed"}
	}
	r.buffer = make([]rune, len(data)+1)
	copy(r.buffer, data)
	return r, nil
}

// Start and CPython reason of the first strict utf-8 decode failure
func utf8Failure(raw []byte) (int, string, bool) {
	for i := 0; i < len(raw); {
		c, size := utf8.DecodeRune(raw[i:])
		if c == utf8.RuneError && size == 1 {
			reason := "invalid start byte"
			if b := raw[i]; b >= 0xC2 && b <= 0xF4 {
				n := 1
				for n < len(raw)-i && n < 4 && raw[i+n]&0xC0 == 0x80 {
					n++
				}
				reason = "invalid continuation byte"
				if i+n == len(raw) {
					reason = "unexpected end of data"
				}
			}
			return i, reason, true
		}
		i += size
	}
	return 0, "", false
}

// Character index ahead of the pointer, the IndexError of a read past the trailing NUL
func (r *reader) peek(index int) rune {
	if r.pointer+index >= len(r.buffer) {
		panic(&Exception{Class: "IndexError", Msg: "string index out of range"})
	}
	return r.buffer[r.pointer+index]
}

func (r *reader) prefix(length int) []rune {
	return sliceOf(r.buffer, r.pointer, r.pointer+length)
}

func (r *reader) prefixString(length int) string { return string(r.prefix(length)) }

// Moves the pointer counting lines on LF and lone CR, columns on every character but a BOM
func (r *reader) forward(length int) {
	for length != 0 {
		if r.pointer >= len(r.buffer) {
			panic(&Exception{Class: "IndexError", Msg: "string index out of range"})
		}
		ch := r.buffer[r.pointer]
		r.pointer++
		r.index++
		if ch == '\n' || (ch == '\r' && r.peekRaw() != '\n') {
			r.line++
			r.column = 0
		} else if ch != 0xFEFF {
			r.column++
		}
		length--
	}
}

func (r *reader) peekRaw() rune {
	if r.pointer >= len(r.buffer) {
		panic(&Exception{Class: "IndexError", Msg: "string index out of range"})
	}
	return r.buffer[r.pointer]
}

func (r *reader) getMark() *Mark {
	return &Mark{Name: r.name, Index: r.index, Line: r.line, Column: r.column, Buffer: r.buffer, Pointer: r.pointer, snippet: true}
}
