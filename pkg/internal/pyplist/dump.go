package pyplist

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const xmlHeader = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
	"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"

// Indenting XML plist writer with sorted dictionary keys
type xmlWriter struct {
	buf   bytes.Buffer
	level int
}

// Writes one tab indented line
func (w *xmlWriter) line(text string) {
	w.buf.WriteString(strings.Repeat("\t", w.level))
	w.buf.WriteString(text)
	w.buf.WriteByte('\n')
}

// Text with line endings folded to newlines and markup escaped, an error for control characters
func escapeText(text string) (string, error) {
	for _, r := range text {
		if (r <= 0x08) || r == 0x0b || r == 0x0c || (r >= 0x0e && r <= 0x1f) {
			return "", &Error{Class: "ValueError", Msg: "strings can't contain control characters; use bytes instead"}
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	return strings.ReplaceAll(text, ">", "&gt;"), nil
}

// Writes an element holding escaped text
func (w *xmlWriter) simple(tag, text string) error {
	escaped, err := escapeText(text)
	if err != nil {
		return err
	}
	w.line("<" + tag + ">" + escaped + "</" + tag + ">")
	return nil
}

// Writes one value as its plist element
func (w *xmlWriter) value(v any) error {
	switch x := v.(type) {
	case string:
		return w.simple("string", x)
	case bool:
		if x {
			w.line("<true/>")
		} else {
			w.line("<false/>")
		}
		return nil
	case int:
		w.line("<integer>" + strconv.Itoa(x) + "</integer>")
		return nil
	case int64:
		w.line("<integer>" + strconv.FormatInt(x, 10) + "</integer>")
		return nil
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return w.array(items)
	case []any:
		return w.array(x)
	case map[string]string:
		items := make(map[string]any, len(x))
		for k, s := range x {
			items[k] = s
		}
		return w.dict(items)
	case map[string]any:
		return w.dict(x)
	}
	return &Error{Class: "TypeError", Msg: fmt.Sprintf("unsupported type: %T", v)}
}

// Writes an array element, self closing when empty
func (w *xmlWriter) array(items []any) error {
	if len(items) == 0 {
		w.line("<array/>")
		return nil
	}
	w.line("<array>")
	w.level++
	for _, item := range items {
		if err := w.value(item); err != nil {
			return err
		}
	}
	w.level--
	w.line("</array>")
	return nil
}

// Writes a dict element with keys in sorted order, self closing when empty
func (w *xmlWriter) dict(items map[string]any) error {
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
		if err := w.simple("key", k); err != nil {
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

// XML plist bytes of v the way plistlib.dump writes them with sorted keys
func DumpXML(v any) ([]byte, error) {
	w := &xmlWriter{}
	w.buf.WriteString(xmlHeader)
	w.line(`<plist version="1.0">`)
	if err := w.value(v); err != nil {
		return nil, err
	}
	w.line("</plist>")
	return w.buf.Bytes(), nil
}
