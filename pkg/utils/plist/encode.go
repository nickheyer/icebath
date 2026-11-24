package plistutil

import (
	"encoding/base64"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const xmlHeader = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
	"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
	"<plist version=\"1.0\">\n"

// Writer of a tab indented XML property list
type xmlWriter struct {
	buf   []byte
	level int
	depth int
}

// XML property list of v, tab indented with dictionary keys sorted: strings, booleans, integer and float kinds, time.Time, []byte, UID, slices and arrays, string-keyed maps
func EncodeXML(v any) ([]byte, error) {
	w := &xmlWriter{buf: make([]byte, 0, 1024)}
	w.buf = append(w.buf, xmlHeader...)
	if err := w.value(v); err != nil {
		return nil, err
	}
	w.buf = append(w.buf, "</plist>\n"...)
	return w.buf, nil
}

func (w *xmlWriter) indent() {
	for range w.level {
		w.buf = append(w.buf, '\t')
	}
}

// One indented line holding text as is
func (w *xmlWriter) line(text string) {
	w.indent()
	w.buf = append(w.buf, text...)
	w.buf = append(w.buf, '\n')
}

// One indented element holding escaped text
func (w *xmlWriter) element(tag, text string) error {
	w.indent()
	w.buf = append(w.buf, '<')
	w.buf = append(w.buf, tag...)
	w.buf = append(w.buf, '>')
	var err error
	if w.buf, err = appendEscaped(w.buf, text); err != nil {
		return err
	}
	w.buf = append(w.buf, "</"...)
	w.buf = append(w.buf, tag...)
	w.buf = append(w.buf, ">\n"...)
	return nil
}

// Text with markup escaped and line ends folded to newlines, an error for invalid UTF-8 and for control characters and the noncharacters U+FFFE and U+FFFF, which XML cannot hold
func appendEscaped(buf []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return buf, &Error{Msg: "string is not valid UTF-8: " + strconv.Quote(s)}
	}
	if strings.ContainsAny(s, "\uFFFE\uFFFF") {
		return buf, &Error{Msg: "strings cannot hold U+FFFE or U+FFFF, store them as data: " + strconv.Quote(s)}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '&':
			buf = append(buf, "&amp;"...)
		case c == '<':
			buf = append(buf, "&lt;"...)
		case c == '>':
			buf = append(buf, "&gt;"...)
		case c == '\r':
			buf = append(buf, '\n')
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
		case c < 0x20 && c != '\t' && c != '\n':
			return buf, &Error{Msg: "strings cannot hold control characters, store them as data: " + strconv.Quote(s)}
		default:
			buf = append(buf, c)
		}
	}
	return buf, nil
}

func (w *xmlWriter) value(v any) error {
	switch x := v.(type) {
	case string:
		return w.element("string", x)
	case bool:
		if x {
			w.line("<true/>")
		} else {
			w.line("<false/>")
		}
		return nil
	case int:
		return w.element("integer", strconv.Itoa(x))
	case int64:
		return w.element("integer", strconv.FormatInt(x, 10))
	case int32:
		return w.element("integer", strconv.FormatInt(int64(x), 10))
	case uint64:
		return w.element("integer", strconv.FormatUint(x, 10))
	case float64:
		return w.element("real", formatReal(x))
	case float32:
		return w.element("real", formatReal(float64(x)))
	case time.Time:
		t := x.UTC()
		if t.Year() < 0 || t.Year() > 9999 {
			return &Error{Msg: "date " + t.Format(time.RFC3339) + " is outside the years 0 to 9999 a property list holds"}
		}
		return w.element("date", t.Format("2006-01-02T15:04:05Z"))
	case []byte:
		w.data(x)
		return nil
	case UID:
		w.line("<dict>")
		w.level++
		w.line("<key>CF$UID</key>")
		w.line("<integer>" + strconv.FormatUint(uint64(x), 10) + "</integer>")
		w.level--
		w.line("</dict>")
		return nil
	case []any:
		return w.array(len(x), func(i int) any { return x[i] })
	case []string:
		return w.array(len(x), func(i int) any { return x[i] })
	case map[string]any:
		return w.dict(x)
	case map[string]string:
		items := make(map[string]any, len(x))
		for k, s := range x {
			items[k] = s
		}
		return w.dict(items)
	case nil:
		return &Error{Msg: "nil has no property list form"}
	}
	return w.reflected(reflect.ValueOf(v))
}

// Value of a type the fast switch does not name, by its kind
func (w *xmlWriter) reflected(rv reflect.Value) error {
	switch rv.Kind() {
	case reflect.String:
		return w.element("string", rv.String())
	case reflect.Bool:
		return w.value(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return w.element("integer", strconv.FormatInt(rv.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return w.element("integer", strconv.FormatUint(rv.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		return w.element("real", formatReal(rv.Float()))
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return w.array(0, nil)
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			if rv.Kind() == reflect.Slice {
				w.data(rv.Bytes())
				return nil
			}
			b := make([]byte, rv.Len())
			for i := range b {
				b[i] = byte(rv.Index(i).Uint())
			}
			w.data(b)
			return nil
		}
		return w.array(rv.Len(), func(i int) any { return rv.Index(i).Interface() })
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			break
		}
		items := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			items[it.Key().String()] = it.Value().Interface()
		}
		return w.dict(items)
	case reflect.Pointer, reflect.Interface:
		if !rv.IsNil() {
			if err := w.enter(); err != nil {
				return err
			}
			defer func() { w.depth-- }()
			return w.value(rv.Elem().Interface())
		}
	}
	return &Error{Msg: "unsupported type " + rv.Type().String()}
}

// Decimal text of a real, the shortest form that reads back to the same value
func formatReal(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "+infinity"
	case math.IsInf(f, -1):
		return "-infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Data element with base64 lines at its own indentation, wrapped to fit 76 columns with tabs counted as eight
func (w *xmlWriter) data(b []byte) {
	w.line("<data>")
	width := max(16, 76-8*w.level) / 4 * 4
	encoded := base64.StdEncoding.EncodeToString(b)
	for len(encoded) > 0 {
		n := min(width, len(encoded))
		w.line(encoded[:n])
		encoded = encoded[n:]
	}
	w.line("</data>")
}

// Error once containers nest deeper than a property list may, which also stops a value that contains itself
func (w *xmlWriter) enter() error {
	if w.depth++; w.depth > maxDepth {
		return &Error{Msg: "containers nested deeper than " + strconv.Itoa(maxDepth) + " levels or holding themselves"}
	}
	return nil
}

// Array element of n items, self closing when empty
func (w *xmlWriter) array(n int, item func(int) any) error {
	if err := w.enter(); err != nil {
		return err
	}
	defer func() { w.depth-- }()
	if n == 0 {
		w.line("<array/>")
		return nil
	}
	w.line("<array>")
	w.level++
	for i := range n {
		if err := w.value(item(i)); err != nil {
			return err
		}
	}
	w.level--
	w.line("</array>")
	return nil
}

// Dict element with its keys in sorted order, self closing when empty
func (w *xmlWriter) dict(items map[string]any) error {
	if err := w.enter(); err != nil {
		return err
	}
	defer func() { w.depth-- }()
	if len(items) == 0 {
		w.line("<dict/>")
		return nil
	}
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	w.line("<dict>")
	w.level++
	for _, k := range keys {
		if err := w.element("key", k); err != nil {
			return err
		}
		if err := w.value(items[k]); err != nil {
			return err
		}
	}
	w.level--
	w.line("</dict>")
	return nil
}
