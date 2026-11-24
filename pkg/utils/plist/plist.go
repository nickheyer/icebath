package plistutil

import (
	"bytes"
	"strconv"
	"strings"
)

// Reference to an object of a keyed archive, a CF$UID entry
type UID uint64

// Failure to read or write a property list, Line naming the XML line it happened on when known
type Error struct {
	Msg  string
	Line int
	Err  error
}

// Message prefixed with plist and the line, followed by the cause
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("plist: ")
	if e.Line > 0 {
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(e.Line))
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Cause of the failure, nil when there is none
func (e *Error) Unwrap() error { return e.Err }

// Leading bytes of a binary property list in the format this package reads
const binaryMagic = "bplist00"

// Deepest nesting of arrays and dictionaries accepted
const maxDepth = 1024

// Value of an XML or binary property list: map[string]any, []any, string, int64 (uint64 above its range), float64, bool, []byte, time.Time, UID, nil for a binary null
func Decode(data []byte) (any, error) {
	if bytes.HasPrefix(data, []byte(binaryMagic)) {
		return decodeBinary(data)
	}
	if bytes.HasPrefix(data, []byte("bplist")) {
		return nil, &Error{Msg: "unsupported binary format " + strconv.Quote(string(data[:min(len(data), 8)]))}
	}
	text, transcoded, err := utf8Document(data)
	if err != nil {
		return nil, err
	}
	if trimmed := bytes.TrimLeft(text, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '<' {
		return nil, &Error{Msg: "not a property list"}
	}
	return decodeXML(text, transcoded)
}
