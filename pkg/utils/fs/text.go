package fsutil

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Byte order mark a UTF-8 text file may start with
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// Bytes of a text file that are not UTF-8, at the offset from the start of the file where decoding failed
type TextDecodeError struct {
	Path   string
	Offset int64
}

// The file and the offset, as in notes.txt: invalid UTF-8 at byte 17
func (e *TextDecodeError) Error() string {
	return fmt.Sprintf("%s: invalid UTF-8 at byte %d", e.Path, e.Offset)
}

// First limit characters of the UTF-8 text file at path, all of it for math.MaxInt, a leading byte order mark dropped and \r\n and a lone \r read as \n; bytes that are not UTF-8 within what it reads give a *TextDecodeError
func ReadText(path string, limit int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	if head, _ := r.Peek(len(utf8BOM)); bytes.Equal(head, utf8BOM) {
		r.Discard(len(utf8BOM))
		offset = int64(len(utf8BOM))
	}
	var b strings.Builder
	if st, err := f.Stat(); err == nil && st.Size() > 0 && limit > 0 {
		b.Grow(int(min(st.Size(), int64(limit))))
	}
	for n := 0; n < limit; n++ {
		c, size, err := r.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if c == utf8.RuneError && size == 1 {
			return "", &TextDecodeError{Path: path, Offset: offset}
		}
		offset += int64(size)
		if c == '\r' {
			if next, err := r.Peek(1); err == nil && next[0] == '\n' {
				r.Discard(1)
				offset++
			}
			c = '\n'
		}
		b.WriteRune(c)
	}
	return b.String(), nil
}
