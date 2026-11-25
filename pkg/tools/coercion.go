package tools

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	m, ok := jsonutil.AsMap(schema)
	if !ok {
		return schema
	}
	root, ok := jsonutil.AsMap(rootSchema)
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
		if d, isDict := jsonutil.AsMap(target); isDict {
			target = d
		}
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
	targetMap, ok := jsonutil.AsMap(target)
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
	resolvedMap, ok := jsonutil.AsMap(resolved)
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
	resolved, _ := jsonutil.AsMap(ResolveLocalSchemaRef(paramsSchema, root))
	if resolved == nil {
		return args
	}
	props, ok := jsonutil.AsMap(resolved["properties"])
	if !ok {
		return args
	}
	var keys []string
	var vals map[string]any
	switch a := args.(type) {
	case map[string]any:
		vals = a
		for key := range a {
			keys = append(keys, key)
		}
	case *jsonutil.Object:
		if a == nil {
			return args
		}
		keys, vals = a.Keys(), a.Map()
	default:
		return args
	}
	reverse := map[string]string{}
	for orig, renamed := range schemaRenamePropertyKeys(resolved["properties"], "<unrename>") {
		reverse[renamed] = orig
	}
	_, isOrdered := args.(*jsonutil.Object)
	out := make(map[string]any, len(vals))
	outOrdered := jsonutil.NewObject()
	for _, key := range keys {
		value := vals[key]
		orig := key
		if o, ok := reverse[key]; ok {
			orig = o
		}
		sub := map[string]any{}
		if ps, ok := jsonutil.AsMap(props[orig]); ok {
			if r, ok := jsonutil.AsMap(ResolveLocalSchemaRef(ps, root)); ok {
				sub = r
			}
		}
		if coerceIsDict(value) && len(sub) > 0 {
			value = UnrenameToolArgs(sub, value, root)
		} else if vl, ok := value.([]any); ok {
			if items, ok := jsonutil.AsMap(sub["items"]); ok {
				rebuilt := make([]any, len(vl))
				for i, item := range vl {
					if coerceIsDict(item) {
						rebuilt[i] = UnrenameToolArgs(items, item, root)
					} else {
						rebuilt[i] = item
					}
				}
				value = rebuilt
			}
		}
		out[orig] = value
		outOrdered.Set(orig, value)
	}
	if isOrdered {
		return outOrdered
	}
	return out
}

// JSON reader coercion parses strings with, the DecodeWTF8 shapes for handlers or Python values for the bridge
type coerceDecoder func(text string) (any, error)

// Decodes into the shapes tool handlers read, every number a float64 and every dict a plain map
func wtf8CoerceDecoder(text string) (any, error) {
	v, err := jsonutil.DecodePlain([]byte(text))
	if err != nil {
		if _, perr := jsonutil.Decode(text); perr != nil {
			return nil, perr
		}
	}
	return v, err
}

// Reports a Python dict, a plain map or an ordered object
func coerceIsDict(v any) bool {
	switch v.(type) {
	case map[string]any, *jsonutil.Object:
		return true
	}
	return false
}

// CoerceToolArgs repairs string typed args against the tool's JSON Schema keeping originals on failure
func CoerceToolArgs(ctx context.Context, r *Registry, env *Env, toolName string, args map[string]any) map[string]any {
	return coerceToolArgsWith(ctx, r, env, toolName, args, wtf8CoerceDecoder)
}

// Coerces args holding typed values, JSON strings decoded into ordered objects with integers of any size exact and floats kept floats
func coercePythonToolArgs(ctx context.Context, r *Registry, env *Env, toolName string, args map[string]any) map[string]any {
	return coerceToolArgsWith(ctx, r, env, toolName, args, exactCoerceDecoder)
}

// Decodes into ordered objects, integers as int64 or *big.Int and other numbers as float64
func exactCoerceDecoder(text string) (any, error) {
	return jsonutil.DecodeWith(text, jsonutil.BigInts())
}

// Coercion loop of coerce_tool_args parsing JSON strings with the given reader
func coerceToolArgsWith(ctx context.Context, r *Registry, env *Env, toolName string, args map[string]any, decode coerceDecoder) map[string]any {
	if len(args) == 0 {
		return args
	}
	def := r.Entry(ctx, env, toolName)
	if def == nil {
		return args
	}
	var parametersRaw any
	if def.Schema != nil {
		parametersRaw = def.Schema.Value("parameters")
	}
	parameters, _ := jsonutil.AsMap(parametersRaw)
	if parameters == nil {
		return args
	}
	properties, _ := jsonutil.AsMap(parameters["properties"])
	if len(properties) == 0 {
		return args
	}
	if unrenamed, ok := UnrenameToolArgs(parametersRaw, args, nil).(map[string]any); ok {
		args = unrenamed
	}
	for _, key := range sortedKeys(args) {
		value := args[key]
		propRaw, ok := properties[key]
		if !ok || propRaw == nil {
			continue
		}
		propSchema, _ := jsonutil.AsMap(ResolveLocalSchemaRef(propRaw, parametersRaw))
		if propSchema == nil {
			continue
		}
		expected := propSchema["type"]
		expectedStr, _ := expected.(string)
		_, isContainer := value.([]any)
		if expectedStr == "array" && value != nil && !isContainer {
			if s, ok := value.(string); ok {
				coerced, changed := coerceValueWith(decode, s, expected, propSchema)
				if changed {
					args[key] = coerced
					continue
				}
				if strings.HasPrefix(strings.TrimSpace(s), "[") {
					pkgLog().Warn(fmt.Sprintf("coerce_tool_args: %s.%s looks like a JSON array string but could not be parsed — model may have emitted a JSON-encoded string instead of a native array. Falling back to single-element list.", toolName, key))
				}
				args[key] = []any{s}
				pkgLog().Info(fmt.Sprintf("coerce_tool_args: wrapped bare string in list for %s.%s", toolName, key))
				continue
			}
			args[key] = []any{value}
			pkgLog().Info(fmt.Sprintf("coerce_tool_args: wrapped bare %s in list for %s.%s", anyutil.Kind(value), toolName, key))
			continue
		}
		s, isString := value.(string)
		if !isString {
			if (expectedStr == "array" && isContainer) || (expectedStr == "object" && coerceIsDict(value)) {
				if normalized, changed := coerceNormalizeJSONStringsWith(decode, value, propSchema, parameters); changed {
					args[key] = normalized
				}
			}
			continue
		}
		if coerceTypeEmpty(expected) && !coerceSchemaAllowsNull(propSchema) {
			continue
		}
		coerced, changed := coerceValueWith(decode, s, expected, propSchema)
		if changed {
			args[key] = coerced
			switch coerced.(type) {
			case []any, map[string]any, *jsonutil.Object:
				if normalized, changed := coerceNormalizeJSONStringsWith(decode, coerced, propSchema, parameters); changed {
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
	m, ok := jsonutil.AsMap(schema)
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
	resolved, ok := jsonutil.AsMap(ResolveLocalSchemaRef(m, rootSchema))
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

// JSON string normalization reading nested strings with the given JSON reader
func coerceNormalizeJSONStringsWith(decode coerceDecoder, value any, schema any, rootSchema any) (any, bool) {
	m, ok := jsonutil.AsMap(schema)
	if !ok {
		return value, false
	}
	if rootSchema == nil {
		rootSchema = m
	}
	resolved, ok := jsonutil.AsMap(ResolveLocalSchemaRef(m, rootSchema))
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
		parsed, err := decode(trimmed)
		if err != nil {
			return value, false
		}
		_, isList := parsed.([]any)
		isObj := coerceIsDict(parsed)
		if !((isList && expectsArray) || (isObj && expectsObject)) {
			return value, false
		}
		value = parsed
		changed = true
	}
	switch v := value.(type) {
	case []any:
		items, ok := jsonutil.AsMap(resolved["items"])
		if !ok {
			return value, changed
		}
		out := make([]any, len(v))
		anyChanged := false
		for i, item := range v {
			n, c := coerceNormalizeJSONStringsWith(decode, item, items, rootSchema)
			out[i] = n
			anyChanged = anyChanged || c
		}
		if anyChanged {
			return out, true
		}
		return value, changed
	case map[string]any:
		props, ok := jsonutil.AsMap(resolved["properties"])
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
			ps, ok := jsonutil.AsMap(propSchema)
			if !has || !ok {
				continue
			}
			n, c := coerceNormalizeJSONStringsWith(decode, val, ps, rootSchema)
			out[k] = n
			anyChanged = anyChanged || c
		}
		if anyChanged {
			return out, true
		}
		return value, changed
	case *jsonutil.Object:
		props, ok := jsonutil.AsMap(resolved["properties"])
		if !ok {
			return value, changed
		}
		out := v.Clone()
		anyChanged := false
		for k, propSchema := range props {
			val, has := v.Get(k)
			ps, ok := jsonutil.AsMap(propSchema)
			if !has || !ok {
				continue
			}
			n, c := coerceNormalizeJSONStringsWith(decode, val, ps, rootSchema)
			out.Set(k, n)
			anyChanged = anyChanged || c
		}
		if anyChanged {
			return out, true
		}
		return value, changed
	}
	return value, changed
}

// String coercion parsing JSON containers with the given JSON reader
func coerceValueWith(decode coerceDecoder, value string, expectedType any, schema map[string]any) (any, bool) {
	if coerceSchemaAllowsNull(schema) && strings.ToLower(strings.TrimSpace(value)) == "null" {
		return nil, true
	}
	switch t := expectedType.(type) {
	case []any:
		for _, candidate := range t {
			if r, changed := coerceValueWith(decode, value, candidate, schema); changed {
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
			return coerceJSONWith(decode, value, "list")
		case "object":
			return coerceJSONWith(decode, value, "dict")
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

// Container parse of a string through the given JSON reader
func coerceJSONWith(decode coerceDecoder, value string, expected string) (any, bool) {
	parsed, err := decode(value)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("coerce_tool_args: failed to parse string as JSON for expected type %s: %s", expected, err))
		return value, false
	}
	switch parsed.(type) {
	case []any:
		if expected == "list" {
			pkgLog().Debug(fmt.Sprintf("coerce_tool_args: coerced string to %s via json.loads", expected))
			return parsed, true
		}
	case map[string]any, *jsonutil.Object:
		if expected == "dict" {
			pkgLog().Debug(fmt.Sprintf("coerce_tool_args: coerced string to %s via json.loads", expected))
			return parsed, true
		}
	}
	pkgLog().Warn(fmt.Sprintf("coerce_tool_args: JSON-parsed value is %s, expected %s — skipping coercion", anyutil.Kind(parsed), expected))
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
