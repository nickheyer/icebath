package jsonutil

import (
	"bytes"
	"errors"
	"strconv"
	"unicode/utf8"
)

// Malformed JSON input with the place it was found
type SyntaxError struct {
	// What is wrong, without the position
	Msg string
	// Byte offset of the problem in the UTF-8 text read: past a leading byte order mark, and in the transcoded text for UTF-16 or UTF-32 input
	Offset int
	// Line of the problem, counted from 1
	Line int
	// Column of the problem in characters, counted from 1
	Column int
}

// The message with its line and column
func (e *SyntaxError) Error() string {
	return "json: " + e.Msg + " at line " + strconv.Itoa(e.Line) + ", column " + strconv.Itoa(e.Column)
}

// Reported by DecodeObject and UnmarshalJSON when the input holds a JSON value other than an object
var ErrNotObject = errors.New("json: value is not an object")

// Syntax error at offset of data with its line and column worked out
func syntaxError(data []byte, offset int, msg string) *SyntaxError {
	offset = min(max(offset, 0), len(data))
	head := data[:offset]
	line := 1 + bytes.Count(head, []byte{'\n'})
	if nl := bytes.LastIndexByte(head, '\n'); nl >= 0 {
		head = head[nl+1:]
	}
	return &SyntaxError{Msg: msg, Offset: offset, Line: line, Column: 1 + utf8.RuneCount(head)}
}

// Character at offset quoted for an error message, a stray byte shown in hex
func quoteAt(data []byte, offset int) string {
	if offset >= len(data) {
		return "end of input"
	}
	c := data[offset]
	switch c {
	case '\'':
		return `'\''`
	case '"':
		return `'"'`
	}
	if c < utf8.RuneSelf {
		q := strconv.Quote(string(rune(c)))
		return "'" + q[1:len(q)-1] + "'"
	}
	r, size := utf8.DecodeRune(data[offset:])
	if r == utf8.RuneError && size == 1 {
		return "byte 0x" + strconv.FormatUint(uint64(c), 16)
	}
	return strconv.QuoteRune(r)
}

// Name of the JSON kind of a decoded value for messages
func kindName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case int64, float64:
		return "a number"
	case []any:
		return "an array"
	case *Object, map[string]any:
		return "an object"
	}
	return "a value"
}
