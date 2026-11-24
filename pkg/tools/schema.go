package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Anthropic and the gateways fronting it reject property keys outside this pattern
var (
	schemaPropKeyRe       = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
	schemaPropKeyBadChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
)

var schemaUnionKeys = []string{"anyOf", "oneOf"}

// Outer union metadata copied onto a collapsed replacement
var schemaUnionMetaKeys = []string{"title", "description", "default", "examples"}

var schemaBareTypeNames = map[string]bool{
	"object": true, "string": true, "number": true, "integer": true, "boolean": true, "array": true, "null": true,
}

var schemaNonSchemaListKeys = map[string]bool{"required": true, "enum": true, "examples": true, "dependentRequired": true}

var schemaMapKeys = map[string]bool{
	"properties": true, "$defs": true, "definitions": true, "patternProperties": true, "dependentSchemas": true,
}

var schemaChildKeys = map[string]bool{
	"items": true, "additionalItems": true, "additionalProperties": true, "unevaluatedItems": true,
	"unevaluatedProperties": true, "contains": true, "propertyNames": true, "not": true, "if": true,
	"then": true, "else": true, "anyOf": true, "oneOf": true, "allOf": true, "prefixItems": true,
}

var schemaRefForbiddenSiblings = []string{"default"}

var schemaTopLevelForbiddenKeys = []string{"allOf", "anyOf", "oneOf", "enum", "not"}

var schemaStripOnRecoveryKeys = map[string]bool{"pattern": true, "format": true}

var schemaMarkers = map[string]bool{"type": true, "anyOf": true, "oneOf": true, "allOf": true}

func schemaEmptyObject() *jsonutil.Object {
	return jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject(), "required", []any{})
}

// Returns map keys in sorted order so renames stay deterministic across runs
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Keys of a dict value in its own order, a plain map read in sorted order
func dictKeysInOrder(v any) []string {
	switch t := v.(type) {
	case *jsonutil.Object:
		if t == nil {
			return nil
		}
		return append([]string(nil), t.Keys()...)
	case map[string]any:
		return sortedKeys(t)
	}
	return nil
}

// Deep copies nested dicts and lists so callers may mutate the result freely, ordered dicts keeping their order
func schemaDeepCopy(v any) any {
	switch t := v.(type) {
	case *jsonutil.Object:
		return jsonutil.Clone(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = schemaDeepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = schemaDeepCopy(val)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = val
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = schemaDeepCopy(val)
		}
		return out
	default:
		return v
	}
}

// Ordered dict of a dict value, a plain map entering in sorted key order, nil for anything else
func schemaObject(v any) *jsonutil.Object {
	o, _ := jsonutil.AsObject(v)
	return o
}

// Bottom up map over a schema tree where fn sees every dict rebuilt in order after its children
func schemaRewrite(schema any, fn func(*jsonutil.Object) any) any {
	switch t := schema.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = schemaRewrite(item, fn)
		}
		return out
	case *jsonutil.Object, map[string]any:
		src := schemaObject(t)
		if src == nil {
			return schema
		}
		rebuilt := jsonutil.NewObject()
		for _, k := range src.Keys() {
			rebuilt.Set(k, schemaRewrite(src.Value(k), fn))
		}
		return fn(rebuilt)
	default:
		return schema
	}
}

// SanitizePropertyKey maps an arbitrary property key to one every provider accepts
func SanitizePropertyKey(key string) string {
	out := schemaPropKeyBadChars.ReplaceAllString(key, "_")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		return "param"
	}
	return out
}

// Builds original to conforming key renames in key order with numeric suffixes on collision
func schemaRenamePropertyKeys(props any, path string) map[string]string {
	renames := map[string]string{}
	taken := map[string]bool{}
	keys := dictKeysInOrder(props)
	for _, k := range keys {
		if schemaPropKeyRe.MatchString(k) {
			taken[k] = true
		}
	}
	for _, key := range keys {
		if schemaPropKeyRe.MatchString(key) {
			continue
		}
		base := SanitizePropertyKey(key)
		candidate := base
		for i := 2; taken[candidate]; i++ {
			suffix := fmt.Sprintf("_%d", i)
			cut := 64 - len(suffix)
			if cut > len(base) {
				cut = len(base)
			}
			candidate = base[:cut] + suffix
		}
		taken[candidate] = true
		renames[key] = candidate
		pkgLog().Debug("schema_sanitizer: renamed property key (provider key-pattern compat)",
			"path", path, "from", key, "to", candidate)
	}
	return renames
}

// SanitizeToolSchemas returns deep copied OpenAI format tools with sanitized parameter schemas
func SanitizeToolSchemas(tools []map[string]any) []map[string]any {
	if len(tools) == 0 {
		return tools
	}
	out := make([]map[string]any, len(tools))
	for i, tool := range tools {
		out[i] = schemaSanitizeSingleTool(tool)
	}
	return out
}

func schemaSanitizeSingleTool(tool map[string]any) map[string]any {
	out, _ := schemaDeepCopy(tool).(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	fn, ok := out["function"].(*jsonutil.Object)
	if !ok {
		if m, isMap := out["function"].(map[string]any); isMap {
			fn = schemaObject(m)
			out["function"] = fn
		} else {
			return out
		}
	}
	params, ok := fn.Get("parameters")
	if _, isDict := jsonutil.AsMap(params); !ok || !isDict {
		fn.Set("parameters", schemaEmptyObject())
		return out
	}
	name := "<tool>"
	if n, ok := fn.Get("name"); ok {
		name = fmt.Sprint(n)
	}
	top, _ := schemaSanitizeNode(params, name).(*jsonutil.Object)
	if top == nil {
		top = jsonutil.NewObject()
	}
	top.Set("type", "object")
	if _, ok := jsonutil.AsMap(top.Value("properties")); !ok {
		top.Set("properties", jsonutil.NewObject())
	}
	if _, ok := top.Value("required").([]any); !ok {
		top.Set("required", []any{})
	}
	collapsed, _ := StripNullableUnions(top, true).(*jsonutil.Object)
	collapsed = schemaStripTopLevelCombinators(collapsed, name)
	fn.Set("parameters", schemaStripRefSiblings(collapsed))
	return out
}

// Drops default beside $ref everywhere since Fireworks rejects that pairing
func schemaStripRefSiblings(node any) any {
	return schemaRewrite(node, func(out *jsonutil.Object) any {
		if _, ok := out.Get("$ref"); ok {
			for _, key := range schemaRefForbiddenSiblings {
				out.Delete(key)
			}
		}
		return out
	})
}

// Drops combinators from the top level only because Codex rejects them there
func schemaStripTopLevelCombinators(params *jsonutil.Object, path string) *jsonutil.Object {
	if params == nil {
		return params
	}
	out := params.Clone()
	for _, key := range schemaTopLevelForbiddenKeys {
		if _, ok := out.Get(key); ok {
			pkgLog().Debug("schema_sanitizer: stripped top-level combinator from tool parameters (strict-backend compat)",
				"path", path, "key", key)
			out.Delete(key)
		}
	}
	return out
}

func schemaIsNullBranch(item any) bool {
	m, ok := jsonutil.AsMap(item)
	if !ok {
		return false
	}
	t, _ := m["type"].(string)
	return t == "null"
}

// Copies outer union metadata onto the replacement where absent
func schemaCarryUnionMeta(outer, replacement *jsonutil.Object, skipDefaultOnRef bool) {
	for _, metaKey := range schemaUnionMetaKeys {
		val, present := outer.Get(metaKey)
		if !present {
			continue
		}
		if _, has := replacement.Get(metaKey); has {
			continue
		}
		if skipDefaultOnRef && metaKey == "default" {
			if _, isRef := replacement.Get("$ref"); isRef {
				continue
			}
		}
		replacement.Set(metaKey, val)
	}
}

// StripNullableUnions collapses anyOf or oneOf nullable unions to the single non null branch
func StripNullableUnions(schema any, keepNullableHint bool) any {
	var collapse func(*jsonutil.Object) any
	collapse = func(stripped *jsonutil.Object) any {
		for _, key := range schemaUnionKeys {
			variants, ok := stripped.Value(key).([]any)
			if !ok {
				continue
			}
			var nonNull []any
			for _, item := range variants {
				if !schemaIsNullBranch(item) {
					nonNull = append(nonNull, item)
				}
			}
			if len(nonNull) == 1 && len(nonNull) != len(variants) {
				replacement := jsonutil.NewObject()
				if m := schemaObject(nonNull[0]); m != nil {
					replacement = m.Clone()
				}
				if keepNullableHint {
					if _, has := replacement.Get("nullable"); !has {
						replacement.Set("nullable", true)
					}
				}
				schemaCarryUnionMeta(stripped, replacement, true)
				return schemaRewrite(replacement, collapse)
			}
		}
		return stripped
	}
	return schemaRewrite(schema, collapse)
}

// Primitive JSON Schema type of a pure const branch or empty when the branch is not one
func schemaConstBranchType(branch any) string {
	m, ok := jsonutil.AsMap(branch)
	if !ok {
		return ""
	}
	constVal, has := m["const"]
	if !has {
		return ""
	}
	for k := range m {
		switch k {
		case "const", "type", "title", "description":
		default:
			return ""
		}
	}
	jsonType := ""
	switch v := constVal.(type) {
	case bool:
		jsonType = "boolean"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		jsonType = "integer"
	case float32:
		jsonType = schemaFloatType(float64(v))
	case float64:
		jsonType = schemaFloatType(v)
	case json.Number:
		if strings.ContainsAny(v.String(), ".eE") {
			jsonType = "number"
		} else {
			jsonType = "integer"
		}
	case string:
		jsonType = "string"
	default:
		return ""
	}
	declaredRaw, hasType := m["type"]
	if !hasType {
		return jsonType
	}
	declared, ok := declaredRaw.(string)
	if !ok {
		return ""
	}
	if declared == jsonType {
		return jsonType
	}
	if declared == "number" && jsonType == "integer" {
		return "number"
	}
	return ""
}

func schemaHasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// Integral floats decoded from JSON count as integers like Python's int literal
func schemaFloatType(v float64) string {
	if math.Trunc(v) == v && !math.IsInf(v, 0) {
		return "integer"
	}
	return "number"
}

// CollapseConstUnions turns unions of same typed consts into enum without mutating the input
func CollapseConstUnions(schema any) any {
	return schemaRewrite(schema, func(out *jsonutil.Object) any {
		for _, key := range schemaUnionKeys {
			variants, ok := out.Value(key).([]any)
			if !ok || len(variants) == 0 {
				continue
			}
			var nullBranches, constBranches []any
			for _, item := range variants {
				m, _ := jsonutil.AsMap(item)
				if schemaIsNullBranch(item) && !schemaHasKey(m, "const") {
					nullBranches = append(nullBranches, item)
				} else {
					constBranches = append(constBranches, item)
				}
			}
			if len(nullBranches) > 1 || len(constBranches) == 0 {
				continue
			}
			branchType := ""
			consistent := true
			for i, item := range constBranches {
				t := schemaConstBranchType(item)
				if t == "" || (i > 0 && t != branchType) {
					consistent = false
					break
				}
				branchType = t
			}
			if !consistent {
				continue
			}
			enum := make([]any, 0, len(constBranches))
			for _, item := range constBranches {
				m, _ := jsonutil.AsMap(item)
				enum = append(enum, m["const"])
			}
			replacement := jsonutil.ObjectOf("type", branchType, "enum", enum)
			if len(nullBranches) > 0 {
				replacement.Set("nullable", true)
			}
			schemaCarryUnionMeta(out, replacement, false)
			return replacement
		}
		return out
	})
}

// Normalizes a type array into a single type, an anyOf of single types, or a null fallback
func schemaNormalizeTypeArray(value []any, out *jsonutil.Object) {
	hasNull := false
	var nonNull []string
	for _, t := range value {
		s, ok := t.(string)
		if !ok {
			continue
		}
		if s == "null" {
			hasNull = true
			continue
		}
		nonNull = append(nonNull, s)
	}
	if len(nonNull) == 0 {
		if hasNull {
			out.Set("type", "null")
		} else {
			out.Set("type", "object")
		}
		return
	}
	if len(nonNull) == 1 {
		out.Set("type", nonNull[0])
	} else {
		branches := make([]any, 0, len(nonNull))
		for _, t := range nonNull {
			branches = append(branches, jsonutil.ObjectOf("type", t))
		}
		out.Set("anyOf", branches)
	}
	if hasNull {
		if _, has := out.Get("nullable"); !has {
			out.Set("nullable", true)
		}
	}
}

// Recursively sanitizes a schema fragment, renaming keys and normalizing bare and array types
func schemaSanitizeNode(node any, path string) any {
	switch t := node.(type) {
	case string:
		if schemaBareTypeNames[t] {
			pkgLog().Debug("schema_sanitizer: replacing bare-string schema with type object", "path", path, "type", t)
			if t == "object" {
				return schemaEmptyObject()
			}
			return jsonutil.ObjectOf("type", t)
		}
		pkgLog().Debug("schema_sanitizer: replacing non-schema string with empty object schema", "path", path, "value", t)
		return schemaEmptyObject()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = schemaSanitizeNode(item, fmt.Sprintf("%s[%d]", path, i))
		}
		return out
	case *jsonutil.Object, map[string]any:
		return schemaSanitizeMap(schemaObject(t), path)
	default:
		return node
	}
}

func schemaSanitizeMap(node *jsonutil.Object, path string) *jsonutil.Object {
	propsIn := schemaObject(node.Value("properties"))
	propRenames := map[string]string{}
	if propsIn != nil {
		propRenames = schemaRenamePropertyKeys(propsIn, path+".properties")
	}
	out := jsonutil.NewObject()
	for _, key := range node.Keys() {
		value := node.Value(key)
		_, valueIsDict := jsonutil.AsMap(value)
		switch {
		case key == "type" && isAnyList(value):
			schemaNormalizeTypeArray(toAnyList(value), out)
		case schemaMapKeys[key] && valueIsDict:
			renames := map[string]string{}
			if key == "properties" {
				renames = propRenames
			}
			sub := jsonutil.NewObject()
			src := schemaObject(value)
			for _, k := range src.Keys() {
				nk := k
				if r, ok := renames[k]; ok {
					nk = r
				}
				sub.Set(nk, schemaSanitizeNode(src.Value(k), fmt.Sprintf("%s.%s.%s", path, key, nk)))
			}
			out.Set(key, sub)
		case key == "dependencies" && valueIsDict:
			sub := jsonutil.NewObject()
			src := schemaObject(value)
			for _, k := range src.Keys() {
				v := src.Value(k)
				if _, isDict := jsonutil.AsMap(v); isDict {
					sub.Set(k, schemaSanitizeNode(v, fmt.Sprintf("%s.%s.%s", path, key, k)))
				} else {
					sub.Set(k, schemaDeepCopy(v))
				}
			}
			out.Set(key, sub)
		case key == "items" || key == "additionalProperties":
			if _, isBool := value.(bool); isBool {
				out.Set(key, value)
			} else {
				out.Set(key, schemaSanitizeNode(value, path+"."+key))
			}
		case schemaNonSchemaListKeys[key]:
			if key == "required" {
				if _, isBool := value.(bool); isBool {
					continue
				}
				if len(propRenames) > 0 && isAnyList(value) {
					list := toAnyList(value)
					remapped := make([]any, len(list))
					for i, r := range list {
						if s, ok := r.(string); ok {
							if nr, ok := propRenames[s]; ok {
								remapped[i] = nr
								continue
							}
						}
						remapped[i] = r
					}
					out.Set(key, remapped)
					continue
				}
			}
			out.Set(key, schemaDeepCopy(value))
		case schemaChildKeys[key]:
			out.Set(key, schemaSanitizeNode(value, path+"."+key))
		default:
			out.Set(key, schemaDeepCopy(value))
		}
	}
	if propsIn != nil {
		var lifted []string
		for _, k := range propsIn.Keys() {
			v, ok := jsonutil.AsMap(propsIn.Value(k))
			if !ok {
				continue
			}
			if flag, ok := v["required"].(bool); ok && flag {
				name := k
				if r, ok := propRenames[k]; ok {
					name = r
				}
				lifted = append(lifted, name)
			}
		}
		if len(lifted) > 0 {
			required, _ := out.Value("required").([]any)
			merged := append([]any{}, required...)
			have := map[string]bool{}
			for _, r := range required {
				if s, ok := r.(string); ok {
					have[s] = true
				}
			}
			for _, key := range lifted {
				if !have[key] {
					merged = append(merged, key)
				}
			}
			out.Set("required", merged)
		}
	}
	if t, _ := out.Value("type").(string); t == "object" {
		props, ok := jsonutil.AsMap(out.Value("properties"))
		if !ok {
			props = map[string]any{}
			out.Set("properties", jsonutil.NewObject())
		}
		filtered := []any{}
		if required, ok := out.Value("required").([]any); ok {
			for _, r := range required {
				if s, ok := r.(string); ok && schemaHasKey(props, s) {
					filtered = append(filtered, s)
				}
			}
		}
		out.Set("required", filtered)
	}
	return out
}

func isAnyList(v any) bool {
	switch v.(type) {
	case []any, []string:
		return true
	}
	return false
}

func toAnyList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return nil
}

// Pre order walk yielding every dict node before its values so the visitor may mutate it
func schemaDictNodes(node any, visit func(any)) {
	switch t := node.(type) {
	case *jsonutil.Object:
		if t == nil {
			return
		}
		visit(t)
		for _, k := range append([]string(nil), t.Keys()...) {
			if v, ok := t.Get(k); ok {
				schemaDictNodes(v, visit)
			}
		}
	case map[string]any:
		visit(t)
		for _, k := range sortedKeys(t) {
			if v, ok := t[k]; ok {
				schemaDictNodes(v, visit)
			}
		}
	case []any:
		for _, child := range t {
			schemaDictNodes(child, visit)
		}
	}
}

// Removes a key from an ordered or plain dict in place
func dictDelete(node any, key string) {
	switch t := node.(type) {
	case *jsonutil.Object:
		t.Delete(key)
	case map[string]any:
		delete(t, key)
	}
}

// Applies stripNode to every dict node of each tool's parameters in place and counts removals
func schemaReactiveStrip(tools []map[string]any, stripNode func(any) int, logMsg string) ([]map[string]any, int) {
	stripped := 0
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		var params any
		if fn, ok := jsonutil.AsMap(tool["function"]); ok {
			params = fn["parameters"]
		}
		if _, ok := jsonutil.AsMap(params); !ok {
			params = tool["parameters"]
		}
		if _, ok := jsonutil.AsMap(params); ok {
			schemaDictNodes(params, func(node any) { stripped += stripNode(node) })
		}
	}
	if stripped > 0 {
		pkgLog().Info(logMsg, "count", stripped)
	}
	return tools, stripped
}

// StripPatternAndFormat removes pattern and format keywords in place after a llama.cpp grammar rejection
func StripPatternAndFormat(tools []map[string]any) ([]map[string]any, int) {
	return schemaReactiveStrip(tools, func(node any) int {
		keys := dictKeysInOrder(node)
		isSchema := false
		for _, k := range keys {
			if schemaMarkers[k] {
				isSchema = true
				break
			}
		}
		if !isSchema {
			return 0
		}
		hits := 0
		for _, k := range keys {
			if schemaStripOnRecoveryKeys[k] {
				dictDelete(node, k)
				hits++
			}
		}
		return hits
	}, "schema_sanitizer: stripped pattern/format keyword(s) from tool schemas (llama.cpp grammar-parse recovery)")
}

// StripSlashEnum removes enum keywords whose string values contain a slash after an xAI grammar rejection
func StripSlashEnum(tools []map[string]any) ([]map[string]any, int) {
	return schemaReactiveStrip(tools, func(node any) int {
		m, _ := jsonutil.AsMap(node)
		enum, ok := m["enum"].([]any)
		if !ok {
			return 0
		}
		for _, v := range enum {
			if s, ok := v.(string); ok && strings.Contains(s, "/") {
				dictDelete(node, "enum")
				return 1
			}
		}
		return 0
	}, "schema_sanitizer: stripped enum keyword(s) containing '/' from tool schemas (xAI Responses grammar-compile recovery)")
}
