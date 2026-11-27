package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

// JSON object whose keys are written in the order given
type ordered []field

type field struct {
	key   string
	value any
}

func (o ordered) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := raw(f.key)
		if err != nil {
			return nil, err
		}
		value, err := raw(f.value)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Copy with the key set to the value, in its place when it is there and last when it is not
func (o ordered) with(key string, value any) ordered {
	out := append(ordered(nil), o...)
	for i := range out {
		if out[i].key == key {
			out[i].value = value
			return out
		}
	}
	return append(out, field{key, value})
}

// Compact JSON of the value, text left unescaped
func raw(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Value as JSON the way hooks read it: ", " between items and ": " after keys, text left unescaped; with ascii every character past ASCII
// written as its \u escape
func encode(value any, ascii bool) (string, error) {
	compact, err := raw(value)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	in, escaped := false, false
	for _, r := range string(compact) {
		if in {
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				in = false
			}
			if ascii && r > 0x7f {
				for _, unit := range utf16.Encode([]rune{r}) {
					fmt.Fprintf(&out, `\u%04x`, unit)
				}
				continue
			}
			out.WriteRune(r)
			continue
		}
		switch r {
		case '"':
			in = true
		case ',':
			out.WriteString(", ")
			continue
		case ':':
			out.WriteString(": ")
			continue
		}
		out.WriteRune(r)
	}
	return out.String(), nil
}
