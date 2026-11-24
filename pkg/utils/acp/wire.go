package acputil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// Index just past the string literal whose opening quote is at i; it jumps from quote to quote, a quote after an odd run of backslashes being escaped, so long strings such as base64 data cost little
func skipString(b []byte, i int) int {
	start := i + 1
	for j := start; j < len(b); {
		k := bytes.IndexByte(b[j:], '"')
		if k < 0 {
			break
		}
		quote := j + k
		run := 0
		for p := quote - 1; p >= start && b[p] == '\\'; p-- {
			run++
		}
		if run%2 == 0 {
			return quote + 1
		}
		j = quote + 1
	}
	return len(b)
}

// Index just past the JSON value starting at i
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return i
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	}
	for i < len(b) {
		switch b[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// Members of the JSON object text in data in document order, each key unescaped beside the raw text of its value; nothing for text that is not an object, and the walk stops where the text stops being well formed
func members(data []byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		i := skipSpace(data, 0)
		if i >= len(data) || data[i] != '{' {
			return
		}
		i++
		for {
			i = skipSpace(data, i)
			if i >= len(data) || data[i] != '"' {
				return
			}
			end := skipString(data, i)
			if end-1 <= i || data[end-1] != '"' {
				return
			}
			key := data[i+1 : end-1]
			if bytes.IndexByte(key, '\\') >= 0 {
				var s string
				if json.Unmarshal(data[i:end], &s) != nil {
					return
				}
				key = []byte(s)
			}
			i = skipSpace(data, end)
			if i >= len(data) || data[i] != ':' {
				return
			}
			start := skipSpace(data, i+1)
			i = skipValue(data, start)
			if !yield(key, data[start:i]) {
				return
			}
			i = skipSpace(data, i)
			if i >= len(data) || data[i] != ',' {
				return
			}
			i++
		}
	}
}

// Raw text of the last member named key, nil when there is none
func member(data []byte, key string) []byte {
	var found []byte
	for k, v := range members(data) {
		if string(k) == key {
			found = v
		}
	}
	return found
}

// Whether the object carries key with a value other than null
func hasMember(data []byte, key string) bool {
	raw := member(data, key)
	return raw != nil && !isNull(raw)
}

func isNull(raw []byte) bool { return string(bytes.TrimSpace(raw)) == "null" }

func isObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// Text of a JSON string literal and whether raw is one
func unquote(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1]), true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Value of the discriminator member key, the raw text when it is not a string, and whether it is present and not null
func tagOf(data []byte, key string) (string, bool) {
	raw := member(data, key)
	if raw == nil || isNull(raw) {
		return "", false
	}
	if s, ok := unquote(raw); ok {
		return s, true
	}
	return string(raw), true
}

// Word for the kind of JSON value raw holds, for messages
func describe(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "nothing"
	}
	switch raw[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}

// Decodes the JSON object text data into v, every member named in required having to be present and not null
func decodeObject(data []byte, v any, typeName string, required ...string) error {
	if isNull(data) {
		return fmt.Errorf("%s must be an object, not null", typeName)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	return checkRequired(data, typeName, required)
}

// A *MissingFieldError for the first name in required the object data lacks or holds null in
func checkRequired(data []byte, typeName string, required []string) error {
	if len(required) == 0 {
		return nil
	}
	var seen uint64
	for key, value := range members(data) {
		if isNull(value) {
			continue
		}
		for i, name := range required {
			if string(key) == name {
				seen |= 1 << i
				break
			}
		}
	}
	for i, name := range required {
		if seen&(1<<i) == 0 {
			return &MissingFieldError{Type: typeName, Field: name}
		}
	}
	return nil
}

// JSON text of the struct v with the discriminator member key set to value after its other members
func withTag(v any, key, value string) ([]byte, error) {
	out, err := jsonutil.Append(nil, v)
	if err != nil {
		return nil, err
	}
	out = out[:len(out)-1]
	if len(out) > 1 {
		out = append(out, ',')
	}
	out = strconv.AppendQuote(out, key)
	out = append(out, ':')
	out = strconv.AppendQuote(out, value)
	return append(out, '}'), nil
}

// Each raw item decoded by one, nil for nil items, a failure naming the field and the index
func decodeEach[T any](items []json.RawMessage, field string, one func([]byte) (T, error)) ([]T, error) {
	if items == nil {
		return nil, nil
	}
	out := make([]T, len(items))
	for i, raw := range items {
		v, err := one(raw)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", field, i, err)
		}
		out[i] = v
	}
	return out, nil
}

// One JSON value decoded into a T
func decodeValue[T any](data []byte) (T, error) {
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}

// Value of raw JSON decoded with objects as *jsonutil.Object, nil when absent or null
func dynamic(raw json.RawMessage) (any, error) {
	if raw == nil || isNull(raw) {
		return nil, nil
	}
	return jsonutil.Decode(raw)
}
