package charsetutil

import (
	"sync"
	"unicode/utf8"
)

// US-ASCII, bytes from 0x80 up undecodable
type asciiCodec struct{}

func (asciiCodec) decode(data []byte, replace bool) (string, int, string) {
	i := 0
	for i < len(data) && data[i] < utf8.RuneSelf {
		i++
	}
	if i == len(data) {
		return string(data), -1, ""
	}
	if !replace {
		return "", i, "byte above 0x7f"
	}
	out := make([]byte, 0, len(data)+2*(len(data)-i))
	out = append(out, data[:i]...)
	for _, c := range data[i:] {
		if c < utf8.RuneSelf {
			out = append(out, c)
		} else {
			out = append(out, replacement...)
		}
	}
	return string(out), -1, ""
}

func (asciiCodec) encode(text string) ([]byte, int) {
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return nil, i
		}
	}
	return []byte(text), -1
}

// Character set of one byte per character, U+FFFD the character of a byte it leaves undefined; x/text charmaps and byteTable both serve it
type byteSet interface {
	DecodeByte(b byte) rune
	EncodeRune(r rune) (byte, bool)
}

// Single byte set whose bytes 0x80 to 0x9f that the underlying table leaves undefined are the C1 control characters, as the ISO 8859 character sets registered for MIME carry them
type withC1 struct{ byteSet }

// Character of a byte, the C1 control for an undefined byte from 0x80 to 0x9f
func (s withC1) DecodeByte(b byte) rune {
	r := s.byteSet.DecodeByte(b)
	if r == utf8.RuneError && b >= 0x80 && b < 0xa0 {
		return rune(b)
	}
	return r
}

// Byte of a character, a C1 control encoding as its own byte where the underlying table leaves that byte undefined
func (s withC1) EncodeRune(r rune) (byte, bool) {
	if b, ok := s.byteSet.EncodeRune(r); ok {
		return b, true
	}
	if r >= 0x80 && r < 0xa0 && s.byteSet.DecodeByte(byte(r)) == utf8.RuneError {
		return byte(r), true
	}
	return 0, false
}

// Codec of a single byte character set
type singleCodec struct{ set byteSet }

func (c singleCodec) decode(data []byte, replace bool) (string, int, string) {
	out := make([]byte, 0, len(data)+len(data)/2)
	for i, b := range data {
		r := c.set.DecodeByte(b)
		if r == utf8.RuneError && !replace {
			return "", i, "undefined byte"
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out), -1, ""
}

func (c singleCodec) encode(text string) ([]byte, int) {
	out := make([]byte, 0, len(text))
	for i, r := range text {
		b, ok := c.set.EncodeRune(r)
		if !ok {
			return nil, i
		}
		out = append(out, b)
	}
	return out, -1
}

// Single byte character set read on first use from the string of its 256 characters, U+FFFE standing for an undefined byte
type byteTable struct {
	chars string
	once  sync.Once
	dec   [256]rune
	enc   map[rune]byte
}

func (t *byteTable) load() {
	t.once.Do(func() {
		i := 0
		for _, r := range t.chars {
			if i == len(t.dec) {
				break
			}
			if r == 0xfffe {
				r = utf8.RuneError
			}
			t.dec[i] = r
			i++
		}
		t.enc = make(map[rune]byte, len(t.dec))
		for b := len(t.dec) - 1; b >= 0; b-- {
			if r := t.dec[b]; r != utf8.RuneError {
				t.enc[r] = byte(b)
			}
		}
	})
}

// Character of a byte, U+FFFD for a byte the table leaves undefined
func (t *byteTable) DecodeByte(b byte) rune {
	t.load()
	return t.dec[b]
}

// Byte of a character, the lowest when two bytes share it, false when the table has none
func (t *byteTable) EncodeRune(r rune) (byte, bool) {
	t.load()
	b, ok := t.enc[r]
	return b, ok
}
