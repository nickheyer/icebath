package provider

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// OpenAPI Schema subset the legacy Gemini parameters field accepts
var geminiSchemaAllowedKeys = map[string]bool{
	"type": true, "format": true, "title": true, "description": true, "nullable": true, "enum": true, "maxItems": true,
	"minItems": true, "properties": true, "required": true, "minProperties": true, "maxProperties": true, "minLength": true,
	"maxLength": true, "pattern": true, "example": true, "anyOf": true, "propertyOrdering": true, "default": true,
	"items": true, "minimum": true, "maximum": true,
}

// Structural keys that belong on the typed branch of a type union
var geminiStructuralKeys = map[string][]string{
	"array":  {"items", "minItems", "maxItems"},
	"object": {"properties", "required", "minProperties", "maxProperties", "propertyOrdering"},
}

// Same document refs a JSON schema may expand before inlining gives up
const geminiMaxRefExpansions = 256

// Gemini safe string for a scalar enum entry, false when the entry is dropped
func stringifyGeminiEnumValue(item any) (string, bool) {
	switch v := item.(type) {
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	case string:
		return v, true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", false
		}
		return pyFloatRepr(v), true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// Type array folded into a single type, an anyOf of single types, or null, with nullable for a null member
func schemaTypeArrayInto(value []any, out map[string]any) {
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
		if _, ok := out["nullable"]; !ok {
			out["nullable"] = true
		}
	}
}

// Keeps union alternatives with their branch local structural constraints
func normalizeGeminiTypeArray(typeArray []any, cleaned map[string]any) {
	derived := map[string]any{}
	schemaTypeArrayInto(typeArray, derived)
	if branches, ok := derived["anyOf"].([]any); ok {
		constraints := map[string]any{}
		if v, ok := cleaned["anyOf"]; ok {
			constraints["anyOf"] = v
		}
		structural := map[string]any{}
		for _, kind := range []string{"array", "object"} {
			for _, key := range geminiStructuralKeys[kind] {
				if v, ok := cleaned[key]; ok {
					structural[key] = v
					delete(cleaned, key)
				}
			}
		}
		out := make([]any, 0, len(branches))
		for _, raw := range branches {
			branch := raw.(map[string]any)
			merged := map[string]any{}
			for k, v := range branch {
				merged[k] = v
			}
			for k, v := range constraints {
				merged[k] = v
			}
			typ, _ := branch["type"].(string)
			for _, key := range geminiStructuralKeys[typ] {
				if v, ok := structural[key]; ok {
					merged[key] = v
				}
			}
			out = append(out, sanitizeGeminiSchema(merged))
		}
		cleaned["anyOf"] = out
	} else {
		cleaned["type"] = derived["type"]
	}
	if b, _ := derived["nullable"].(bool); b {
		cleaned["nullable"] = true
	}
}

// Gemini compatible copy of a tool parameter schema keeping only the documented subset, recursively
func sanitizeGeminiSchema(schema any) map[string]any {
	m, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	cleaned := map[string]any{}
	for key, value := range m {
		if !geminiSchemaAllowedKeys[key] {
			continue
		}
		switch key {
		case "properties":
			if props, ok := value.(map[string]any); ok {
				out := make(map[string]any, len(props))
				for name, sub := range props {
					out[name] = sanitizeGeminiSchema(sub)
				}
				cleaned[key] = out
			}
		case "items":
			cleaned[key] = sanitizeGeminiSchema(value)
		case "anyOf":
			if list, ok := value.([]any); ok {
				out := make([]any, 0, len(list))
				for _, item := range list {
					if _, ok := item.(map[string]any); ok {
						out = append(out, sanitizeGeminiSchema(item))
					}
				}
				cleaned[key] = out
			}
		default:
			cleaned[key] = value
		}
	}
	typeArray, isArray := cleaned["type"].([]any)
	if isArray {
		delete(cleaned, "type")
		normalizeGeminiTypeArray(typeArray, cleaned)
	}
	if enumVal, ok := cleaned["enum"].([]any); ok {
		typ, _ := cleaned["type"].(string)
		if isArray || typ == "integer" || typ == "number" || typ == "boolean" {
			seen := map[string]bool{}
			var stringified []any
			for _, v := range enumVal {
				s, ok := stringifyGeminiEnumValue(v)
				if !ok || seen[s] {
					continue
				}
				seen[s] = true
				stringified = append(stringified, s)
			}
			if len(stringified) > 0 {
				cleaned["enum"] = stringified
			} else {
				delete(cleaned, "enum")
			}
		}
	}
	if required, ok := cleaned["required"].([]any); ok {
		props, _ := cleaned["properties"].(map[string]any)
		var valid []any
		for _, name := range required {
			if s, ok := name.(string); ok {
				if _, ok := props[s]; ok {
					valid = append(valid, s)
				}
			}
		}
		if len(valid) == 0 {
			delete(cleaned, "required")
		} else if len(valid) != len(required) {
			cleaned["required"] = valid
		}
	}
	return cleaned
}

// Valid Gemini object schema for the legacy parameters field
func sanitizeGeminiToolParameters(parameters any) map[string]any {
	if out := sanitizeGeminiSchema(parameters); len(out) > 0 {
		return out
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// Node a same document JSON pointer names, nil when it does not resolve to an object
func resolveLocalRef(root map[string]any, ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var node any = root
	for _, raw := range strings.Split(ref[2:], "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		next, ok := m[part]
		if !ok {
			return nil
		}
		node = next
	}
	out, _ := node.(map[string]any)
	return out
}

// Inlines same document refs, failing on an unresolvable or circular ref or an exhausted budget
func inlineGeminiRefs(node any, root map[string]any, budget *int, stack []string) (any, error) {
	switch v := node.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			x, err := inlineGeminiRefs(item, root, budget, stack)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	case map[string]any:
		ref, isRef := v["$ref"].(string)
		if !isRef {
			out := make(map[string]any, len(v))
			for k, val := range v {
				x, err := inlineGeminiRefs(val, root, budget, stack)
				if err != nil {
					return nil, err
				}
				out[k] = x
			}
			return out, nil
		}
		for _, s := range stack {
			if s == ref {
				return nil, errors.New("circular $ref " + pyStringRepr(ref))
			}
		}
		*budget--
		if *budget < 0 {
			return nil, errors.New("$ref expansion budget exhausted")
		}
		target := resolveLocalRef(root, ref)
		if target == nil {
			return nil, errors.New("unresolvable $ref " + pyStringRepr(ref))
		}
		inlined, err := inlineGeminiRefs(target, root, budget, append(append([]string{}, stack...), ref))
		if err != nil {
			return nil, err
		}
		siblings := map[string]any{}
		for k, val := range v {
			if k != "$ref" {
				siblings[k] = val
			}
		}
		if len(siblings) == 0 {
			return inlined, nil
		}
		resolved, err := inlineGeminiRefs(siblings, root, budget, stack)
		if err != nil {
			return nil, err
		}
		merged := map[string]any{}
		for k, val := range inlined.(map[string]any) {
			merged[k] = val
		}
		for k, val := range resolved.(map[string]any) {
			merged[k] = val
		}
		return merged, nil
	}
	return node, nil
}

// Full JSON schema for parametersJsonSchema with root $schema dropped, local refs inlined and an object root
func prepareGeminiToolParameters(parameters any) map[string]any {
	params, ok := parameters.(map[string]any)
	if !ok || len(params) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	schema := deepCopyAny(params).(map[string]any)
	delete(schema, "$schema")
	budget := geminiMaxRefExpansions
	inlined, err := inlineGeminiRefs(schema, schema, &budget, nil)
	if err != nil {
		catalogLog.Debug("Gemini tool schema kept as-is ($ref inlining skipped): " + err.Error())
		return schema
	}
	schema = inlined.(map[string]any)
	delete(schema, "$defs")
	delete(schema, "definitions")
	if len(schema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if schema["type"] == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
	return schema
}
