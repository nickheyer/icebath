package mailutil

import (
	"bytes"
	"slices"
	"strings"
)

// One header field: the name as written and the value with its folding removed, bytes kept as they arrived
type Field struct {
	Name  string
	Value string
}

// Fields of a message header in the order they appear, names matched without regard to case
type Header []Field

// First value of the field with invalid UTF-8 replaced by U+FFFD, empty when the field is absent
func (h Header) Get(name string) string {
	v, _ := h.Lookup(name)
	return v
}

// First value of the field as Get gives it and whether the field is present
func (h Header) Lookup(name string) (string, bool) {
	if v, ok := h.raw(name); ok {
		return validText(v), true
	}
	return "", false
}

// Every value of the field in order, invalid UTF-8 replaced by U+FFFD
func (h Header) Values(name string) []string {
	var out []string
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			out = append(out, validText(f.Value))
		}
	}
	return out
}

// First value of the field with RFC 2047 encoded words and raw 8 bit text decoded to UTF-8, empty when the field is absent
func (h Header) Text(name string) string {
	v, _ := h.raw(name)
	return DecodeHeader(v)
}

// Replaces the first field of the name and drops any later ones, or appends the field when there is none
func (h *Header) Set(name, value string) {
	fields := *h
	for i := range fields {
		if strings.EqualFold(fields[i].Name, name) {
			fields[i] = Field{Name: name, Value: value}
			tail := slices.DeleteFunc(fields[i+1:], func(f Field) bool { return strings.EqualFold(f.Name, name) })
			*h = fields[:i+1+len(tail)]
			return
		}
	}
	*h = append(fields, Field{Name: name, Value: value})
}

// Appends a field, fields of the same name kept before it
func (h *Header) Add(name, value string) { *h = append(*h, Field{Name: name, Value: value}) }

// First value of the field exactly as stored
func (h Header) raw(name string) (string, bool) {
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			return f.Value, true
		}
	}
	return "", false
}

// Fields of the header block at the start of raw, read leniently: mbox From lines skipped, continuation lines unfolded, the block ending at a blank line or at the first line that is not a field, fields past the first maxFields left out
func ParseHeader(raw []byte) Header {
	h, _ := splitHeader(raw, maxFields)
	return h
}

var mboxFrom = []byte("From ")

// Header fields at the start of raw, at most limit of them kept and the rest skipped, and the offset where the body after them begins
func splitHeader(raw []byte, limit int) (Header, int) {
	var h Header
	var value []byte
	open := false
	flush := func() {
		if open {
			h[len(h)-1].Value = string(bytes.Trim(value, " \t"))
			open = false
		}
	}
	for pos := 0; pos < len(raw); {
		line, next := lineAt(raw, pos)
		switch {
		case len(line) == 0:
			flush()
			return h, next
		case line[0] == ' ' || line[0] == '\t':
			if open {
				value = append(value, line...)
			}
		case line[0] == ':':
		default:
			name, rest, ok := fieldLine(line)
			flush()
			if !ok {
				if bytes.HasPrefix(line, mboxFrom) {
					break
				}
				return h, pos
			}
			if len(h) < limit {
				h = append(h, Field{Name: string(name)})
				value = append(value[:0], rest...)
				open = true
			}
		}
		pos = next
	}
	flush()
	return h, len(raw)
}

// Name and value of a field line: printable ASCII up to the colon, white space before the colon allowed, false for any other line
func fieldLine(line []byte) ([]byte, []byte, bool) {
	i := 0
	for i < len(line) && line[i] > ' ' && line[i] < 0x7f && line[i] != ':' {
		i++
	}
	if i == 0 {
		return nil, nil, false
	}
	j := i
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	if j == len(line) || line[j] != ':' {
		return nil, nil, false
	}
	return line[:i], line[j+1:], true
}

// Line starting at pos without its terminator, CRLF or LF, and the offset of the line after it; a CR not followed by LF is data, as RFC 5322 reads it, so a value cannot end its line early where a mail server sees none
func lineAt(b []byte, pos int) ([]byte, int) {
	rest := b[pos:]
	i := bytes.IndexByte(rest, '\n')
	if i < 0 {
		return rest, len(b)
	}
	line := rest[:i]
	if i > 0 && line[i-1] == '\r' {
		line = line[:i-1]
	}
	return line, pos + i + 1
}
