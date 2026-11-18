package provider

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// OpenAPI Schema subset the legacy Gemini parameters field accepts
var geminiSchemaAllowedKeys = map[string]bool{
	"type": true, "format": true, "title": true, "description": true, "nullable": true, "enum": true, "maxItems": true,
	"minItems": true, "properties": true, "required": true, "minProperties": true, "maxProperties": true, "minLength": true,
	"maxLength": true, "pattern": true, "example": true, "anyOf": true, "propertyOrdering": true, "default": true,
	"items": true, "minimum": true, "maximum": true,
}

// Structural keys that belong on the typed branch of a type union, array first then object
var (
	geminiStructuralKinds = []string{"array", "object"}
	geminiStructuralKeys  = map[string][]string{
		"array":  {"items", "minItems", "maxItems"},
		"object": {"properties", "required", "minProperties", "maxProperties", "propertyOrdering"},
	}
)

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
	case *big.Int:
		return v.String(), true
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
func schemaTypeArrayInto(value []any, out *pyjson.Object) {
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
			branches = append(branches, pyjson.ObjectOf("type", t))
		}
		out.Set("anyOf", branches)
	}
	if hasNull {
		if _, ok := out.Get("nullable"); !ok {
			out.Set("nullable", true)
		}
	}
}

// Keeps union alternatives with their branch local structural constraints
func normalizeGeminiTypeArray(typeArray []any, cleaned *pyjson.Object) {
	derived := pyjson.NewObject()
	schemaTypeArrayInto(typeArray, derived)
	if branches, ok := derived.Vals["anyOf"].([]any); ok {
		constraints := pyjson.NewObject()
		if v, ok := cleaned.Get("anyOf"); ok {
			constraints.Set("anyOf", v)
		}
		structural := pyjson.NewObject()
		for _, kind := range geminiStructuralKinds {
			for _, key := range geminiStructuralKeys[kind] {
				if v, ok := cleaned.Pop(key); ok {
					structural.Set(key, v)
				}
			}
		}
		out := make([]any, 0, len(branches))
		for _, raw := range branches {
			branch := raw.(*pyjson.Object)
			merged := branch.Copy()
			merged.Update(constraints)
			typ, _ := branch.Vals["type"].(string)
			allowed := map[string]bool{}
			for _, key := range geminiStructuralKeys[typ] {
				allowed[key] = true
			}
			for _, key := range structural.Keys {
				if allowed[key] {
					merged.Set(key, structural.Vals[key])
				}
			}
			out = append(out, sanitizeGeminiSchema(merged))
		}
		cleaned.Set("anyOf", out)
	} else {
		cleaned.Set("type", derived.Vals["type"])
	}
	if b, _ := derived.Vals["nullable"].(bool); b {
		cleaned.Set("nullable", true)
	}
}

// Gemini compatible copy of a tool parameter schema keeping only the documented subset, recursively, dict order kept
func sanitizeGeminiSchema(schema any) *pyjson.Object {
	m, ok := pyjson.AsObject(schema)
	if !ok || m == nil {
		return pyjson.NewObject()
	}
	cleaned := pyjson.NewObject()
	for _, key := range m.Keys {
		value := m.Vals[key]
		if !geminiSchemaAllowedKeys[key] {
			continue
		}
		switch key {
		case "properties":
			if props, ok := pyjson.AsObject(value); ok && props != nil {
				out := pyjson.NewObject()
				for _, name := range props.Keys {
					out.Set(name, sanitizeGeminiSchema(props.Vals[name]))
				}
				cleaned.Set(key, out)
			}
		case "items":
			cleaned.Set(key, sanitizeGeminiSchema(value))
		case "anyOf":
			if list, ok := value.([]any); ok {
				out := make([]any, 0, len(list))
				for _, item := range list {
					if _, isDict := pyjson.Dict(item); isDict {
						out = append(out, sanitizeGeminiSchema(item))
					}
				}
				cleaned.Set(key, out)
			}
		default:
			cleaned.Set(key, value)
		}
	}
	typeArray, isArray := cleaned.Vals["type"].([]any)
	if isArray {
		cleaned.Delete("type")
		normalizeGeminiTypeArray(typeArray, cleaned)
	}
	if enumVal, ok := cleaned.Vals["enum"].([]any); ok {
		typ, _ := cleaned.Vals["type"].(string)
		if isArray || typ == "integer" || typ == "number" || typ == "boolean" {
			seen := map[string]bool{}
			stringified := []any{}
			for _, v := range enumVal {
				s, keep := stringifyGeminiEnumValue(v)
				if !keep || seen[s] {
					continue
				}
				seen[s] = true
				stringified = append(stringified, s)
			}
			if len(stringified) > 0 {
				cleaned.Set("enum", stringified)
			} else {
				cleaned.Delete("enum")
			}
		}
	}
	if required, ok := cleaned.Vals["required"].([]any); ok {
		props, _ := pyjson.Dict(cleaned.Vals["properties"])
		valid := []any{}
		for _, name := range required {
			if s, ok := name.(string); ok {
				if _, has := props[s]; has {
					valid = append(valid, s)
				}
			}
		}
		if len(valid) == 0 {
			cleaned.Delete("required")
		} else if len(valid) != len(required) {
			cleaned.Set("required", valid)
		}
	}
	return cleaned
}

// Tool parameters normalized to a valid Gemini object schema
func sanitizeGeminiToolParameters(parameters any) *pyjson.Object {
	if cleaned := sanitizeGeminiSchema(parameters); cleaned.Len() > 0 {
		return cleaned
	}
	return pyjson.ObjectOf("type", "object", "properties", pyjson.NewObject())
}

// Node a same document JSON pointer names under root, nil when the pointer does not resolve to a dict
func resolveLocalRef(root *pyjson.Object, ref string) *pyjson.Object {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var node any = root
	for _, raw := range strings.Split(ref[2:], "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		m, ok := node.(*pyjson.Object)
		if !ok {
			return nil
		}
		next, ok := m.Get(part)
		if !ok {
			return nil
		}
		node = next
	}
	out, _ := node.(*pyjson.Object)
	return out
}

// Inlines same document refs, failing on an unresolvable or circular ref or an exhausted budget
func inlineGeminiRefs(node any, root *pyjson.Object, budget *int, stack []string) (any, error) {
	switch t := node.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			x, err := inlineGeminiRefs(item, root, budget, stack)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case *pyjson.Object:
		ref, isRef := t.Vals["$ref"].(string)
		if !isRef {
			out := pyjson.NewObject()
			for _, k := range t.Keys {
				x, err := inlineGeminiRefs(t.Vals[k], root, budget, stack)
				if err != nil {
					return nil, err
				}
				out.Set(k, x)
			}
			return out, nil
		}
		for _, s := range stack {
			if s == ref {
				return nil, errors.New("circular $ref " + pyRepr(ref))
			}
		}
		*budget--
		if *budget < 0 {
			return nil, errors.New("$ref expansion budget exhausted")
		}
		target := resolveLocalRef(root, ref)
		if target == nil {
			return nil, errors.New("unresolvable $ref " + pyRepr(ref))
		}
		inlined, err := inlineGeminiRefs(target, root, budget, append(append([]string{}, stack...), ref))
		if err != nil {
			return nil, err
		}
		siblings := pyjson.NewObject()
		for _, k := range t.Keys {
			if k != "$ref" {
				siblings.Set(k, t.Vals[k])
			}
		}
		if siblings.Len() == 0 {
			return inlined, nil
		}
		resolved, err := inlineGeminiRefs(siblings, root, budget, stack)
		if err != nil {
			return nil, err
		}
		merged := inlined.(*pyjson.Object).Copy()
		merged.Update(resolved.(*pyjson.Object))
		return merged, nil
	}
	return node, nil
}

// Full JSON schema for parametersJsonSchema with root $schema dropped, local refs inlined and an object root guaranteed
func prepareGeminiToolParameters(parameters any) *pyjson.Object {
	src, ok := pyjson.AsObject(parameters)
	if !ok || src == nil || src.Len() == 0 {
		return pyjson.ObjectOf("type", "object", "properties", pyjson.NewObject())
	}
	schema := pyjson.Ordered(src).(*pyjson.Object)
	schema.Delete("$schema")
	budget := geminiMaxRefExpansions
	inlined, err := inlineGeminiRefs(schema, schema, &budget, nil)
	if err != nil {
		pkgLog().Debug("Gemini tool schema kept as-is ($ref inlining skipped): " + err.Error())
		return schema
	}
	schema = inlined.(*pyjson.Object)
	schema.Delete("$defs")
	schema.Delete("definitions")
	if schema.Len() == 0 {
		return pyjson.ObjectOf("type", "object", "properties", pyjson.NewObject())
	}
	if t, _ := schema.Vals["type"].(string); t == "object" {
		if _, has := schema.Get("properties"); !has {
			schema.Set("properties", pyjson.NewObject())
		}
	}
	return schema
}

// Whether a value is a dict as isinstance(value, dict) sees it, a plain map or an ordered object
func geminiIsDict(v any) bool {
	_, ok := pyjson.Dict(v)
	return ok
}
