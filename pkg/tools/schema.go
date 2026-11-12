package tools

import (
	"encoding/json"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
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

func schemaEmptyObject() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
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

// Deep copies nested maps and slices so callers may mutate the result freely
func schemaDeepCopy(v any) any {
	switch t := v.(type) {
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

// Bottom up map over a schema tree where fn sees every map after its children
func schemaRewrite(schema any, fn func(map[string]any) any) any {
	switch t := schema.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = schemaRewrite(item, fn)
		}
		return out
	case map[string]any:
		rebuilt := make(map[string]any, len(t))
		for k, v := range t {
			rebuilt[k] = schemaRewrite(v, fn)
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

// Builds original to conforming key renames with numeric suffixes on collision
func schemaRenamePropertyKeys(props map[string]any, path string) map[string]string {
	renames := map[string]string{}
	taken := map[string]bool{}
	for k := range props {
		if schemaPropKeyRe.MatchString(k) {
			taken[k] = true
		}
	}
	for _, key := range sortedKeys(props) {
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
		slog.Debug("schema_sanitizer: renamed property key (provider key-pattern compat)",
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
	fn, ok := out["function"].(map[string]any)
	if !ok {
		return out
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		fn["parameters"] = schemaEmptyObject()
		return out
	}
	name := "<tool>"
	if n, ok := fn["name"].(string); ok {
		name = n
	}
	top, _ := schemaSanitizeNode(params, name).(map[string]any)
	if top == nil {
		top = map[string]any{}
	}
	top["type"] = "object"
	if _, ok := top["properties"].(map[string]any); !ok {
		top["properties"] = map[string]any{}
	}
	if _, ok := top["required"].([]any); !ok {
		top["required"] = []any{}
	}
	collapsed, _ := StripNullableUnions(top, true).(map[string]any)
	collapsed = schemaStripTopLevelCombinators(collapsed, name)
	fn["parameters"] = schemaStripRefSiblings(collapsed)
	return out
}

// Drops default beside $ref everywhere since Fireworks rejects that pairing
func schemaStripRefSiblings(node any) any {
	return schemaRewrite(node, func(out map[string]any) any {
		if _, ok := out["$ref"]; ok {
			for _, key := range schemaRefForbiddenSiblings {
				delete(out, key)
			}
		}
		return out
	})
}

// Drops combinators from the top level only because Codex rejects them there
func schemaStripTopLevelCombinators(params map[string]any, path string) map[string]any {
	if params == nil {
		return params
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = v
	}
	for _, key := range schemaTopLevelForbiddenKeys {
		if _, ok := out[key]; ok {
			slog.Debug("schema_sanitizer: stripped top-level combinator from tool parameters (strict-backend compat)",
				"path", path, "key", key)
			delete(out, key)
		}
	}
	return out
}

func schemaIsNullBranch(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	t, _ := m["type"].(string)
	return t == "null"
}

// Copies outer union metadata onto the replacement where absent
func schemaCarryUnionMeta(outer, replacement map[string]any, skipDefaultOnRef bool) {
	for _, metaKey := range schemaUnionMetaKeys {
		val, present := outer[metaKey]
		if !present {
			continue
		}
		if _, has := replacement[metaKey]; has {
			continue
		}
		if skipDefaultOnRef && metaKey == "default" {
			if _, isRef := replacement["$ref"]; isRef {
				continue
			}
		}
		replacement[metaKey] = val
	}
}

// StripNullableUnions collapses anyOf or oneOf nullable unions to the single non null branch
func StripNullableUnions(schema any, keepNullableHint bool) any {
	var collapse func(map[string]any) any
	collapse = func(stripped map[string]any) any {
		for _, key := range schemaUnionKeys {
			variants, ok := stripped[key].([]any)
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
				replacement := map[string]any{}
				if m, ok := nonNull[0].(map[string]any); ok {
					for k, v := range m {
						replacement[k] = v
					}
				}
				if keepNullableHint {
					if _, has := replacement["nullable"]; !has {
						replacement["nullable"] = true
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
	m, ok := branch.(map[string]any)
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
	return schemaRewrite(schema, func(out map[string]any) any {
		for _, key := range schemaUnionKeys {
			variants, ok := out[key].([]any)
			if !ok || len(variants) == 0 {
				continue
			}
			var nullBranches, constBranches []any
			for _, item := range variants {
				m, _ := item.(map[string]any)
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
				enum = append(enum, item.(map[string]any)["const"])
			}
			replacement := map[string]any{"type": branchType, "enum": enum}
			if len(nullBranches) > 0 {
				replacement["nullable"] = true
			}
			schemaCarryUnionMeta(out, replacement, false)
			return replacement
		}
		return out
	})
}

// Normalizes a type array into a single type, an anyOf of single types, or a null fallback
func schemaNormalizeTypeArray(value []any, out map[string]any) {
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
			out["type"] = "null"
		} else {
			out["type"] = "object"
		}
		return
	}
	if len(nonNull) == 1 {
		out["type"] = nonNull[0]
	} else {
		branches := make([]any, 0, len(nonNull))
		for _, t := range nonNull {
			branches = append(branches, map[string]any{"type": t})
		}
		out["anyOf"] = branches
	}
	if hasNull {
		if _, has := out["nullable"]; !has {
			out["nullable"] = true
		}
	}
}

// Recursively sanitizes a schema fragment, renaming keys and normalizing bare and array types
func schemaSanitizeNode(node any, path string) any {
	switch t := node.(type) {
	case string:
		if schemaBareTypeNames[t] {
			slog.Debug("schema_sanitizer: replacing bare-string schema with type object", "path", path, "type", t)
			if t == "object" {
				return schemaEmptyObject()
			}
			return map[string]any{"type": t}
		}
		slog.Debug("schema_sanitizer: replacing non-schema string with empty object schema", "path", path, "value", t)
		return schemaEmptyObject()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = schemaSanitizeNode(item, fmt.Sprintf("%s[%d]", path, i))
		}
		return out
	case map[string]any:
		return schemaSanitizeMap(t, path)
	default:
		return node
	}
}

func schemaSanitizeMap(node map[string]any, path string) map[string]any {
	propsIn, _ := node["properties"].(map[string]any)
	propRenames := map[string]string{}
	if propsIn != nil {
		propRenames = schemaRenamePropertyKeys(propsIn, path+".properties")
	}
	out := map[string]any{}
	for _, key := range sortedKeys(node) {
		value := node[key]
		switch {
		case key == "type" && isAnyList(value):
			schemaNormalizeTypeArray(toAnyList(value), out)
		case schemaMapKeys[key] && isMap(value):
			renames := map[string]string{}
			if key == "properties" {
				renames = propRenames
			}
			sub := map[string]any{}
			for k, v := range value.(map[string]any) {
				nk := k
				if r, ok := renames[k]; ok {
					nk = r
				}
				sub[nk] = schemaSanitizeNode(v, fmt.Sprintf("%s.%s.%s", path, key, nk))
			}
			out[key] = sub
		case key == "dependencies" && isMap(value):
			sub := map[string]any{}
			for k, v := range value.(map[string]any) {
				if isMap(v) {
					sub[k] = schemaSanitizeNode(v, fmt.Sprintf("%s.%s.%s", path, key, k))
				} else {
					sub[k] = schemaDeepCopy(v)
				}
			}
			out[key] = sub
		case key == "items" || key == "additionalProperties":
			if _, isBool := value.(bool); isBool {
				out[key] = value
			} else {
				out[key] = schemaSanitizeNode(value, path+"."+key)
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
					out[key] = remapped
					continue
				}
			}
			out[key] = schemaDeepCopy(value)
		case schemaChildKeys[key]:
			out[key] = schemaSanitizeNode(value, path+"."+key)
		default:
			out[key] = schemaDeepCopy(value)
		}
	}
	if propsIn != nil {
		var lifted []string
		for _, k := range sortedKeys(propsIn) {
			v, ok := propsIn[k].(map[string]any)
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
			required, _ := out["required"].([]any)
			if required == nil {
				required = []any{}
			}
			have := map[string]bool{}
			for _, r := range required {
				if s, ok := r.(string); ok {
					have[s] = true
				}
			}
			for _, key := range lifted {
				if !have[key] {
					required = append(required, key)
					have[key] = true
				}
			}
			out["required"] = required
		}
	}
	if t, _ := out["type"].(string); t == "object" {
		props, ok := out["properties"].(map[string]any)
		if !ok {
			props = map[string]any{}
			out["properties"] = props
		}
		filtered := []any{}
		if required, ok := out["required"].([]any); ok {
			for _, r := range required {
				if s, ok := r.(string); ok && schemaHasKey(props, s) {
					filtered = append(filtered, s)
				}
			}
		}
		out["required"] = filtered
	}
	return out
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
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

// Pre order walk yielding every map node before its values so the visitor may mutate it
func schemaDictNodes(node any, visit func(map[string]any)) {
	switch t := node.(type) {
	case map[string]any:
		visit(t)
		for _, k := range sortedKeys(t) {
			schemaDictNodes(t[k], visit)
		}
	case []any:
		for _, child := range t {
			schemaDictNodes(child, visit)
		}
	}
}

// Applies stripNode to every map node of each tool's parameters in place and counts removals
func schemaReactiveStrip(tools []map[string]any, stripNode func(map[string]any) int, logMsg string) ([]map[string]any, int) {
	stripped := 0
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		var params map[string]any
		if fn, ok := pyjson.Dict(tool["function"]); ok {
			params, _ = fn["parameters"].(map[string]any)
		}
		if params == nil {
			params, _ = tool["parameters"].(map[string]any)
		}
		if params != nil {
			schemaDictNodes(params, func(node map[string]any) { stripped += stripNode(node) })
		}
	}
	if stripped > 0 {
		slog.Info(logMsg, "count", stripped)
	}
	return tools, stripped
}

// StripPatternAndFormat removes pattern and format keywords in place after a llama.cpp grammar rejection
func StripPatternAndFormat(tools []map[string]any) ([]map[string]any, int) {
	return schemaReactiveStrip(tools, func(node map[string]any) int {
		isSchema := false
		for k := range node {
			if schemaMarkers[k] {
				isSchema = true
				break
			}
		}
		if !isSchema {
			return 0
		}
		hits := 0
		for k := range schemaStripOnRecoveryKeys {
			if _, ok := node[k]; ok {
				delete(node, k)
				hits++
			}
		}
		return hits
	}, "schema_sanitizer: stripped pattern/format keyword(s) from tool schemas (llama.cpp grammar-parse recovery)")
}

// StripSlashEnum removes enum keywords whose string values contain a slash after an xAI grammar rejection
func StripSlashEnum(tools []map[string]any) ([]map[string]any, int) {
	return schemaReactiveStrip(tools, func(node map[string]any) int {
		enum, ok := node["enum"].([]any)
		if !ok {
			return 0
		}
		for _, v := range enum {
			if s, ok := v.(string); ok && strings.Contains(s, "/") {
				delete(node, "enum")
				return 1
			}
		}
		return 0
	}, "schema_sanitizer: stripped enum keyword(s) containing '/' from tool schemas (xAI Responses grammar-compile recovery)")
}
