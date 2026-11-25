package provider

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
		return floatRepr(v), true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// Type array folded into a single type, an anyOf of single types, or null, with nullable for a null member
func schemaTypeArrayInto(value []any, out *jsonutil.Object) {
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
		if _, ok := out.Get("nullable"); !ok {
			out.Set("nullable", true)
		}
	}
}

// Keeps union alternatives with their branch local structural constraints
func normalizeGeminiTypeArray(typeArray []any, cleaned *jsonutil.Object) {
	derived := jsonutil.NewObject()
	schemaTypeArrayInto(typeArray, derived)
	if branches, ok := derived.Value("anyOf").([]any); ok {
		constraints := jsonutil.NewObject()
		if v, ok := cleaned.Get("anyOf"); ok {
			constraints.Set("anyOf", v)
		}
		structural := jsonutil.NewObject()
		for _, kind := range geminiStructuralKinds {
			for _, key := range geminiStructuralKeys[kind] {
				if v, ok := cleaned.Take(key); ok {
					structural.Set(key, v)
				}
			}
		}
		out := make([]any, 0, len(branches))
		for _, raw := range branches {
			branch := raw.(*jsonutil.Object)
			merged := branch.Clone()
			merged.Merge(constraints)
			typ, _ := branch.Value("type").(string)
			allowed := map[string]bool{}
			for _, key := range geminiStructuralKeys[typ] {
				allowed[key] = true
			}
			for _, key := range structural.Keys() {
				if allowed[key] {
					merged.Set(key, structural.Value(key))
				}
			}
			out = append(out, sanitizeGeminiSchema(merged))
		}
		cleaned.Set("anyOf", out)
	} else {
		cleaned.Set("type", derived.Value("type"))
	}
	if b, _ := derived.Value("nullable").(bool); b {
		cleaned.Set("nullable", true)
	}
}

// Gemini compatible copy of a tool parameter schema keeping only the documented subset, recursively, dict order kept
func sanitizeGeminiSchema(schema any) *jsonutil.Object {
	m, ok := jsonutil.AsObject(schema)
	if !ok || m == nil {
		return jsonutil.NewObject()
	}
	cleaned := jsonutil.NewObject()
	for _, key := range m.Keys() {
		value := m.Value(key)
		if !geminiSchemaAllowedKeys[key] {
			continue
		}
		switch key {
		case "properties":
			if props, ok := jsonutil.AsObject(value); ok && props != nil {
				out := jsonutil.NewObject()
				for _, name := range props.Keys() {
					out.Set(name, sanitizeGeminiSchema(props.Value(name)))
				}
				cleaned.Set(key, out)
			}
		case "items":
			cleaned.Set(key, sanitizeGeminiSchema(value))
		case "anyOf":
			if list, ok := value.([]any); ok {
				out := make([]any, 0, len(list))
				for _, item := range list {
					if _, isDict := jsonutil.AsMap(item); isDict {
						out = append(out, sanitizeGeminiSchema(item))
					}
				}
				cleaned.Set(key, out)
			}
		default:
			cleaned.Set(key, value)
		}
	}
	typeArray, isArray := cleaned.Value("type").([]any)
	if isArray {
		cleaned.Delete("type")
		normalizeGeminiTypeArray(typeArray, cleaned)
	}
	if enumVal, ok := cleaned.Value("enum").([]any); ok {
		typ, _ := cleaned.Value("type").(string)
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
	if required, ok := cleaned.Value("required").([]any); ok {
		props, _ := jsonutil.AsMap(cleaned.Value("properties"))
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
func sanitizeGeminiToolParameters(parameters any) *jsonutil.Object {
	if cleaned := sanitizeGeminiSchema(parameters); cleaned.Len() > 0 {
		return cleaned
	}
	return jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject())
}

// Node a same document JSON pointer names under root, nil when the pointer does not resolve to a dict
func resolveLocalRef(root *jsonutil.Object, ref string) *jsonutil.Object {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var node any = root
	for _, raw := range strings.Split(ref[2:], "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		m, ok := node.(*jsonutil.Object)
		if !ok {
			return nil
		}
		next, ok := m.Get(part)
		if !ok {
			return nil
		}
		node = next
	}
	out, _ := node.(*jsonutil.Object)
	return out
}

// Inlines same document refs, failing on an unresolvable or circular ref or an exhausted budget
func inlineGeminiRefs(node any, root *jsonutil.Object, budget *int, stack []string) (any, error) {
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
	case *jsonutil.Object:
		ref, isRef := t.Value("$ref").(string)
		if !isRef {
			out := jsonutil.NewObject()
			for _, k := range t.Keys() {
				x, err := inlineGeminiRefs(t.Value(k), root, budget, stack)
				if err != nil {
					return nil, err
				}
				out.Set(k, x)
			}
			return out, nil
		}
		for _, s := range stack {
			if s == ref {
				return nil, errors.New("circular $ref " + anyutil.Quote(ref))
			}
		}
		*budget--
		if *budget < 0 {
			return nil, errors.New("$ref expansion budget exhausted")
		}
		target := resolveLocalRef(root, ref)
		if target == nil {
			return nil, errors.New("unresolvable $ref " + anyutil.Quote(ref))
		}
		inlined, err := inlineGeminiRefs(target, root, budget, append(append([]string{}, stack...), ref))
		if err != nil {
			return nil, err
		}
		siblings := jsonutil.NewObject()
		for _, k := range t.Keys() {
			if k != "$ref" {
				siblings.Set(k, t.Value(k))
			}
		}
		if siblings.Len() == 0 {
			return inlined, nil
		}
		resolved, err := inlineGeminiRefs(siblings, root, budget, stack)
		if err != nil {
			return nil, err
		}
		merged := inlined.(*jsonutil.Object).Clone()
		merged.Merge(resolved.(*jsonutil.Object))
		return merged, nil
	}
	return node, nil
}

// Full JSON schema for parametersJsonSchema with root $schema dropped, local refs inlined and an object root guaranteed
func prepareGeminiToolParameters(parameters any) *jsonutil.Object {
	src, ok := jsonutil.AsObject(parameters)
	if !ok || src == nil || src.Len() == 0 {
		return jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject())
	}
	schema := jsonutil.Ordered(src).(*jsonutil.Object)
	schema.Delete("$schema")
	budget := geminiMaxRefExpansions
	inlined, err := inlineGeminiRefs(schema, schema, &budget, nil)
	if err != nil {
		pkgLog().Debug("Gemini tool schema kept as-is ($ref inlining skipped): " + err.Error())
		return schema
	}
	schema = inlined.(*jsonutil.Object)
	schema.Delete("$defs")
	schema.Delete("definitions")
	if schema.Len() == 0 {
		return jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject())
	}
	if t, _ := schema.Value("type").(string); t == "object" {
		if _, has := schema.Get("properties"); !has {
			schema.Set("properties", jsonutil.NewObject())
		}
	}
	return schema
}

// Whether a value is a dict as isinstance(value, dict) sees it, a plain map or an ordered object
func geminiIsDict(v any) bool {
	_, ok := jsonutil.AsMap(v)
	return ok
}
