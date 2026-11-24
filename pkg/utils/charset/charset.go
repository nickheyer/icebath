package charsetutil

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Error of a character set name that no character set answers to
var ErrUnknown = errors.New("unknown character set")

// Bytes that are not valid text in a character set, found at a byte offset of the input
type DecodeError struct {
	Charset string // canonical name of the character set
	Offset  int    // offset of the first byte of the sequence that does not decode, always inside the input
	Reason  string // what is wrong with that sequence
}

// Message naming the offset, the character set and the reason
func (e *DecodeError) Error() string {
	return fmt.Sprintf("cannot decode byte %d as %s: %s", e.Offset, e.Charset, e.Reason)
}

// Text holding a character the character set has no bytes for, or text that is not valid UTF-8
type EncodeError struct {
	Charset string // canonical name of the character set
	Offset  int    // byte offset of the character in the text
	Char    rune   // the character, -1 when the text is not valid UTF-8 at Offset
}

// Message naming the character, its offset and the character set
func (e *EncodeError) Error() string {
	if e.Char < 0 {
		return fmt.Sprintf("cannot encode as %s: invalid UTF-8 at byte %d", e.Charset, e.Offset)
	}
	return fmt.Sprintf("cannot encode %U at byte %d as %s", e.Char, e.Offset, e.Charset)
}

// Text of bytes in the named character set, a *DecodeError for bytes it cannot decode or ErrUnknown for an unknown name
func Decode(data []byte, name string) (string, error) {
	cs, err := lookup(name)
	if err != nil {
		return "", err
	}
	text, bad, reason := cs.codec.decode(data, false)
	if bad >= 0 {
		return "", &DecodeError{Charset: cs.name, Offset: bad, Reason: reason}
	}
	return text, nil
}

// Text of bytes in the named character set with every undecodable sequence turned into U+FFFD, the error only ErrUnknown for an unknown name
func DecodeReplace(data []byte, name string) (string, error) {
	cs, err := lookup(name)
	if err != nil {
		return "", err
	}
	text, _, _ := cs.codec.decode(data, true)
	return text, nil
}

// Bytes of text in the named character set, an *EncodeError for a character it cannot encode or ErrUnknown for an unknown name
func Encode(text, name string) ([]byte, error) {
	cs, err := lookup(name)
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(text) {
		return nil, &EncodeError{Charset: cs.name, Offset: firstInvalidUTF8(text), Char: -1}
	}
	out, bad := cs.codec.encode(text)
	if bad >= 0 {
		r, _ := utf8.DecodeRuneInString(text[bad:])
		return nil, &EncodeError{Charset: cs.name, Offset: bad, Char: r}
	}
	return out, nil
}

// Canonical name of a character set label in any spelling, false when no character set answers to it
func Canonical(name string) (string, bool) {
	cs, ok := find(name)
	if !ok {
		return "", false
	}
	return cs.name, true
}

// How one character set turns bytes into text and text into bytes
type codec interface {
	// Text of data and -1, or the offset of the first sequence that does not decode and why; with replace set every such sequence becomes U+FFFD and the offset is -1
	decode(data []byte, replace bool) (string, int, string)
	// Bytes of valid UTF-8 text and -1, or nil and the byte offset of the first character the set has no bytes for
	encode(text string) ([]byte, int)
}

// One character set, its canonical name and its codec
type charset struct {
	name  string
	codec codec
}

func lookup(name string) (*charset, error) {
	if cs, ok := find(name); ok {
		return cs, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknown, name)
}

func firstInvalidUTF8(text string) int {
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && n == 1 {
			return i
		}
		i += n
	}
	return len(text)
}
