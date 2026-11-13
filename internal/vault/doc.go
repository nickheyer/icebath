package vault

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Decoding options keeping the last value of a repeated name and reading escaped lone surrogates as U+FFFD
var readOptions = []jsontext.Options{jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)}

// Layout every vault document and page script value is written in, the reference default with ASCII escapes
var referenceStyle = fileutil.JSONStyle{ASCII: true}

// JSON null literal
var nullValue = jsontext.Value("null")

// Error for text that is not exactly one JSON value
var errNotJSON = errors.New("not a JSON document")

// Name and value of one entry of a JSON object
type member struct {
	name  string
	value jsontext.Value
}

// Sole JSON value of UTF-8 text with nothing but whitespace around it
func parseValue(data []byte) (jsontext.Value, error) {
	if !utf8.Valid(data) {
		return nil, errNotJSON
	}
	dec := jsontext.NewDecoder(bytes.NewReader(data), readOptions...)
	v, err := dec.ReadValue()
	if err != nil {
		return nil, errNotJSON
	}
	v = v.Clone()
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errNotJSON
	}
	return v, nil
}

// Members of an object, a repeated name keeping its first position and last value, false for any other value
func objectMembers(v jsontext.Value) ([]member, bool) {
	if v.Kind() != jsontext.KindBeginObject {
		return nil, false
	}
	dec := jsontext.NewDecoder(bytes.NewReader(v), readOptions...)
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	var out []member
	for dec.PeekKind() == jsontext.KindString {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, false
		}
		value, err := dec.ReadValue()
		if err != nil {
			return nil, false
		}
		out = setMember(out, name.String(), value.Clone())
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	return out, true
}

// Elements of an array, false for any other value
func arrayItems(v jsontext.Value) ([]jsontext.Value, bool) {
	if v.Kind() != jsontext.KindBeginArray {
		return nil, false
	}
	dec := jsontext.NewDecoder(bytes.NewReader(v), readOptions...)
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	var out []jsontext.Value
	for dec.PeekKind() != jsontext.KindEndArray {
		item, err := dec.ReadValue()
		if err != nil {
			return nil, false
		}
		out = append(out, item.Clone())
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	return out, true
}

// Members with name set to value, replacing an earlier member in place or appending a new one
func setMember(ms []member, name string, value jsontext.Value) []member {
	for i := range ms {
		if ms[i].name == name {
			ms[i].value = value
			return ms
		}
	}
	return append(ms, member{name: name, value: value})
}

// Value of the member under name, an absent value when there is none
func lookup(ms []member, name string) jsontext.Value {
	for _, m := range ms {
		if m.name == name {
			return m.value
		}
	}
	return nil
}

// Members without the one under name, with the value it held
func takeMember(ms []member, name string) ([]member, jsontext.Value) {
	for i, m := range ms {
		if m.name == name {
			return append(ms[:i:i], ms[i+1:]...), m.value
		}
	}
	return ms, nil
}

// Decoded text of a string, the JSON text of a number, boolean or container, empty for null or absent
func valueText(v jsontext.Value) string {
	switch v.Kind() {
	case jsontext.KindString:
		s, _ := jsontext.AppendUnquote(nil, v)
		return string(s)
	case jsontext.KindNull, jsontext.KindInvalid:
		return ""
	}
	return string(v)
}

// Text of a truthy value, empty for a falsy or absent one
func truthyText(v jsontext.Value) string {
	if !truthy(v) {
		return ""
	}
	return valueText(v)
}

// Reports whether a value is true, a non-zero number, a non-empty string or a non-empty container
func truthy(v jsontext.Value) bool {
	switch v.Kind() {
	case jsontext.KindTrue:
		return true
	case jsontext.KindString:
		return len(v) > 2
	case jsontext.KindNumber:
		f, _ := strconv.ParseFloat(string(v), 64)
		return f != 0
	case jsontext.KindBeginObject, jsontext.KindBeginArray:
		return strings.TrimLeft(string(v[1:len(v)-1]), " \t\r\n") != ""
	}
	return false
}

// Reports whether a value is a number equal to n
func numberIs(v jsontext.Value, n float64) bool {
	if v.Kind() != jsontext.KindNumber {
		return false
	}
	f, err := strconv.ParseFloat(string(v), 64)
	return err == nil && f == n
}

// String value of s, invalid UTF-8 bytes written as U+FFFD
func stringValue(s string) jsontext.Value {
	b, _ := jsontext.AppendQuote(nil, s)
	return b
}

// String value of s, null when s is empty
func nullableValue(s string) jsontext.Value {
	if s == "" {
		return nullValue
	}
	return stringValue(s)
}

// Object value of members in order
func objectValue(ms ...member) jsontext.Value {
	b := []byte{'{'}
	for i, m := range ms {
		if i > 0 {
			b = append(b, ',')
		}
		b, _ = jsontext.AppendQuote(b, m.name)
		b = append(append(b, ':'), m.value...)
	}
	return append(b, '}')
}
