package gemini

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Object schema with no properties
const emptyObjectSchema = `{"type":"object","properties":{}}`

// Most reference expansions one tool schema may take, bounding circular models
const maxRefExpansions = 256

// Returned inside the package when a schema reference cannot be inlined
var errRef = errors.New("gemini: schema reference cannot be inlined")

// Reports whether the legacy parameters field accepts a schema keyword
func allowedSchemaKey(key string) bool {
	switch key {
	case "type", "format", "title", "description", "nullable", "enum", "maxItems", "minItems", "properties", "required",
		"minProperties", "maxProperties", "minLength", "maxLength", "pattern", "example", "anyOf", "propertyOrdering",
		"default", "items", "minimum", "maximum":
		return true
	}
	return false
}

// Structural keywords that belong on a typed branch of that type and not on its typeless parent
func structuralKeys(typ string) []string {
	switch typ {
	case "array":
		return []string{"items", "minItems", "maxItems"}
	case "object":
		return []string{"properties", "required", "minProperties", "maxProperties", "propertyOrdering"}
	}
	return nil
}

// String Gemini accepts for a scalar enum entry, false for an entry it cannot take
func enumString(v wire.Value) (string, bool) {
	switch v.Kind() {
	case wire.KindBool:
		if v.True() {
			return "true", true
		}
		return "false", true
	case wire.KindNumber:
		f, ok := v.Float()
		if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
			return "", false
		}
		if !strings.ContainsAny(string(v.Raw()), ".eE") {
			return string(v.Raw()), true
		}
		return string(wire.Float(f)), true
	case wire.KindString:
		return v.Text(), true
	}
	return "", false
}

// Replaces a type array with one type or a union of typed branches that carry their own structure
func normalizeTypeArray(types []wire.Value, d *wire.Dict) {
	hasNull := false
	var named []string
	for _, t := range types {
		if s, ok := t.Str(); ok && s == "null" {
			hasNull = true
		} else if ok {
			named = append(named, s)
		}
	}
	switch len(named) {
	case 0:
		typ := "object"
		if hasNull {
			typ = "null"
		}
		d.SetString("type", typ)
		return
	case 1:
		d.SetString("type", named[0])
	default:
		outer := d.Get("anyOf")
		structural := &wire.Dict{}
		for _, typ := range [...]string{"array", "object"} {
			for _, key := range structuralKeys(typ) {
				if v := d.Get(key); v.Exists() {
					structural.Set(key, v.Raw())
					d.Delete(key)
				}
			}
		}
		branches := make([]json.RawMessage, len(named))
		for i, typ := range named {
			branch := (&wire.Dict{}).SetString("type", typ)
			if outer.Exists() {
				branch.Set("anyOf", outer.Raw())
			}
			for _, key := range structuralKeys(typ) {
				if v := structural.Get(key); v.Exists() {
					branch.Set(key, v.Raw())
				}
			}
			branches[i] = sanitizeSchema(branch.Value())
		}
		d.Set("anyOf", wire.Array(branches...))
	}
	if hasNull {
		d.SetBool("nullable", true)
	}
}

// Copy of a schema holding only the OpenAPI subset the legacy parameters field accepts
func sanitizeSchema(schema wire.Value) json.RawMessage {
	d := &wire.Dict{}
	if !schema.IsObject() {
		return d.JSON()
	}
	for _, f := range schema.Fields() {
		if !allowedSchemaKey(f.Key) {
			continue
		}
		switch f.Key {
		case "properties":
			if f.Value.IsObject() {
				props := &wire.Dict{}
				for _, p := range f.Value.Fields() {
					props.Set(p.Key, sanitizeSchema(p.Value))
				}
				d.Set(f.Key, props.JSON())
			}
		case "items":
			d.Set(f.Key, sanitizeSchema(f.Value))
		case "anyOf":
			if f.Value.IsArray() {
				var branches []json.RawMessage
				for _, item := range f.Value.Elems() {
					if item.IsObject() {
						branches = append(branches, sanitizeSchema(item))
					}
				}
				d.Set(f.Key, wire.Array(branches...))
			}
		default:
			d.Set(f.Key, f.Value.Raw())
		}
	}
	typeArray := d.Get("type")
	if typeArray.IsArray() {
		d.Delete("type")
		normalizeTypeArray(typeArray.Elems(), d)
	}
	if enum := d.Get("enum"); enum.IsArray() {
		switch typ := d.Get("type").Text(); {
		case typeArray.IsArray(), typ == "integer", typ == "number", typ == "boolean":
			var seen []string
			for _, e := range enum.Elems() {
				if s, ok := enumString(e); ok && !slices.Contains(seen, s) {
					seen = append(seen, s)
				}
			}
			if len(seen) > 0 {
				d.Set("enum", wire.Strings(seen))
			} else {
				d.Delete("enum")
			}
		}
	}
	if required := d.Get("required"); required.IsArray() {
		props := d.Get("properties")
		var valid []json.RawMessage
		for _, name := range required.Elems() {
			if s, ok := name.Str(); ok && props.IsObject() && props.Get(s).Exists() {
				valid = append(valid, name.Raw())
			}
		}
		switch {
		case len(valid) == 0:
			d.Delete("required")
		case len(valid) != len(required.Elems()):
			d.Set("required", wire.Array(valid...))
		}
	}
	return d.JSON()
}

// Tool parameters as a valid object schema in the legacy OpenAPI subset
func sanitizeParameters(parameters wire.Value) json.RawMessage {
	out := sanitizeSchema(parameters)
	if v, err := wire.ParseValue(out); err != nil || len(v.Fields()) == 0 {
		return json.RawMessage(emptyObjectSchema)
	}
	return out
}

// Schema a same-document pointer names, absent when the pointer leads nowhere
func resolveRef(root wire.Value, ref string) wire.Value {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return wire.Value{}
	}
	node := root
	for _, part := range strings.Split(rest, "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		if node = node.Get(part); !node.Exists() {
			return wire.Value{}
		}
	}
	if !node.IsObject() {
		return wire.Value{}
	}
	return node
}

// Inlines every same-document reference under a node, failing on a circular or dangling one or a spent budget
func inlineRefs(node, root wire.Value, budget *int, stack []string) (json.RawMessage, error) {
	if node.IsArray() {
		elems := node.Elems()
		out := make([]json.RawMessage, len(elems))
		for i, e := range elems {
			raw, err := inlineRefs(e, root, budget, stack)
			if err != nil {
				return nil, err
			}
			out[i] = raw
		}
		return wire.Array(out...), nil
	}
	if !node.IsObject() {
		return node.Raw(), nil
	}
	ref, isRef := node.Get("$ref").Str()
	d := &wire.Dict{}
	if isRef {
		if slices.Contains(stack, ref) {
			return nil, errRef
		}
		if *budget--; *budget < 0 {
			return nil, errRef
		}
		target := resolveRef(root, ref)
		if !target.Exists() {
			return nil, errRef
		}
		inlined, err := inlineRefs(target, root, budget, append(slices.Clone(stack), ref))
		if err != nil {
			return nil, err
		}
		v, err := wire.ParseValue(inlined)
		if err != nil {
			return nil, err
		}
		d = wire.DictOf(v)
	}
	for _, f := range node.Fields() {
		if isRef && f.Key == "$ref" {
			continue
		}
		raw, err := inlineRefs(f.Value, root, budget, stack)
		if err != nil {
			return nil, err
		}
		d.Set(f.Key, raw)
	}
	return d.JSON(), nil
}

// Full JSON Schema for the parametersJsonSchema field, references inlined and an object root guaranteed
func prepareParameters(parameters wire.Value) json.RawMessage {
	if !parameters.IsObject() || len(parameters.Fields()) == 0 {
		return json.RawMessage(emptyObjectSchema)
	}
	schema := wire.DictOf(parameters)
	schema.Delete("$schema")
	root := schema.Value()
	budget := maxRefExpansions
	inlined, err := inlineRefs(root, root, &budget, nil)
	if err != nil {
		slog.Debug("gemini tool schema kept as written, its references could not be inlined")
		return root.Raw()
	}
	v, err := wire.ParseValue(inlined)
	if err != nil {
		return root.Raw()
	}
	schema = wire.DictOf(v)
	schema.Delete("$defs")
	schema.Delete("definitions")
	if schema.Len() == 0 {
		return json.RawMessage(emptyObjectSchema)
	}
	if schema.Get("type").Text() == "object" && !schema.Has("properties") {
		schema.Set("properties", json.RawMessage("{}"))
	}
	return schema.JSON()
}
