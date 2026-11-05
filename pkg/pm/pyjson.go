package pm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Decodes JSON the way json loads does, numbers normalized to Python's own spelling and NaN or Infinity accepted
func loadJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(pyJSONSource(trimBOM(data))))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("extra data after JSON value")
	}
	return normalizePyJSON(v), nil
}

// pyFloatSpecial is a NaN or infinite float, spelled NaN, Infinity or -Infinity
type pyFloatSpecial string

// Prefix of the string sentinels that carry NaN and Infinity tokens through Go's strict decoder
const specialSentinel = "\x00\x00pyjson-special:"

// Rewrites bare NaN, Infinity and -Infinity tokens outside strings into sentinel strings
func pyJSONSource(in []byte) []byte {
	if !bytes.Contains(in, []byte("NaN")) && !bytes.Contains(in, []byte("Infinity")) {
		return in
	}
	out := make([]byte, 0, len(in)+32)
	inString, escaped := false, false
	for i := 0; i < len(in); i++ {
		b := in[i]
		if inString {
			out = append(out, b)
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			out = append(out, b)
			continue
		}
		for _, token := range []string{"-Infinity", "Infinity", "NaN"} {
			if bytes.HasPrefix(in[i:], []byte(token)) {
				out = append(out, []byte(`"\u0000\u0000pyjson-special:`+token+`"`)...)
				i += len(token) - 1
				b = 0
				break
			}
		}
		if b != 0 {
			out = append(out, b)
		}
	}
	return out
}

// Normalizes decoded numbers to Python int and float spellings and restores NaN and Infinity values
func normalizePyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalizePyJSON(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalizePyJSON(x)
		}
		return t
	case string:
		if strings.HasPrefix(t, specialSentinel) {
			return pyFloatSpecial(strings.TrimPrefix(t, specialSentinel))
		}
		return t
	case json.Number:
		return pyNumberText(t)
	}
	return v
}

// Python spelling of a decoded number, integers as int text and others as float repr
func pyNumberText(n json.Number) any {
	text := n.String()
	if !strings.ContainsAny(text, ".eE") {
		if text == "-0" {
			return json.Number("0")
		}
		return n
	}
	f, err := strconv.ParseFloat(text, 64)
	if math.IsInf(f, 1) {
		return pyFloatSpecial("Infinity")
	}
	if math.IsInf(f, -1) {
		return pyFloatSpecial("-Infinity")
	}
	if err != nil {
		return n
	}
	return json.Number(pyrepr.Float(f))
}

// Replaces NaN and Infinity values with sentinel strings the encoder accepts, nil mappings staying null
func encodeSpecials(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = encodeSpecials(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = encodeSpecials(x)
		}
		return out
	case *OrderedObject:
		if t == nil {
			return nil
		}
		out := NewOrderedObject()
		for _, k := range t.Keys {
			out.Set(k, encodeSpecials(t.Values[k]))
		}
		return out
	case pyFloatSpecial:
		return specialSentinel + string(t)
	}
	return v
}

// Turns encoded sentinel strings back into bare NaN and Infinity tokens
func restoreSpecials(out []byte) []byte {
	for _, token := range []string{"-Infinity", "Infinity", "NaN"} {
		out = bytes.ReplaceAll(out, []byte(`"\u0000\u0000pyjson-special:`+token+`"`), []byte(token))
	}
	return out
}

// Encodes like Python json dumps with sorted keys, ASCII escapes and an optional two space indent
func dumpsJSON(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(encodeSpecials(v)); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if !indent {
		out = pySeparators(out)
	}
	return restoreSpecials(asciiEscape(out)), nil
}

// Rewrites compact Go separators into Python's default comma space and colon space outside strings
func pySeparators(in []byte) []byte {
	out := make([]byte, 0, len(in)+len(in)/8)
	inString, escaped := false, false
	for _, b := range in {
		out = append(out, b)
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case ',', ':':
			out = append(out, ' ')
		}
	}
	return out
}

// Escapes every non ASCII rune as Python ensure_ascii does, astral runes as surrogate pairs
func asciiEscape(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for len(in) > 0 {
		r, size := utf8.DecodeRune(in)
		if r < 0x7f {
			out = append(out, in[0])
			in = in[1:]
			continue
		}
		in = in[size:]
		if r > 0xFFFF {
			r -= 0x10000
			out = append(out, fmt.Sprintf(`\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))...)
			continue
		}
		out = append(out, fmt.Sprintf(`\u%04x`, r)...)
	}
	return out
}

// Python repr of a NaN or infinite float
func (s pyFloatSpecial) PyRepr() string {
	return map[pyFloatSpecial]string{"NaN": "nan", "Infinity": "inf", "-Infinity": "-inf"}[s]
}

// Python type name of a NaN or infinite float
func (s pyFloatSpecial) PyTypeName() string { return "float" }
