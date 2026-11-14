package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
)

func wideSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

func trimWide(s string) string {
	return strings.TrimFunc(s, wideSpace)
}

func wideFields(s string) []string {
	return strings.FieldsFunc(s, wideSpace)
}

func coerceArgs(tool string, params *jnode, args Args) Args {
	if len(args) == 0 || !params.isObj() {
		return args
	}
	properties := params.get("properties")
	if !properties.truthy() || !properties.isObj() {
		return args
	}
	out := Args(unrenameArgs(params, map[string]any(args), nil))
	for key, value := range out {
		prop := properties.get(key)
		if !prop.truthy() || !prop.isObj() {
			continue
		}
		prop = resolveLocalRef(prop, params, nil)
		if !prop.isObj() {
			continue
		}
		expected := prop.get("type")
		_, isContainer := value.([]any)
		if expected.strEq("array") && value != nil && !isContainer {
			if s, ok := value.(string); ok {
				if coerced, changed := coerceValue(s, expected, prop); changed {
					out[key] = coerced
					continue
				}
				if strings.HasPrefix(trimWide(s), "[") {
					slog.Warn(fmt.Sprintf("coerce_tool_args: %s.%s looks like a JSON array string but could not be parsed — model may have emitted a JSON-encoded string instead of a native array. Falling back to single-element list.", tool, key))
				}
				out[key] = []any{s}
				slog.Info(fmt.Sprintf("coerce_tool_args: wrapped bare string in list for %s.%s", tool, key))
				continue
			}
			out[key] = []any{value}
			slog.Info(fmt.Sprintf("coerce_tool_args: wrapped bare %s in list for %s.%s", jsonKind(value), tool, key))
			continue
		}
		s, isString := value.(string)
		if !isString {
			_, isObject := asObject(value)
			if expected.strEq("array") && isContainer || expected.strEq("object") && isObject {
				if normalized, changed := normalizeJSONStrings(value, prop, params); changed {
					out[key] = normalized
				}
			}
			continue
		}
		if !expected.truthy() && !schemaAllowsNull(prop) {
			continue
		}
		coerced, changed := coerceValue(s, expected, prop)
		if !changed {
			continue
		}
		out[key] = coerced
		switch coerced.(type) {
		case []any, map[string]any:
			if normalized, changed := normalizeJSONStrings(coerced, prop, params); changed {
				out[key] = normalized
			}
		}
	}
	return out
}

func jsonKind(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(string(v.(json.Number)), ".eE") {
			return "float"
		}
		return "int"
	case float64, float32:
		return "float"
	case int, int64, int32:
		return "int"
	case map[string]any, Args:
		return "dict"
	case nil:
		return "NoneType"
	}
	return "str"
}

func schemaAcceptsKind(schema *jnode, kind string, root *jnode, seen map[*jnode]bool) bool {
	if !schema.isObj() || seen[schema] {
		return false
	}
	next := make(map[*jnode]bool, len(seen)+1)
	for k := range seen {
		next[k] = true
	}
	next[schema] = true
	if root == nil {
		root = schema
	}
	schema = resolveLocalRef(schema, root, nil)
	t := schema.get("type")
	if t.strEq(kind) {
		return true
	}
	if t.isArr() {
		for _, item := range t.arr {
			if item.strEq(kind) {
				return true
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches := schema.get(key)
		if !branches.isArr() {
			continue
		}
		for _, b := range branches.arr {
			if schemaAcceptsKind(b, kind, root, next) {
				return true
			}
		}
	}
	return false
}

func normalizeJSONStrings(value any, schema, root *jnode) (any, bool) {
	if !schema.isObj() {
		return value, false
	}
	if root == nil {
		root = schema
	}
	schema = resolveLocalRef(schema, root, nil)
	changed := false
	if s, ok := value.(string); ok {
		trimmed := trimWide(s)
		expectsArray := schemaAcceptsKind(schema, "array", root, nil)
		expectsObject := schemaAcceptsKind(schema, "object", root, nil)
		if !(expectsArray && strings.HasPrefix(trimmed, "[") || expectsObject && strings.HasPrefix(trimmed, "{")) {
			return value, false
		}
		parsed, err := decodeLoose(trimmed)
		if err != nil {
			return value, false
		}
		_, isList := parsed.([]any)
		_, isObject := parsed.(map[string]any)
		if !(isList && expectsArray || isObject && expectsObject) {
			return value, false
		}
		value = parsed
		changed = true
	}
	if list, ok := value.([]any); ok {
		items := schema.get("items")
		if !items.isObj() {
			return value, changed
		}
		out := make([]any, len(list))
		touched := false
		for i, item := range list {
			n, c := normalizeJSONStrings(item, items, root)
			out[i] = n
			touched = touched || c
		}
		if touched {
			return out, true
		}
		return value, changed
	}
	if m, ok := asObject(value); ok {
		props := schema.get("properties")
		if !props.isObj() {
			return value, changed
		}
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		touched := false
		for i, k := range props.keys {
			v, present := m[k]
			if !present || !props.vals[i].isObj() {
				continue
			}
			n, c := normalizeJSONStrings(v, props.vals[i], root)
			out[k] = n
			touched = touched || c
		}
		if touched {
			return out, true
		}
		return value, changed
	}
	return value, changed
}

func decodeLoose(text string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("extra data after JSON value")
	}
	return v, nil
}

func coerceValue(value string, expected, schema *jnode) (any, bool) {
	if schemaAllowsNull(schema) && strings.ToLower(trimWide(value)) == "null" {
		return nil, true
	}
	if expected.isArr() {
		for _, t := range expected.arr {
			if r, ok := coerceValue(value, t, schema); ok {
				return r, true
			}
		}
		return value, false
	}
	t, _ := expected.text()
	switch t {
	case "integer":
		return coerceNumber(value, true)
	case "number":
		return coerceNumber(value, false)
	case "boolean":
		switch strings.ToLower(trimWide(value)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return value, false
	case "array":
		return coerceJSON(value, "list")
	case "object":
		return coerceJSON(value, "dict")
	case "null":
		if strings.ToLower(trimWide(value)) == "null" {
			return nil, true
		}
	}
	return value, false
}

func schemaAllowsNull(schema *jnode) bool {
	if !schema.isObj() {
		return false
	}
	t := schema.get("type")
	if t.strEq("null") {
		return true
	}
	if t.isArr() {
		for _, item := range t.arr {
			if item.strEq("null") {
				return true
			}
		}
	}
	if n := schema.get("nullable"); n != nil && n.kind == jBool && n.b {
		return true
	}
	for _, key := range unionKeys {
		variants := schema.get(key)
		if !variants.isArr() {
			continue
		}
		for _, v := range variants.arr {
			if v.isObj() && v.get("type").strEq("null") {
				return true
			}
		}
	}
	return false
}

func coerceJSON(value, kind string) (any, bool) {
	parsed, err := decodeLoose(value)
	if err != nil {
		slog.Warn(fmt.Sprintf("coerce_tool_args: failed to parse string as JSON for expected type %s: %v", kind, err))
		return value, false
	}
	switch parsed.(type) {
	case []any:
		if kind == "list" {
			slog.Debug("coerce_tool_args: coerced string to " + kind + " via json.loads")
			return parsed, true
		}
	case map[string]any:
		if kind == "dict" {
			slog.Debug("coerce_tool_args: coerced string to " + kind + " via json.loads")
			return parsed, true
		}
	}
	slog.Warn(fmt.Sprintf("coerce_tool_args: JSON-parsed value is %s, expected %s — skipping coercion", decodedKind(parsed), kind))
	return value, false
}

func decodedKind(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	case string:
		return "str"
	case nil:
		return "NoneType"
	}
	return jsonKind(v)
}

func parseDecimalText(s string) (float64, bool) {
	s = trimWide(s)
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 {
		return 0, false
	}
	switch strings.ToLower(body) {
	case "inf", "infinity", "nan":
		return 0, false
	}
	var clean strings.Builder
	digits, prevDigit, seenDot, seenExp := 0, false, false, false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9':
			clean.WriteByte(c)
			digits++
			prevDigit = true
		case c == '_':
			if !prevDigit || i+1 >= len(body) || body[i+1] < '0' || body[i+1] > '9' {
				return 0, false
			}
			prevDigit = false
		case c == '.':
			if seenDot || seenExp {
				return 0, false
			}
			seenDot = true
			clean.WriteByte(c)
			prevDigit = false
		case c == 'e' || c == 'E':
			if seenExp || digits == 0 {
				return 0, false
			}
			seenExp = true
			clean.WriteByte('e')
			prevDigit = false
			if i+1 < len(body) && (body[i+1] == '+' || body[i+1] == '-') {
				i++
				clean.WriteByte(body[i])
			}
			if i+1 >= len(body) || body[i+1] < '0' || body[i+1] > '9' {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	if digits == 0 {
		return 0, false
	}
	text := clean.String()
	if strings.HasPrefix(s, "-") {
		text = "-" + text
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func coerceNumber(value string, integerOnly bool) (any, bool) {
	f, ok := parseDecimalText(value)
	if !ok {
		return value, false
	}
	if f == math.Trunc(f) {
		if f == 0 {
			return json.Number("0"), true
		}
		return json.Number(new(big.Float).SetFloat64(f).Text('f', 0)), true
	}
	if integerOnly {
		return value, false
	}
	return json.Number(floatText(f)), true
}

func floatText(f float64) string {
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(sci[strings.LastIndexByte(sci, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return sci
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
