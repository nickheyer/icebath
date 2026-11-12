package pydantic

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// JSON schema generator in serialization mode sharing one definitions table across models
type SchemaGenerator struct {
	refTemplate string
	defs        map[string]*pyjson.Object
	owners      map[string]any
}

// Generator whose references follow the template, the {model} slot naming the definition
func NewSchemaGenerator(refTemplate string) *SchemaGenerator {
	return &SchemaGenerator{refTemplate: refTemplate, defs: map[string]*pyjson.Object{}, owners: map[string]any{}}
}

var defNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Reference schema of the model, its definition and every definition it reaches recorded
func (g *SchemaGenerator) ModelRef(m *Model) *pyjson.Object {
	return g.define(m.Name, m, func() *pyjson.Object { return g.modelSchema(m) })
}

// Every recorded definition in name order with each schema sorted the way pydantic sorts one
func (g *SchemaGenerator) Definitions() *pyjson.Object {
	names := make([]string, 0, len(g.defs))
	for name := range g.defs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := pyjson.NewObject()
	for _, name := range names {
		out.Set(name, SortSchema(g.defs[name], ""))
	}
	return out
}

// Definition recorded under the class name, refusing two classes that share one
func (g *SchemaGenerator) define(class string, owner any, build func() *pyjson.Object) *pyjson.Object {
	name := defNameUnsafe.ReplaceAllString(class, "_")
	ref := pyjson.ObjectOf("$ref", strings.ReplaceAll(g.refTemplate, "{model}", name))
	if prev, seen := g.owners[name]; seen {
		if prev != owner {
			panic(fmt.Sprintf("two classes share the JSON schema definition name %s", name))
		}
		return ref
	}
	g.owners[name] = owner
	g.defs[name] = pyjson.NewObject()
	g.defs[name] = build()
	return ref
}

// Definition of a model, a RootModel taking the schema of its root annotation
func (g *SchemaGenerator) modelSchema(m *Model) *pyjson.Object {
	var s *pyjson.Object
	if m.Root != nil {
		s = g.TypeSchema(m.Root).Copy()
	} else {
		props := pyjson.NewObject()
		var required []any
		for _, f := range m.Fields {
			if f.exclude {
				continue
			}
			name := f.Name
			if f.alias != "" {
				name = f.alias
			}
			fs := g.TypeSchema(f.Type).Copy()
			if f.required {
				required = append(required, name)
			} else if f.factory == nil {
				fs.Set("default", dumpValue(f.defaultValue(), DumpOptions{ByAlias: true}))
			}
			if _, titled := fs.Get("title"); !titled && titleShouldBeSet(f.Type) {
				fs.Set("title", pystr.Strip(strings.ReplaceAll(pystr.Title(name), "_", " ")))
			}
			props.Set(name, g.refOverrides(fs))
		}
		s = pyjson.ObjectOf("type", "object", "properties", props)
		if required != nil {
			s.Set("required", required)
		}
		switch m.Config.Extra {
		case ExtraAllow:
			s.Set("additionalProperties", true)
		case ExtraForbid:
			s.Set("additionalProperties", false)
		}
	}
	if _, ok := s.Get("title"); !ok {
		s.Set("title", m.Name)
	}
	if _, ok := s.Get("description"); !ok && m.Doc != "" {
		s.Set("description", m.Doc)
	}
	return s
}

// Field schema without the keys a referenced definition already holds with the same value
func (g *SchemaGenerator) refOverrides(fs *pyjson.Object) *pyjson.Object {
	ref, ok := fs.Get("$ref")
	if !ok {
		return fs
	}
	name := ref.(string)[strings.LastIndex(ref.(string), "/")+1:]
	target, ok := g.defs[name]
	if !ok {
		return fs
	}
	for _, k := range append([]string(nil), fs.Keys...) {
		if v, has := target.Get(k); k != "$ref" && has && pyjson.Equal(v, fs.Vals[k]) {
			fs.Delete(k)
		}
	}
	return fs
}

// A field gets a title unless its annotation is a referenced model or enum, seen through Optional
func titleShouldBeSet(t Type) bool {
	switch x := t.(type) {
	case *Model, *EnumType:
		return false
	case nullableType:
		return titleShouldBeSet(x.inner)
	}
	return true
}

// JSON schema of one annotation, models and enums as references to their definitions
func (g *SchemaGenerator) TypeSchema(t Type) *pyjson.Object {
	switch x := t.(type) {
	case *strType:
		s := pyjson.ObjectOf("type", "string")
		if x.min >= 0 {
			s.Set("minLength", int64(x.min))
		}
		if x.max >= 0 {
			s.Set("maxLength", int64(x.max))
		}
		if x.pattern != nil {
			s.Set("pattern", x.patternText)
		}
		return s
	case intType:
		return pyjson.ObjectOf("type", "integer")
	case floatType:
		return pyjson.ObjectOf("type", "number")
	case boolType:
		return pyjson.ObjectOf("type", "boolean")
	case anyType:
		return pyjson.NewObject()
	case noneType:
		return pyjson.ObjectOf("type", "null")
	case nullableType:
		inner, null := g.TypeSchema(x.inner), pyjson.ObjectOf("type", "null")
		if pyjson.Equal(inner, null) {
			return null
		}
		return flattenedAnyOf([]*pyjson.Object{inner, null})
	case listType:
		s := pyjson.ObjectOf("type", "array", "items", g.TypeSchema(x.item))
		if x.min >= 0 {
			s.Set("minItems", int64(x.min))
		}
		if x.max >= 0 {
			s.Set("maxItems", int64(x.max))
		}
		return s
	case dictType:
		s := pyjson.ObjectOf("type", "object")
		values := g.TypeSchema(x.value).Copy()
		values.Delete("title")
		if values.Len() > 0 {
			s.Set("additionalProperties", values)
		} else {
			s.Set("additionalProperties", true)
		}
		return s
	case literalType:
		return literalSchema(x.values)
	case *EnumType:
		return g.define(x.Name, x, func() *pyjson.Object {
			s := pyjson.ObjectOf("title", x.Name)
			if x.Doc != "" {
				s.Set("description", x.Doc)
			}
			values := make([]any, len(x.values))
			for i, v := range x.values {
				values[i] = v
			}
			s.Set("enum", values)
			s.Set("type", "string")
			return s
		})
	case unionType:
		choices := make([]*pyjson.Object, len(x.choices))
		for i, c := range x.choices {
			choices[i] = g.TypeSchema(c)
		}
		if len(choices) == 1 {
			return choices[0]
		}
		return flattenedAnyOf(choices)
	case taggedType:
		choices := make([]*pyjson.Object, len(x.models))
		mapping := pyjson.NewObject()
		for i, m := range x.models {
			choices[i] = g.ModelRef(m)
			ref, _ := choices[i].Get("$ref")
			mapping.Set(x.tags[i], ref)
		}
		oneOf := []any{}
		for _, c := range dedupSchemas(choices) {
			oneOf = append(oneOf, c)
		}
		return pyjson.ObjectOf("oneOf", oneOf, "discriminator", pyjson.ObjectOf("propertyName", x.discriminator, "mapping", mapping))
	case *Model:
		return g.ModelRef(x)
	}
	panic(fmt.Sprintf("no JSON schema for annotation %s", t.Label()))
}

// Literal schema, const for one value and enum for several, typed when every value shares a JSON type
func literalSchema(values []any) *pyjson.Object {
	s := pyjson.NewObject()
	if len(values) == 1 {
		s.Set("const", values[0])
	} else {
		s.Set("enum", append([]any(nil), values...))
	}
	kinds := map[string]bool{}
	for _, v := range values {
		switch v.(type) {
		case string:
			kinds["string"] = true
		case bool:
			kinds["boolean"] = true
		case int64, int, *big.Int:
			kinds["integer"] = true
		case float64:
			kinds["number"] = true
		case nil:
			kinds["null"] = true
		default:
			kinds["other"] = true
		}
	}
	if len(kinds) == 1 && !kinds["other"] {
		for k := range kinds {
			s.Set("type", k)
		}
	}
	return s
}

// anyOf of the members with nested lone anyOf lists spliced in and duplicates dropped
func flattenedAnyOf(schemas []*pyjson.Object) *pyjson.Object {
	var members []*pyjson.Object
	for _, s := range schemas {
		if nested, ok := s.Get("anyOf"); ok && s.Len() == 1 {
			for _, n := range nested.([]any) {
				members = append(members, n.(*pyjson.Object))
			}
			continue
		}
		members = append(members, s)
	}
	members = dedupSchemas(members)
	if len(members) == 1 {
		return members[0]
	}
	out := make([]any, len(members))
	for i, m := range members {
		out[i] = m
	}
	return pyjson.ObjectOf("anyOf", out)
}

// Schemas with later duplicates removed, equal meaning the same keys and values in any order
func dedupSchemas(schemas []*pyjson.Object) []*pyjson.Object {
	seen := map[string]bool{}
	var out []*pyjson.Object
	for _, s := range schemas {
		key, err := pyjson.Dumps(s, pyjson.SortKeys(true))
		if err != nil {
			panic(err)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

// Copy of a schema with keys sorted at every level except directly under properties and default
func SortSchema(v any, parentKey string) any {
	switch x := v.(type) {
	case *pyjson.Object:
		keys := append([]string(nil), x.Keys...)
		if parentKey != "properties" && parentKey != "default" {
			sort.Strings(keys)
		}
		out := pyjson.NewObject()
		for _, k := range keys {
			out.Set(k, SortSchema(x.Vals[k], k))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = SortSchema(item, parentKey)
		}
		return out
	}
	return v
}
