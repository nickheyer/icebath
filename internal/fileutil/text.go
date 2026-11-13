package fileutil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Byte order mark a UTF-8 text file may open with
const utf8BOM = "\xef\xbb\xbf"

// Text that is not valid UTF-8 and the byte offset where decoding fails
type UTF8Error struct {
	Path   string
	Offset int64
}

// Path and byte offset of the first invalid UTF-8 sequence
func (e *UTF8Error) Error() string {
	return fmt.Sprintf("%s is not valid UTF-8 text at byte %d", e.Path, e.Offset)
}

// Atomically writes UTF-8 text with platform line endings, replaced POSIX files ending 0600 and new ones at umask
func WriteText(ctx context.Context, path, text string, opts ...WriteOption) error {
	if !utf8.ValidString(text) {
		return &UTF8Error{Path: path, Offset: int64(firstInvalidUTF8(text))}
	}
	return writeStaged(ctx, path, []byte(nativeLineEndings(text)), newWriteConfig(writeConfig{prefix: ".tmp_", umaskNew: true}, opts))
}

// Byte offset of the first invalid UTF-8 sequence in s, len(s) when there is none
func firstInvalidUTF8(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return len(s)
}

// Whole UTF-8 text file with a leading byte order mark dropped and every CRLF or CR read as LF
func ReadText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	offset := int64(0)
	if bytes.HasPrefix(data, []byte(utf8BOM)) {
		data, offset = data[len(utf8BOM):], int64(len(utf8BOM))
	}
	if !utf8.Valid(data) {
		return "", &UTF8Error{Path: path, Offset: offset + int64(firstInvalidUTF8(string(data)))}
	}
	return universalNewlines(string(data)), nil
}

// CRLF and lone CR rewritten as LF
func universalNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// First n characters of a UTF-8 text file read the way ReadText reads the whole file, decoding no further
func ReadTextHead(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	offset := int64(0)
	if head, err := r.Peek(len(utf8BOM)); err == nil && string(head) == utf8BOM {
		if _, err := r.Discard(len(utf8BOM)); err != nil {
			return "", err
		}
		offset = int64(len(utf8BOM))
	}
	var out strings.Builder
	for count := 0; count < n; count++ {
		c, size, err := r.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if c == utf8.RuneError && size == 1 {
			return "", &UTF8Error{Path: path, Offset: offset}
		}
		offset += int64(size)
		if c == '\r' {
			if next, err := r.Peek(1); err == nil && next[0] == '\n' {
				if _, err := r.Discard(1); err != nil {
					return "", err
				}
				offset++
			}
			c = '\n'
		}
		out.WriteRune(c)
	}
	return out.String(), nil
}
