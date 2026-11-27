package tools

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	pointerIndex = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	floatWord    = regexp.MustCompile(`^[+-]?(inf|infinity|nan)$`)
	digitGroup   = regexp.MustCompile(`([0-9])_([0-9])`)
)

// Repairs arguments a model sent as text where the schema wants a number, a boolean, a list, an object or null, and a lone value where it
// wants a list; RawArgs follows the repaired arguments, and an argument that does not read as its type is left as it came
func coerce(call *Call, parameters map[string]any) error {
	properties, _ := parameters["properties"].(map[string]any)
	if len(call.Args) == 0 || len(properties) == 0 {
		return nil
	}
	changed := false
	keys := make([]string, 0, len(call.Args))
	for key := range call.Args {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		property, _ := resolveRef(properties[key], parameters, nil).(map[string]any)
		if property == nil {
			continue
		}
		if value, ok := coerceArg(call.Name, key, call.Args[key], property, parameters); ok {
			call.Args[key], changed = value, true
		}
	}
	if !changed {
		return nil
	}
	raw, err := json.Marshal(call.Args)
	if err != nil {
		return err
	}
	call.RawArgs, call.Args = string(raw), nil
	return json.Unmarshal(raw, &call.Args)
}

// One argument as its schema wants it, false when it stays as it is
func coerceArg(tool, key string, value any, schema, root map[string]any) (any, bool) {
	expected := schema["type"]
	_, isList := value.([]any)
	if expected == "array" && value != nil && !isList {
		if text, ok := value.(string); ok {
			if parsed, ok := coerceValue(text, expected, schema); ok {
				return parsed, true
			}
			if strings.HasPrefix(strings.TrimSpace(text), "[") {
				slog.Warn("tool argument looks like a JSON array string but could not be parsed; taking it as a single item list", "tool", tool, "argument", key)
			}
		}
		return []any{value}, true
	}
	text, isText := value.(string)
	if !isText {
		_, isObject := value.(map[string]any)
		if expected == "array" && isList || expected == "object" && isObject {
			return normalizeNested(value, schema, root)
		}
		return value, false
	}
	if emptyType(expected) && !allowsNull(schema) {
		return value, false
	}
	parsed, ok := coerceValue(text, expected, schema)
	if !ok {
		return value, false
	}
	switch parsed.(type) {
	case []any, map[string]any:
		if normalized, changed := normalizeNested(parsed, schema, root); changed {
			return normalized, true
		}
	}
	return parsed, true
}

func emptyType(expected any) bool {
	switch t := expected.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}

// Text read as the type the schema names, false when it does not read as one
func coerceValue(text string, expected any, schema map[string]any) (any, bool) {
	if allowsNull(schema) && strings.ToLower(strings.TrimSpace(text)) == "null" {
		return nil, true
	}
	switch t := expected.(type) {
	case []any:
		for _, candidate := range t {
			if value, ok := coerceValue(text, candidate, schema); ok {
				return value, true
			}
		}
	case string:
		switch t {
		case "integer":
			return number(text, true)
		case "number":
			return number(text, false)
		case "boolean":
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "true":
				return true, true
			case "false":
				return false, true
			}
		case "array":
			var list []any
			if err := json.Unmarshal([]byte(text), &list); err == nil && list != nil {
				return list, true
			}
		case "object":
			var object map[string]any
			if err := json.Unmarshal([]byte(text), &object); err == nil && object != nil {
				return object, true
			}
		case "null":
			if strings.ToLower(strings.TrimSpace(text)) == "null" {
				return nil, true
			}
		}
	}
	return text, false
}

// Number of a text, underscores between digits allowed, infinities, NaN and hex refused, a fraction refused for an integer
func number(text string, integer bool) (any, bool) {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	if trimmed == "" || floatWord.MatchString(lower) || strings.ContainsAny(lower, "xp") {
		return text, false
	}
	candidate := trimmed
	for digitGroup.MatchString(candidate) {
		candidate = digitGroup.ReplaceAllString(candidate, "$1$2")
	}
	if strings.Contains(candidate, "_") {
		return text, false
	}
	f, err := strconv.ParseFloat(candidate, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return text, false
	}
	if math.Trunc(f) == f {
		if f >= math.MinInt64 && f <= math.MaxInt64 {
			return int64(f), true
		}
		return f, true
	}
	if integer {
		return text, false
	}
	return f, true
}

// Whether the schema lets the value be null
func allowsNull(schema map[string]any) bool {
	switch t := schema["type"].(type) {
	case string:
		if t == "null" {
			return true
		}
	case []any:
		if slices.Contains(t, any("null")) {
			return true
		}
	}
	if nullable, _ := schema["nullable"].(bool); nullable {
		return true
	}
	for _, union := range []string{"anyOf", "oneOf"} {
		variants, _ := schema[union].([]any)
		for _, variant := range variants {
			if branch, ok := variant.(map[string]any); ok && branch["type"] == "null" {
				return true
			}
		}
	}
	return false
}

// Whether the schema takes the kind of JSON value, through its type or any branch of a union
func accepts(schema any, kind string, root map[string]any, depth int) bool {
	resolved, _ := resolveRef(schema, root, nil).(map[string]any)
	if resolved == nil || depth > 32 {
		return false
	}
	switch t := resolved["type"].(type) {
	case string:
		if t == kind {
			return true
		}
	case []any:
		if slices.Contains(t, any(kind)) {
			return true
		}
	}
	for _, union := range []string{"anyOf", "oneOf", "allOf"} {
		branches, _ := resolved[union].([]any)
		for _, branch := range branches {
			if accepts(branch, kind, root, depth+1) {
				return true
			}
		}
	}
	return false
}

// Lists and objects whose items or properties hold JSON text where their schema wants a list or an object, that text parsed
func normalizeNested(value any, schema any, root map[string]any) (any, bool) {
	resolved, _ := resolveRef(schema, root, nil).(map[string]any)
	if resolved == nil {
		return value, false
	}
	changed := false
	if text, ok := value.(string); ok {
		trimmed := strings.TrimSpace(text)
		wantList, wantObject := accepts(resolved, "array", root, 0), accepts(resolved, "object", root, 0)
		if !(wantList && strings.HasPrefix(trimmed, "[") || wantObject && strings.HasPrefix(trimmed, "{")) {
			return value, false
		}
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) != nil {
			return value, false
		}
		_, isList := parsed.([]any)
		_, isObject := parsed.(map[string]any)
		if !(isList && wantList || isObject && wantObject) {
			return value, false
		}
		value, changed = parsed, true
	}
	switch v := value.(type) {
	case []any:
		items, ok := resolved["items"].(map[string]any)
		if !ok {
			return value, changed
		}
		out := slices.Clone(v)
		nested := false
		for i, item := range v {
			var c bool
			out[i], c = normalizeNested(item, items, root)
			nested = nested || c
		}
		if nested {
			return out, true
		}
	case map[string]any:
		properties, ok := resolved["properties"].(map[string]any)
		if !ok {
			return value, changed
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = item
		}
		nested := false
		for k, property := range properties {
			item, has := v[k]
			if _, ok := property.(map[string]any); !has || !ok {
				continue
			}
			var c bool
			out[k], c = normalizeNested(item, property, root)
			nested = nested || c
		}
		if nested {
			return out, true
		}
	}
	return value, changed
}

// Schema a document local $ref points to, with the referring schema's other keys kept; the schema itself when it holds no such ref or
// the ref does not resolve
func resolveRef(schema any, root map[string]any, seen map[string]bool) any {
	m, ok := schema.(map[string]any)
	if !ok || root == nil {
		return schema
	}
	ref, ok := m["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#") || seen[ref] {
		return schema
	}
	pointer, err := url.PathUnescape(ref[1:])
	if err != nil {
		pointer = ref[1:]
	}
	if pointer != "" && !strings.HasPrefix(pointer, "/") {
		return schema
	}
	var target any = root
	if pointer != "" {
		for _, token := range strings.Split(pointer, "/")[1:] {
			if badEscape(token) {
				return schema
			}
			token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
			switch t := target.(type) {
			case map[string]any:
				next, ok := t[token]
				if !ok {
					return schema
				}
				target = next
			case []any:
				if !pointerIndex.MatchString(token) {
					return schema
				}
				index, err := strconv.Atoi(token)
				if err != nil || index >= len(t) {
					return schema
				}
				target = t[index]
			default:
				return schema
			}
		}
	}
	if _, ok := target.(map[string]any); !ok {
		return schema
	}
	next := map[string]bool{ref: true}
	for k := range seen {
		next[k] = true
	}
	resolved, ok := resolveRef(target, root, next).(map[string]any)
	if !ok {
		return resolved
	}
	combined := make(map[string]any, len(resolved)+len(m))
	for k, v := range resolved {
		combined[k] = v
	}
	for k, v := range m {
		if _, has := combined[k]; !has && k != "$ref" {
			combined[k] = v
		}
	}
	return combined
}

// Whether a JSON pointer token holds a tilde that escapes neither 0 nor 1
func badEscape(token string) bool {
	for i := 0; i < len(token); i++ {
		if token[i] == '~' && (i+1 >= len(token) || token[i+1] != '0' && token[i+1] != '1') {
			return true
		}
	}
	return false
}
