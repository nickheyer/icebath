package tools

import (
	"encoding/json"
	"math"
	"strings"
)

// Keywords whose value maps names to schemas, never a schema itself
var schemaMaps = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}

// Union keywords, and the keywords a union's replacement takes from it
var (
	unionKeys = []string{"anyOf", "oneOf"}
	unionMeta = []string{"description", "title", "default", "examples", "deprecated", "readOnly", "writeOnly"}
	// Combinators strict providers refuse at the top of the parameters
	topForbidden = []string{"anyOf", "oneOf", "allOf", "not", "enum"}
	// Keywords some providers refuse beside $ref
	refSiblings = []string{"default"}
)

// Input schema of a tool in the one form every provider takes: an object at the top with properties and required naming only properties,
// legacy definitions moved to $defs, a union with null collapsed to its other branch marked nullable, a union of same typed constants
// turned into an enum, type lists made single, top level combinators and $ref siblings providers refuse dropped
func normalize(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	node := rewrite(clone(schema), true)
	top, ok := node.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	for _, key := range topForbidden {
		delete(top, key)
	}
	top["type"] = "object"
	if _, ok := top["properties"].(map[string]any); !ok {
		top["properties"] = map[string]any{}
	}
	return top
}

// Deep copy of a JSON value
func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = clone(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = clone(item)
		}
		return out
	}
	return v
}

// One schema node normalized, its children first
func rewrite(node any, root bool) any {
	switch x := node.(type) {
	case []any:
		for i, item := range x {
			x[i] = rewrite(item, false)
		}
		return x
	case string:
		// A bare type name where a schema belongs
		switch x {
		case "object":
			return map[string]any{"type": "object", "properties": map[string]any{}}
		case "string", "number", "integer", "boolean", "array", "null":
			return map[string]any{"type": x}
		}
		return map[string]any{}
	case map[string]any:
		if defs, ok := x["definitions"]; ok {
			if _, taken := x["$defs"]; !taken {
				x["$defs"] = defs
			}
			delete(x, "definitions")
		}
		for key, value := range x {
			if inner, ok := value.(map[string]any); ok && schemaMaps[key] {
				for name, schema := range inner {
					inner[name] = rewrite(schema, false)
				}
				continue
			}
			switch key {
			case "items", "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames", "unevaluatedProperties", "unevaluatedItems":
				if _, ok := value.(bool); !ok {
					x[key] = rewrite(value, false)
				}
			case "anyOf", "oneOf", "allOf", "prefixItems":
				x[key] = rewrite(value, false)
			}
		}
		if ref, ok := x["$ref"].(string); ok {
			if rest, legacy := strings.CutPrefix(ref, "#/definitions/"); legacy {
				x["$ref"] = "#/$defs/" + rest
			}
			for _, key := range refSiblings {
				delete(x, key)
			}
		}
		if types, ok := x["type"].([]any); ok {
			typeList(x, types)
		}
		node = collapseConst(collapseNull(x))
		if m, ok := node.(map[string]any); ok {
			repair(m, root)
		}
		return node
	}
	return node
}

// A type list made one type, a union of single types or null
func typeList(x map[string]any, types []any) {
	var kinds []string
	nullable := false
	for _, t := range types {
		name, _ := t.(string)
		switch name {
		case "":
		case "null":
			nullable = true
		default:
			kinds = append(kinds, name)
		}
	}
	switch len(kinds) {
	case 0:
		x["type"] = "object"
		if nullable {
			x["type"] = "null"
		}
	case 1:
		x["type"] = kinds[0]
	default:
		delete(x, "type")
		branches := make([]any, len(kinds))
		for i, k := range kinds {
			branches[i] = map[string]any{"type": k}
		}
		x["anyOf"] = branches
	}
	if nullable && len(kinds) > 0 {
		if _, set := x["nullable"]; !set {
			x["nullable"] = true
		}
	}
}

func isNull(branch any) bool {
	m, ok := branch.(map[string]any)
	return ok && m["type"] == "null"
}

// A union of one branch and null replaced by that branch marked nullable
func collapseNull(x map[string]any) any {
	for _, key := range unionKeys {
		variants, ok := x[key].([]any)
		if !ok {
			continue
		}
		var rest []any
		for _, v := range variants {
			if !isNull(v) {
				rest = append(rest, v)
			}
		}
		if len(rest) != 1 || len(rest) == len(variants) {
			continue
		}
		replacement, ok := rest[0].(map[string]any)
		if !ok {
			replacement = map[string]any{}
		}
		if _, set := replacement["nullable"]; !set {
			replacement["nullable"] = true
		}
		for _, meta := range unionMeta {
			if v, present := x[meta]; present {
				if _, has := replacement[meta]; !has && !(meta == "default" && replacement["$ref"] != nil) {
					replacement[meta] = v
				}
			}
		}
		return replacement
	}
	return x
}

// Type of a constant branch, empty when the branch is not a constant alone
func constType(branch any) string {
	m, ok := branch.(map[string]any)
	if !ok {
		return ""
	}
	value, has := m["const"]
	if !has {
		return ""
	}
	for k := range m {
		if k != "const" && k != "type" && k != "title" && k != "description" {
			return ""
		}
	}
	kind := ""
	switch v := value.(type) {
	case bool:
		kind = "boolean"
	case string:
		kind = "string"
	case float64:
		kind = "number"
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			kind = "integer"
		}
	case int, int64:
		kind = "integer"
	case json.Number:
		kind = "integer"
		if strings.ContainsAny(v.String(), ".eE") {
			kind = "number"
		}
	default:
		return ""
	}
	declared, has := m["type"]
	if !has || declared == kind {
		return kind
	}
	if declared == "number" && kind == "integer" {
		return "number"
	}
	return ""
}

// A union of constants of one type turned into an enum of them
func collapseConst(node any) any {
	x, ok := node.(map[string]any)
	if !ok {
		return node
	}
	for _, key := range unionKeys {
		variants, ok := x[key].([]any)
		if !ok || len(variants) == 0 {
			continue
		}
		var nulls, consts []any
		for _, v := range variants {
			if m, _ := v.(map[string]any); isNull(v) && m["const"] == nil {
				nulls = append(nulls, v)
			} else {
				consts = append(consts, v)
			}
		}
		if len(nulls) > 1 || len(consts) == 0 {
			continue
		}
		kind := ""
		same := true
		for i, v := range consts {
			t := constType(v)
			if t == "" || i > 0 && t != kind {
				same = false
				break
			}
			kind = t
		}
		if !same {
			continue
		}
		enum := make([]any, len(consts))
		for i, v := range consts {
			enum[i] = v.(map[string]any)["const"]
		}
		replacement := map[string]any{"type": kind, "enum": enum}
		if len(nulls) > 0 {
			replacement["nullable"] = true
		}
		for _, meta := range unionMeta {
			if v, present := x[meta]; present {
				replacement[meta] = v
			}
		}
		return replacement
	}
	return x
}

// Object shape filled in: a missing object type where properties or required show one, properties present, required naming properties only
func repair(x map[string]any, root bool) {
	_, hasProps := x["properties"]
	_, hasType := x["type"]
	if !root && !hasProps && !hasType {
		return
	}
	_, hasRequired := x["required"]
	if t, _ := x["type"].(string); t == "" && (hasProps || hasRequired) {
		x["type"] = "object"
	}
	if x["type"] != "object" {
		return
	}
	props, ok := x["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		x["properties"] = props
	}
	kept := []any{}
	if required, ok := x["required"].([]any); ok {
		for _, r := range required {
			if name, ok := r.(string); ok {
				if _, in := props[name]; in {
					kept = append(kept, name)
				}
			}
		}
	}
	x["required"] = kept
}
