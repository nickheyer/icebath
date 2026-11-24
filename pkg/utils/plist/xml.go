package plistutil

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
)

// Reader of one XML property list document
type xmlReader struct {
	dec   *xml.Decoder
	depth int
}

// Value of an XML property list held in text, transcoded telling that a byte order mark already turned it into UTF-8
func decodeXML(text []byte, transcoded bool) (any, error) {
	r := &xmlReader{dec: xml.NewDecoder(bytes.NewReader(text))}
	r.dec.Strict = true
	r.dec.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		return charsetReader(label, input, transcoded)
	}
	root, found, err := r.next()
	if errors.Is(err, io.EOF) || (err == nil && !found) {
		return nil, &Error{Msg: "no property list element"}
	}
	if err != nil {
		return nil, err
	}
	if root.Name.Local == "plist" {
		inner, found, err := r.next()
		if err != nil {
			return nil, r.endOfDocument(err)
		}
		if !found {
			return nil, r.fail("empty property list")
		}
		v, err := r.value(inner)
		if err != nil {
			return nil, err
		}
		if _, found, err := r.next(); err != nil {
			return nil, r.endOfDocument(err)
		} else if found {
			return nil, r.fail("more than one value in <plist>")
		}
		return r.finish(v)
	}
	v, err := r.value(root)
	if err != nil {
		return nil, err
	}
	return r.finish(v)
}

// Root value v once nothing but white space, comments and processing instructions follow it
func (r *xmlReader) finish(v any) (any, error) {
	if err := r.trailer(); err != nil {
		return nil, err
	}
	return v, nil
}

// Reader of the bytes after the XML declaration in UTF-8, through the named character set unless they already are
func charsetReader(label string, input io.Reader, transcoded bool) (io.Reader, error) {
	name := strings.ToLower(strings.TrimSpace(label))
	if transcoded || name == "us-ascii" || name == "ascii" || strings.HasPrefix(name, "utf-16") || strings.HasPrefix(name, "utf-32") {
		return input, nil
	}
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	text, err := charsetutil.Decode(raw, label)
	if err != nil {
		return nil, &Error{Msg: "cannot read the " + strconv.Quote(label) + " encoding", Err: err}
	}
	return strings.NewReader(text), nil
}

// Error located at the decoder's current line
func (r *xmlReader) fail(msg string) error {
	line, _ := r.dec.InputPos()
	return &Error{Msg: msg, Line: line}
}

// Error for a document that ended or broke inside the value
func (r *xmlReader) endOfDocument(err error) error {
	if errors.Is(err, io.EOF) {
		return r.fail("unexpected end of document")
	}
	return err
}

// Next token, a malformed document reported as an Error and the clean end as io.EOF
func (r *xmlReader) token() (xml.Token, error) {
	tok, err := r.dec.Token()
	if err == nil || errors.Is(err, io.EOF) {
		return tok, err
	}
	var plistErr *Error
	if errors.As(err, &plistErr) {
		return nil, plistErr
	}
	return nil, &Error{Msg: "malformed XML", Err: err}
}

// Next element inside the open container, false at the container's end tag, comments and white space skipped
func (r *xmlReader) next() (xml.StartElement, bool, error) {
	for {
		tok, err := r.token()
		if err != nil {
			return xml.StartElement{}, false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, true, nil
		case xml.EndElement:
			return xml.StartElement{}, false, nil
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return xml.StartElement{}, false, r.fail("text outside a value")
			}
		case xml.Directive:
			if bytes.Contains(t, []byte("ENTITY")) {
				return xml.StartElement{}, false, r.fail("entity declarations are not supported")
			}
		}
	}
}

// Nothing but white space, comments and processing instructions after the root value
func (r *xmlReader) trailer() error {
	for {
		tok, err := r.token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return r.fail("unexpected <" + t.Name.Local + "> after the root value")
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return r.fail("text after the root value")
			}
		}
	}
}

// Value of the element that start opened, its end tag consumed
func (r *xmlReader) value(start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict":
		return r.dict()
	case "array":
		return r.array()
	case "string":
		return r.text()
	case "true", "false":
		if err := r.empty(start.Name.Local); err != nil {
			return nil, err
		}
		return start.Name.Local == "true", nil
	case "integer":
		text, err := r.text()
		if err != nil {
			return nil, err
		}
		n, err := parseInteger(text)
		if err != nil {
			return nil, &Error{Msg: "invalid integer " + strconv.Quote(text), Line: r.line(), Err: err}
		}
		return n, nil
	case "real":
		text, err := r.text()
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, &Error{Msg: "invalid real " + strconv.Quote(text), Line: r.line()}
		}
		return f, nil
	case "data":
		text, err := r.text()
		if err != nil {
			return nil, err
		}
		b, err := decodeData(text)
		if err != nil {
			return nil, &Error{Msg: "invalid base64 data", Line: r.line(), Err: err}
		}
		return b, nil
	case "date":
		text, err := r.text()
		if err != nil {
			return nil, err
		}
		t, ok := parseDate(text)
		if !ok {
			return nil, &Error{Msg: "invalid date " + strconv.Quote(text), Line: r.line()}
		}
		return t, nil
	}
	return nil, r.fail("unexpected <" + start.Name.Local + ">")
}

func (r *xmlReader) line() int {
	line, _ := r.dec.InputPos()
	return line
}

// Contents of a dict element as a map, a single CF$UID integer entry read as a UID
func (r *xmlReader) dict() (any, error) {
	if r.depth++; r.depth > maxDepth {
		return nil, r.fail("nesting too deep")
	}
	defer func() { r.depth-- }()
	d := map[string]any{}
	for {
		start, found, err := r.next()
		if err != nil {
			return nil, r.endOfDocument(err)
		}
		if !found {
			break
		}
		if start.Name.Local != "key" {
			return nil, r.fail("expected <key> in <dict>, found <" + start.Name.Local + ">")
		}
		key, err := r.text()
		if err != nil {
			return nil, err
		}
		start, found, err = r.next()
		if err != nil {
			return nil, r.endOfDocument(err)
		}
		if !found {
			return nil, r.fail("missing value for key " + strconv.Quote(key))
		}
		v, err := r.value(start)
		if err != nil {
			return nil, err
		}
		d[key] = v
	}
	if len(d) == 1 {
		if ref, ok := archiveUID(d["CF$UID"]); ok {
			return ref, nil
		}
	}
	return d, nil
}

// UID a CF$UID entry holds, false for anything but a non-negative integer
func archiveUID(v any) (UID, bool) {
	switch n := v.(type) {
	case int64:
		if n >= 0 {
			return UID(n), true
		}
	case uint64:
		return UID(n), true
	}
	return 0, false
}

// Contents of an array element as a slice
func (r *xmlReader) array() ([]any, error) {
	if r.depth++; r.depth > maxDepth {
		return nil, r.fail("nesting too deep")
	}
	defer func() { r.depth-- }()
	items := []any{}
	for {
		start, found, err := r.next()
		if err != nil {
			return nil, r.endOfDocument(err)
		}
		if !found {
			return items, nil
		}
		v, err := r.value(start)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
}

// Character data of an element up to its end tag, comments skipped and a nested element refused
func (r *xmlReader) text() (string, error) {
	var b strings.Builder
	for {
		tok, err := r.token()
		if err != nil {
			return "", r.endOfDocument(err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			return b.String(), nil
		case xml.StartElement:
			return "", r.fail("unexpected <" + t.Name.Local + "> inside a value")
		}
	}
}

// End of an element that holds nothing but white space and comments
func (r *xmlReader) empty(name string) error {
	text, err := r.text()
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		return r.fail("<" + name + "> holds text")
	}
	return nil
}

// Integer of decimal or 0x hexadecimal text with an optional sign: int64, uint64 for positive values beyond int64
func parseInteger(text string) (any, error) {
	s := strings.TrimSpace(text)
	negative := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		negative, s = s[0] == '-', s[1:]
	}
	base := 10
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		base, s = 16, s[2:]
	}
	u, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return nil, err
	}
	switch {
	case negative && u > 1<<63:
		return nil, strconv.ErrRange
	case negative:
		return int64(-u), nil
	case u > math.MaxInt64:
		return u, nil
	}
	return int64(u), nil
}

// Bytes of base64 text with any white space inside, padding optional
func decodeData(text string) ([]byte, error) {
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' {
			return -1
		}
		return r
	}, text)
	if b, err := base64.StdEncoding.DecodeString(compact); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(compact, "="))
}

// Layouts of plist dates, the full form first and the shortened forms Apple's reader also takes after it, down to the year alone
var dateLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00", "2006-01-02T15Z07:00", "2006-01-02Z07:00", "2006-01Z07:00", "2006Z07:00"}

// UTC time of an ISO 8601 plist date
func parseDate(text string) (time.Time, bool) {
	s := strings.TrimSpace(text)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
