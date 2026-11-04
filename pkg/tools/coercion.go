package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var (
	schemaPointerIndexRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	coerceFloatWordRe    = regexp.MustCompile(`^[+-]?(inf|infinity|nan)$`)
	coerceUnderscoreRe   = regexp.MustCompile(`([0-9])_([0-9])`)
)

// A tilde escapes only ~0 and ~1 inside an RFC 6901 pointer token
func schemaBadPointerEscape(token string) bool {
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			continue
		}
		if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
			return true
		}
	}
	return false
}

// ResolveLocalSchemaRef follows a document local $ref against the root with siblings kept
func ResolveLocalSchemaRef(schema, rootSchema any) any {
	return schemaResolveLocalRef(schema, rootSchema, map[string]bool{})
}

func schemaResolveLocalRef(schema, rootSchema any, seen map[string]bool) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	root, ok := rootSchema.(map[string]any)
	if !ok {
		return schema
	}
	ref, ok := m["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#") {
		return schema
	}
	pointer, err := url.PathUnescape(ref[1:])
	if err != nil {
		pointer = ref[1:]
	}
	if pointer != "" && !strings.HasPrefix(pointer, "/") {
		return schema
	}
	if seen[ref] {
		return schema
	}
	var target any = root
	tokens := strings.Split(pointer, "/")
	if len(tokens) > 0 {
		tokens = tokens[1:]
	}
	for _, raw := range tokens {
		if schemaBadPointerEscape(raw) {
			return schema
		}
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch t := target.(type) {
		case map[string]any:
			next, ok := t[token]
			if !ok {
				return schema
			}
			target = next
		case []any:
			if !schemaPointerIndexRe.MatchString(token) {
				return schema
			}
			idx, err := strconv.Atoi(token)
			if err != nil || idx >= len(t) {
				return schema
			}
			target = t[idx]
		default:
			return schema
		}
	}
	targetMap, ok := target.(map[string]any)
	if !ok {
		return schema
	}
	nextSeen := make(map[string]bool, len(seen)+1)
	for k := range seen {
		nextSeen[k] = true
	}
	nextSeen[ref] = true
	resolved := schemaResolveLocalRef(targetMap, root, nextSeen)
	siblings := map[string]any{}
	for k, v := range m {
		if k != "$ref" {
			siblings[k] = v
		}
	}
	resolvedMap, ok := resolved.(map[string]any)
	if len(siblings) == 0 || !ok {
		return resolved
	}
	combined := make(map[string]any, len(resolvedMap)+len(siblings))
	for k, v := range resolvedMap {
		combined[k] = v
	}
	for k, v := range siblings {
		if _, has := combined[k]; !has {
			combined[k] = v
		}
	}
	return combined
}

// UnrenameToolArgs maps sanitized keys in model emitted args back to the registry wire names
func UnrenameToolArgs(paramsSchema any, args any, rootSchema any) any {
	root := rootSchema
	if root == nil {
		root = paramsSchema
	}
	resolved, _ := ResolveLocalSchemaRef(paramsSchema, root).(map[string]any)
	if resolved == nil {
		return args
	}
	props, ok := resolved["properties"].(map[string]any)
	argMap, isMap := args.(map[string]any)
	if !ok || !isMap {
		return args
	}
	reverse := map[string]string{}
	for orig, renamed := range schemaRenamePropertyKeys(props, "<unrename>") {
		reverse[renamed] = orig
	}
	out := make(map[string]any, len(argMap))
	for key, value := range argMap {
		orig := key
		if o, ok := reverse[key]; ok {
			orig = o
		}
		sub := map[string]any{}
		if ps, ok := props[orig].(map[string]any); ok {
			if r, ok := ResolveLocalSchemaRef(ps, root).(map[string]any); ok {
				sub = r
			}
		}
		if vm, ok := value.(map[string]any); ok && len(sub) > 0 {
			value = UnrenameToolArgs(sub, vm, root)
		} else if vl, ok := value.([]any); ok {
			if items, ok := sub["items"].(map[string]any); ok {
				rebuilt := make([]any, len(vl))
				for i, item := range vl {
					if im, ok := item.(map[string]any); ok {
						rebuilt[i] = UnrenameToolArgs(items, im, root)
					} else {
						rebuilt[i] = item
					}
				}
				value = rebuilt
			}
		}
		out[orig] = value
	}
	return out
}

// CoerceToolArgs repairs string typed args against the tool's JSON Schema keeping originals on failure
func CoerceToolArgs(ctx context.Context, r *Registry, env *Env, toolName string, args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	def := r.Entry(ctx, env, toolName)
	if def == nil {
		return args
	}
	parameters, _ := def.Schema["parameters"].(map[string]any)
	if parameters == nil {
		return args
	}
	properties, _ := parameters["properties"].(map[string]any)
	if len(properties) == 0 {
		return args
	}
	if unrenamed, ok := UnrenameToolArgs(parameters, args, nil).(map[string]any); ok {
		args = unrenamed
	}
	for _, key := range sortedKeys(args) {
		value := args[key]
		propRaw, ok := properties[key]
		if !ok || propRaw == nil {
			continue
		}
		propSchema, _ := ResolveLocalSchemaRef(propRaw, parameters).(map[string]any)
		if propSchema == nil {
			continue
		}
		expected := propSchema["type"]
		expectedStr, _ := expected.(string)
		_, isContainer := value.([]any)
		if expectedStr == "array" && value != nil && !isContainer {
			if s, ok := value.(string); ok {
				coerced, changed := coerceValue(s, expected, propSchema)
				if changed {
					args[key] = coerced
					continue
				}
				if strings.HasPrefix(strings.TrimSpace(s), "[") {
					slog.Warn("coerce_tool_args: value looks like a JSON array string but could not be parsed, model may have emitted a JSON-encoded string instead of a native array. Falling back to single-element list.",
						"tool", toolName, "key", key)
				}
				args[key] = []any{s}
				slog.Info("coerce_tool_args: wrapped bare string in list", "tool", toolName, "key", key)
				continue
			}
			args[key] = []any{value}
			slog.Info("coerce_tool_args: wrapped bare value in list", "tool", toolName, "key", key)
			continue
		}
		s, isString := value.(string)
		if !isString {
			_, isObject := value.(map[string]any)
			if (expectedStr == "array" && isContainer) || (expectedStr == "object" && isObject) {
				if normalized, changed := coerceNormalizeJSONStrings(value, propSchema, parameters); changed {
					args[key] = normalized
				}
			}
			continue
		}
		if coerceTypeEmpty(expected) && !coerceSchemaAllowsNull(propSchema) {
			continue
		}
		coerced, changed := coerceValue(s, expected, propSchema)
		if changed {
			args[key] = coerced
			switch coerced.(type) {
			case []any, map[string]any:
				if normalized, changed := coerceNormalizeJSONStrings(coerced, propSchema, parameters); changed {
					args[key] = normalized
				}
			}
		}
	}
	return args
}

func coerceTypeEmpty(expected any) bool {
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

// Reports whether the schema permits the JSON kind through type or any union branch
func coerceSchemaAcceptsKind(schema any, kind string, rootSchema any, seen map[uintptr]bool) bool {
	m, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	id := reflect.ValueOf(m).Pointer()
	if seen[id] {
		return false
	}
	nextSeen := map[uintptr]bool{}
	for k := range seen {
		nextSeen[k] = true
	}
	nextSeen[id] = true
	if rootSchema == nil {
		rootSchema = m
	}
	resolved, ok := ResolveLocalSchemaRef(m, rootSchema).(map[string]any)
	if !ok {
		return false
	}
	switch t := resolved["type"].(type) {
	case string:
		if t == kind {
			return true
		}
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s == kind {
				return true
			}
		}
	}
	for _, unionKey := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := resolved[unionKey].([]any)
		if !ok {
			continue
		}
		for _, b := range branches {
			if coerceSchemaAcceptsKind(b, kind, rootSchema, nextSeen) {
				return true
			}
		}
	}
	return false
}

// Recursively parses JSON encoded strings where the schema expects a container
func coerceNormalizeJSONStrings(value any, schema any, rootSchema any) (any, bool) {
	m, ok := schema.(map[string]any)
	if !ok {
		return value, false
	}
	if rootSchema == nil {
		rootSchema = m
	}
	resolved, ok := ResolveLocalSchemaRef(m, rootSchema).(map[string]any)
	if !ok {
		return value, false
	}
	changed := false
	if s, ok := value.(string); ok {
		trimmed := strings.TrimSpace(s)
		expectsArray := coerceSchemaAcceptsKind(resolved, "array", rootSchema, nil)
		expectsObject := coerceSchemaAcceptsKind(resolved, "object", rootSchema, nil)
		if !((expectsArray && strings.HasPrefix(trimmed, "[")) || (expectsObject && strings.HasPrefix(trimmed, "{"))) {
			return value, false
		}
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return value, false
		}
		_, isList := parsed.([]any)
		_, isObj := parsed.(map[string]any)
		if !((isList && expectsArray) || (isObj && expectsObject)) {
			return value, false
		}
		value = parsed
		changed = true
	}
	switch v := value.(type) {
	case []any:
		items, ok := resolved["items"].(map[string]any)
		if !ok {
			return value, changed
		}
		out := make([]any, len(v))
		anyChanged := false
		for i, item := range v {
			n, c := coerceNormalizeJSONStrings(item, items, rootSchema)
			out[i] = n
			anyChanged = anyChanged || c
		}
		if anyChanged {
			return out, true
		}
		return value, changed
	case map[string]any:
		props, ok := resolved["properties"].(map[string]any)
		if !ok {
			return value, changed
		}
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = val
		}
		anyChanged := false
		for k, propSchema := range props {
			val, has := v[k]
			ps, ok := propSchema.(map[string]any)
			if !has || !ok {
				continue
			}
			n, c := coerceNormalizeJSONStrings(val, ps, rootSchema)
			out[k] = n
			anyChanged = anyChanged || c
		}
		if anyChanged {
			return out, true
		}
		return value, changed
	}
	return value, changed
}

// Coerces a string to the expected type or type union and reports whether it changed
func coerceValue(value string, expectedType any, schema map[string]any) (any, bool) {
	if coerceSchemaAllowsNull(schema) && strings.ToLower(strings.TrimSpace(value)) == "null" {
		return nil, true
	}
	switch t := expectedType.(type) {
	case []any:
		for _, candidate := range t {
			if r, changed := coerceValue(value, candidate, schema); changed {
				return r, true
			}
		}
		return value, false
	case string:
		switch t {
		case "integer":
			return coerceNumber(value, true)
		case "number":
			return coerceNumber(value, false)
		case "boolean":
			return coerceBoolean(value)
		case "array":
			return coerceJSON(value, "list")
		case "object":
			return coerceJSON(value, "dict")
		case "null":
			if strings.ToLower(strings.TrimSpace(value)) == "null" {
				return nil, true
			}
		}
	}
	return value, false
}

// Reports whether a schema fragment explicitly permits null
func coerceSchemaAllowsNull(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	switch t := schema["type"].(type) {
	case string:
		if t == "null" {
			return true
		}
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s == "null" {
				return true
			}
		}
	}
	if nullable, ok := schema["nullable"].(bool); ok && nullable {
		return true
	}
	for _, unionKey := range []string{"anyOf", "oneOf"} {
		variants, ok := schema[unionKey].([]any)
		if !ok {
			continue
		}
		for _, v := range variants {
			if schemaIsNullBranch(v) {
				return true
			}
		}
	}
	return false
}

// Parses the string as JSON when the parsed kind matches the expected container
func coerceJSON(value string, expected string) (any, bool) {
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		slog.Warn("coerce_tool_args: failed to parse string as JSON for expected type", "type", expected, "error", err)
		return value, false
	}
	switch parsed.(type) {
	case []any:
		if expected == "list" {
			slog.Debug("coerce_tool_args: coerced string via json.loads", "type", expected)
			return parsed, true
		}
	case map[string]any:
		if expected == "dict" {
			slog.Debug("coerce_tool_args: coerced string via json.loads", "type", expected)
			return parsed, true
		}
	}
	slog.Warn("coerce_tool_args: JSON-parsed value kind does not match expected type, skipping coercion", "expected", expected)
	return value, false
}

// Parses like Python float and refuses inf, nan, and decimals when integers are required
func coerceNumber(value string, integerOnly bool) (any, bool) {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if trimmed == "" || coerceFloatWordRe.MatchString(lower) || strings.ContainsAny(lower, "xp") {
		return value, false
	}
	candidate := coerceUnderscoreRe.ReplaceAllString(trimmed, "$1$2")
	for strings.Contains(candidate, "_") && coerceUnderscoreRe.MatchString(candidate) {
		candidate = coerceUnderscoreRe.ReplaceAllString(candidate, "$1$2")
	}
	if strings.Contains(candidate, "_") {
		return value, false
	}
	f, err := strconv.ParseFloat(candidate, 64)
	if err != nil {
		return value, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return value, false
	}
	if math.Trunc(f) == f {
		if f >= math.MinInt64 && f <= math.MaxInt64 {
			return int64(f), true
		}
		return f, true
	}
	if integerOnly {
		return value, false
	}
	return f, true
}

// Parses true or false ignoring case and surrounding whitespace
func coerceBoolean(value string) (any, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return value, false
}
