package openai

import (
	"encoding/json"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Object schema with no properties in the form Moonshot accepts
const emptyObjectSchema = `{"type":"object","properties":{},"required":[]}`

// Reports whether a schema keyword maps names to schemas
func schemaMapKey(key string) bool {
	switch key {
	case "properties", "patternProperties", "$defs", "definitions":
		return true
	}
	return false
}

// Reports whether a schema keyword holds a list of schemas
func schemaListKey(key string) bool {
	switch key {
	case "anyOf", "oneOf", "allOf", "prefixItems":
		return true
	}
	return false
}

// Reports whether a schema keyword holds one nested schema
func schemaNodeKey(key string) bool {
	switch key {
	case "items", "contains", "not", "additionalProperties", "propertyNames", "if", "then", "else":
		return true
	}
	return false
}

// Applies the Moonshot schema rules to a node and everything under it
func repairSchema(v wire.Value) json.RawMessage {
	if v.IsArray() {
		elems := v.Elems()
		out := make([]json.RawMessage, len(elems))
		for i, e := range elems {
			out[i] = repairSchema(e)
		}
		return wire.Array(out...)
	}
	if !v.IsObject() {
		return v.Raw()
	}
	d := &wire.Dict{}
	for _, f := range v.Fields() {
		switch {
		case schemaMapKey(f.Key) && f.Value.IsObject():
			sub := &wire.Dict{}
			for _, sf := range f.Value.Fields() {
				sub.Set(sf.Key, repairSchema(sf.Value))
			}
			d.Set(f.Key, sub.JSON())
		case (schemaListKey(f.Key) && f.Value.IsArray()) || (schemaNodeKey(f.Key) && f.Value.IsObject()):
			d.Set(f.Key, repairSchema(f.Value))
		default:
			d.Set(f.Key, f.Value.Raw())
		}
	}
	if anyOf := d.Get("anyOf"); anyOf.IsArray() {
		d.Delete("type")
		branches := anyOf.Elems()
		var nonNull []wire.Value
		for _, b := range branches {
			if b.IsObject() && b.Get("type").Text() != "null" {
				nonNull = append(nonNull, b)
			}
		}
		if len(nonNull) == 0 || len(nonNull) == len(branches) {
			return d.JSON()
		}
		if len(nonNull) > 1 {
			raws := make([]json.RawMessage, len(nonNull))
			for i, b := range nonNull {
				raws[i] = b.Raw()
			}
			return d.Set("anyOf", wire.Array(raws...)).JSON()
		}
		d.Delete("anyOf")
		for _, f := range nonNull[0].Fields() {
			d.Set(f.Key, f.Value.Raw())
		}
	}
	d.Delete("nullable")
	if !d.Has("$ref") {
		fillMissingType(d)
	}
	if enum := d.Get("enum"); enum.IsArray() {
		switch d.Get("type").Text() {
		case "string", "integer", "number", "boolean":
			var cleaned []json.RawMessage
			for _, e := range enum.Elems() {
				if s, isText := e.Str(); e.Kind() != wire.KindNull && !(isText && s == "") {
					cleaned = append(cleaned, e.Raw())
				}
			}
			if len(cleaned) > 0 {
				d.Set("enum", wire.Array(cleaned...))
			} else {
				d.Delete("enum")
			}
		}
	}
	if d.Get("type").Text() == "object" {
		ensureRequired(d)
	}
	return d.JSON()
}

// Gives an object schema a required list naming only declared properties
func ensureRequired(d *wire.Dict) {
	required := d.Get("required")
	if !required.IsArray() {
		d.Set("required", json.RawMessage("[]"))
		return
	}
	props := d.Get("properties")
	if !props.IsObject() {
		return
	}
	kept := []json.RawMessage{}
	for _, r := range required.Elems() {
		if name, ok := r.Str(); ok && props.Get(name).Exists() {
			kept = append(kept, r.Raw())
		}
	}
	d.Set("required", wire.Array(kept...))
}

// Infers the type of a schema node that names none, leaving a bare conditional untyped
func fillMissingType(d *wire.Dict) {
	typ := d.Get("type")
	if typ.IsArray() {
		concrete := "string"
		for _, t := range typ.Elems() {
			if s, ok := t.Str(); ok && s != "" && s != "null" {
				concrete = s
				break
			}
		}
		d.SetString("type", concrete)
		return
	}
	if s, isText := typ.Str(); typ.Exists() && typ.Kind() != wire.KindNull && !(isText && s == "") {
		return
	}
	inferred := "string"
	enum := d.Get("enum")
	switch {
	case d.Has("properties") || d.Has("required") || d.Has("additionalProperties"):
		inferred = "object"
	case d.Has("items") || d.Has("prefixItems"):
		inferred = "array"
	case enum.IsArray() && len(enum.Elems()) > 0:
		sample := enum.Elems()[0]
		switch sample.Kind() {
		case wire.KindBool:
			inferred = "boolean"
		case wire.KindNumber:
			inferred = "integer"
			if strings.ContainsAny(string(sample.Raw()), ".eE") {
				inferred = "number"
			}
		}
	case d.Has("if") || d.Has("then") || d.Has("else"):
		return
	}
	d.SetString("type", inferred)
}

// Tool parameter schema rewritten to the stricter JSON Schema subset Moonshot accepts
func MoonshotParameters(parameters json.RawMessage) json.RawMessage {
	v, err := wire.ParseValue(parameters)
	if err != nil || !v.IsObject() {
		return json.RawMessage(emptyObjectSchema)
	}
	repaired, err := wire.ParseValue(repairSchema(v))
	if err != nil || !repaired.IsObject() {
		return json.RawMessage(emptyObjectSchema)
	}
	d := wire.DictOf(repaired)
	d.SetString("type", "object")
	if !d.Has("properties") {
		d.Set("properties", json.RawMessage("{}"))
	}
	ensureRequired(d)
	return d.JSON()
}

// Reports whether a model id names a Kimi or Moonshot model under any aggregator prefix
func IsMoonshotModel(model string) bool {
	bare := strings.ToLower(strip(model))
	if bare == "" {
		return false
	}
	tail := bare[strings.LastIndex(bare, "/")+1:]
	if strings.HasPrefix(tail, "kimi-") || tail == "kimi" {
		return true
	}
	if tail == "k3" || strings.HasPrefix(tail, "k3.") || strings.HasPrefix(tail, "k3-") {
		return true
	}
	return strings.Contains(bare, "moonshot") || strings.Contains(bare, "/kimi") || strings.HasPrefix(bare, "kimi")
}
