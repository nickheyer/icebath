package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Prefix marking a content cell that holds JSON encoded structured content
const contentJSONPrefix = "\x00json:"

// Storage classes a content value takes
type contentKind int

// Absent, plain text, structured JSON, a number or raw bytes
const (
	contentNull contentKind = iota
	contentText
	contentStructured
	contentInteger
	contentReal
	contentBlob
)

// Content of one message, plain text or structured JSON parts, the zero value meaning no content
type Content struct {
	kind contentKind
	text string
	raw  json.RawMessage
	num  float64
	n    int64
	blob []byte
}

// Plain text content
func TextContent(s string) Content { return Content{kind: contentText, text: s} }

// Content of a JSON value, a JSON string read as text, null as absent and a list or object kept as its raw bytes
func JSONContent(raw json.RawMessage) Content {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return Content{}
	}
	switch t[0] {
	case '{', '[':
		return Content{kind: contentStructured, raw: append(json.RawMessage{}, t...)}
	case '"':
		var s string
		if json.Unmarshal(t, &s) == nil {
			return TextContent(s)
		}
	case 'n':
		if string(t) == "null" {
			return Content{}
		}
	case 't', 'f':
		var b bool
		if json.Unmarshal(t, &b) == nil {
			return Content{kind: contentInteger, n: int64(boolInt(b))}
		}
	default:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return Content{kind: contentInteger, n: n}
		}
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return Content{kind: contentReal, num: f}
		}
	}
	return Content{kind: contentStructured, raw: append(json.RawMessage{}, t...)}
}

// Reports whether the message has no content
func (c Content) IsNull() bool { return c.kind == contentNull }

// Plain text of the content, false for anything else
func (c Content) Text() (string, bool) { return c.text, c.kind == contentText }

// Raw JSON of structured content, false for anything else
func (c Content) JSON() (json.RawMessage, bool) { return c.raw, c.kind == contentStructured }

// JSON form of the content for a payload, text as a JSON string and absent content as null
func (c Content) MarshalJSON() ([]byte, error) {
	switch c.kind {
	case contentText:
		return json.Marshal(c.text)
	case contentStructured:
		return c.raw, nil
	case contentInteger:
		return []byte(strconv.FormatInt(c.n, 10)), nil
	case contentReal:
		return json.Marshal(c.num)
	case contentBlob:
		return json.Marshal(string(c.blob))
	}
	return []byte("null"), nil
}

// Reads content from a payload value the way JSONContent does
func (c *Content) UnmarshalJSON(data []byte) error {
	*c = JSONContent(data)
	return nil
}

// Reports whether a truth test of the content holds, empty text and empty containers failing it
func (c Content) truthy() bool {
	switch c.kind {
	case contentText:
		return c.text != ""
	case contentStructured:
		return jsonTruthy(c.raw)
	case contentInteger:
		return c.n != 0
	case contentReal:
		return c.num != 0
	case contentBlob:
		return len(c.blob) > 0
	}
	return false
}

// Value bound for the content column, structured content as prefixed JSON and reserved-prefix text wrapped
func (c Content) encode() (any, error) {
	switch c.kind {
	case contentNull:
		return nil, nil
	case contentInteger:
		return c.n, nil
	case contentReal:
		return c.num, nil
	case contentBlob:
		return c.blob, nil
	case contentText:
		text := replaceSurrogates(c.text)
		if !strings.HasPrefix(text, contentJSONPrefix) {
			return text, nil
		}
		wrapped, err := asciiJSONString(text)
		if err != nil {
			return nil, err
		}
		return contentJSONPrefix + wrapped, nil
	}
	text, err := spacedJSON(c.raw)
	if err != nil {
		return nil, fmt.Errorf("message content is not valid JSON: %w", err)
	}
	return contentJSONPrefix + text, nil
}

// Content of a stored cell, a reserved-prefix cell that is not JSON staying raw text
func decodeContent(cell any) Content {
	switch v := cell.(type) {
	case nil:
		return Content{}
	case string:
		rest, ok := strings.CutPrefix(v, contentJSONPrefix)
		if !ok {
			return TextContent(v)
		}
		if !json.Valid([]byte(rest)) {
			slog.Warn("Failed to decode JSON-encoded message content; returning raw string")
			return TextContent(v)
		}
		return JSONContent(json.RawMessage(rest))
	case []byte:
		return Content{kind: contentBlob, blob: v}
	case int64:
		return Content{kind: contentInteger, n: v}
	case float64:
		return Content{kind: contentReal, num: v}
	}
	text, _ := cellText(cell)
	return TextContent(text)
}

// Reports whether two contents bind the same cell value
func (c Content) equal(o Content) bool {
	a, errA := c.encode()
	b, errB := o.encode()
	if errA != nil || errB != nil {
		return false
	}
	return cellsEqual(a, b)
}

// Reports whether two bound or stored cell values are the same value
func cellsEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	case int64:
		switch y := b.(type) {
		case int64:
			return x == y
		case float64:
			return float64(x) == y
		}
	case float64:
		switch y := b.(type) {
		case float64:
			return x == y
		case int64:
			return x == float64(y)
		}
	}
	return false
}

// Error of display metadata that is not one JSON object
var errDisplayMetadataShape = errors.New("display metadata is not a JSON object")

// Column text of display metadata, NULL for empty, non-JSON or non-object input
func encodeDisplayMetadata(raw json.RawMessage) (any, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || !jsonTruthy(t) {
		return nil, nil
	}
	if t[0] == '"' {
		var inner string
		if json.Unmarshal(t, &inner) != nil || !json.Valid([]byte(inner)) {
			slog.Warn("Ignoring non-JSON display metadata on write")
			return nil, nil
		}
		t = bytes.TrimSpace([]byte(inner))
		if len(t) == 0 || t[0] != '{' {
			slog.Warn("Ignoring non-object display metadata on write")
			return nil, nil
		}
	} else if t[0] != '{' {
		slog.Warn(fmt.Sprintf("Ignoring unexpected display metadata type on write: %s", jsonTypeName(t)))
		return nil, nil
	}
	if !json.Valid(t) {
		slog.Warn("Ignoring non-JSON display metadata on write")
		return nil, nil
	}
	text, err := spacedJSON(t)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Type name the write warning gives a JSON value that is not an object
func jsonTypeName(t []byte) string {
	switch t[0] {
	case '[':
		return "list"
	case 't', 'f':
		return "bool"
	case 'n':
		return "NoneType"
	}
	if bytes.ContainsAny(t, ".eE") {
		return "float"
	}
	return "int"
}

// Display metadata object of a stored cell with a second string layer unwrapped, nil for anything else
func decodeDisplayMetadata(cell any) json.RawMessage {
	text, ok := cell.(string)
	if !ok {
		if cell != nil {
			slog.Warn("Ignoring non-object display metadata on message row")
		}
		return nil
	}
	current := []byte(text)
	for range 2 {
		t := bytes.TrimSpace(current)
		if !json.Valid(t) {
			slog.Warn("Ignoring invalid display metadata on message row")
			return nil
		}
		if t[0] == '{' {
			return json.RawMessage(t)
		}
		var inner string
		if t[0] != '"' || json.Unmarshal(t, &inner) != nil {
			break
		}
		current = []byte(inner)
	}
	slog.Warn("Ignoring non-object display metadata on message row")
	return nil
}

// Column text of a structured reasoning field, NULL when empty and text that is not JSON stored as it is
func reasoningJSONText(raw json.RawMessage) (any, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return nil, nil
	}
	if !json.Valid(t) {
		return string(raw), nil
	}
	if !jsonTruthy(t) {
		return nil, nil
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	text, err := spacedJSON(t)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Text with invalid sequences replaced, NULL for an absent value
func scrubNullable(v *string) any {
	if v == nil {
		return nil
	}
	return replaceSurrogates(*v)
}

// Ordered members of a JSON object, false for anything else
func objectMembers(raw json.RawMessage) ([]configMember, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' || !json.Valid(t) {
		return nil, false
	}
	return configMembers(string(t)), true
}

// Raw value of key in an ordered object
func memberValue(members []configMember, key string) (json.RawMessage, bool) {
	for _, m := range members {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

// Members without key
func dropMember(members []configMember, key string) []configMember {
	out := members[:0:0]
	for _, m := range members {
		if m.key != key {
			out = append(out, m)
		}
	}
	return out
}

// Elements of a JSON array, false for anything else
func arrayElements(raw json.RawMessage) ([]json.RawMessage, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '[' {
		return nil, false
	}
	var out []json.RawMessage
	if json.Unmarshal(t, &out) != nil {
		return nil, false
	}
	if out == nil {
		out = []json.RawMessage{}
	}
	return out, true
}

// JSON array text of elements in the spaced layout
func encodeArray(elements []json.RawMessage) (string, error) {
	parts := make([]string, len(elements))
	for i, e := range elements {
		text, err := spacedJSON(e)
		if err != nil {
			return "", err
		}
		parts[i] = text
	}
	return "[" + strings.Join(parts, ", ") + "]", nil
}
