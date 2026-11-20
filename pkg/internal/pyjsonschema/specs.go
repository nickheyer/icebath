package pyjsonschema

import (
	_ "embed"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

//go:embed metaschemas.json
var metaschemaText string

// Attribute access contents.get(key) on a dict, the AttributeError any other value raises
func pyGet(contents any, key string) any {
	d, ok := contents.(*pyjson.Object)
	if !ok {
		raise("AttributeError", "'"+typeName(contents)+"' object has no attribute 'get'")
	}
	return d.Vals[key]
}

// The in operator with a str on the left over a dict, list or str
func pyIn(key string, container any) bool {
	switch c := container.(type) {
	case *pyjson.Object:
		_, ok := c.Vals[key]
		return ok
	case []any:
		for _, item := range c {
			if pyEq(item, key) {
				return true
			}
		}
		return false
	case string:
		return strings.Contains(c, key)
	}
	raise("TypeError", "argument of type '"+typeName(container)+"' is not a container or iterable")
	return false
}

// Subscript contents[key] with a str key
func pySubscript(contents any, key string) any {
	switch c := contents.(type) {
	case *pyjson.Object:
		v, ok := c.Vals[key]
		if !ok {
			raise("KeyError", repr(key))
		}
		return v
	case []any:
		raise("TypeError", "list indices must be integers or slices, not str")
	case string:
		raise("TypeError", "string indices must be integers, not 'str'")
	}
	raise("TypeError", "'"+typeName(contents)+"' object is not subscriptable")
	return nil
}

// Items iteration of a value the way yield from walks it
func pyIter(v any) []any {
	switch c := v.(type) {
	case []any:
		return c
	case *pyjson.Object:
		out := make([]any, len(c.Keys))
		for i, k := range c.Keys {
			out[i] = k
		}
		return out
	case string:
		rs := pystr.Runes(c)
		out := make([]any, len(rs))
		for i := range rs {
			out[i] = pystr.FromRunes(rs[i : i+1])
		}
		return out
	}
	raise("TypeError", "'"+typeName(v)+"' object is not iterable")
	return nil
}

// dict.values of a value, the AttributeError a non dict raises
func pyValues(v any) []any {
	d, ok := v.(*pyjson.Object)
	if !ok {
		raise("AttributeError", "'"+typeName(v)+"' object has no attribute 'values'")
	}
	out := make([]any, len(d.Keys))
	for i, k := range d.Keys {
		out[i] = d.Vals[k]
	}
	return out
}

type keywordSets struct {
	inValue, inSubarray, inSubvalues []string
}

// Subschema walk of _subresources_of and its crazy items, dependencies and additional variants
func subresourcesOf(sets keywordSets, boolNone, crazyItems, crazyDeps, crazyAP bool) func(any) []any {
	return func(contents any) []any {
		if _, ok := contents.(bool); ok && boolNone {
			return nil
		}
		var out []any
		for _, each := range sets.inValue {
			if pyIn(each, contents) {
				out = append(out, pySubscript(contents, each))
			}
		}
		for _, each := range sets.inSubarray {
			if pyIn(each, contents) {
				out = append(out, pyIter(pySubscript(contents, each))...)
			}
		}
		for _, each := range sets.inSubvalues {
			if pyIn(each, contents) {
				out = append(out, pyValues(pySubscript(contents, each))...)
			}
		}
		if crazyItems {
			if items := pyGet(contents, "items"); items != nil {
				switch items.(type) {
				case []any, string:
					out = append(out, pyIter(items)...)
				default:
					out = append(out, items)
				}
			}
		}
		if crazyDeps {
			if deps := pyGet(contents, "dependencies"); deps != nil {
				values := pyValues(deps)
				if len(values) > 0 && isDict(values[0]) {
					out = append(out, values...)
				}
			}
		}
		if crazyAP {
			for _, each := range []string{"additionalItems", "additionalProperties"} {
				if v := pyGet(contents, each); isDict(v) {
					out = append(out, v)
				}
			}
		}
		return out
	}
}

// Resolver change of _maybe_in_subresource while a pointer walks into keyword values
func maybeInSubresource(sets keywordSets, crazy []string) func([]any, *resolver, *resource) *resolver {
	inChild := append(append([]string(nil), sets.inSubvalues...), sets.inSubarray...)
	return func(segments []any, r *resolver, sub *resource) *resolver {
		for i := 0; i < len(segments); i++ {
			s, isStr := segments[i].(string)
			if isStr && slices.Contains(crazy, s) && isDict(sub.contents) {
				return r.inSubresource(sub)
			}
			if !isStr || !slices.Contains(sets.inValue, s) {
				if !isStr || !slices.Contains(inChild, s) {
					return r
				}
				i++
				if i >= len(segments) {
					return r
				}
			}
		}
		return r.inSubresource(sub)
	}
}

func dollarID(contents any) any {
	if _, ok := contents.(bool); ok {
		return nil
	}
	return pyGet(contents, "$id")
}

func legacyDollarID(contents any) any {
	if _, ok := contents.(bool); ok {
		return nil
	}
	if pyIn("$ref", contents) {
		return nil
	}
	return idNotFragment(pyGet(contents, "$id"))
}

func legacyID(contents any) any {
	if pyIn("$ref", contents) {
		return nil
	}
	return idNotFragment(pyGet(contents, "id"))
}

func idNotFragment(id any) any {
	if id == nil {
		return nil
	}
	if s, ok := id.(string); !ok {
		raise("AttributeError", "'"+typeName(id)+"' object has no attribute 'startswith'")
	} else if strings.HasPrefix(s, "#") {
		return nil
	}
	return id
}

func anchors2020(spec *specification, contents any) []*anchor {
	if _, ok := contents.(bool); ok {
		return nil
	}
	var out []*anchor
	if name := pyGet(contents, "$anchor"); name != nil {
		out = append(out, &anchor{name: name, resource: spec.createResource(contents)})
	}
	if name := pyGet(contents, "$dynamicAnchor"); name != nil {
		out = append(out, &anchor{name: name, dynamic: true, resource: spec.createResource(contents)})
	}
	return out
}

func anchors2019(spec *specification, contents any) []*anchor {
	if _, ok := contents.(bool); ok {
		return nil
	}
	if name := pyGet(contents, "$anchor"); name != nil {
		return []*anchor{{name: name, resource: spec.createResource(contents)}}
	}
	return nil
}

// Anchor named by a fragment only $id or id the way legacy drafts spell plain name anchors
func legacyAnchor(key string, boolNone bool) func(*specification, any) []*anchor {
	return func(spec *specification, contents any) []*anchor {
		if _, ok := contents.(bool); ok && boolNone {
			return nil
		}
		id := pyGet(contents, key)
		if id == nil {
			id = ""
		}
		s, ok := id.(string)
		if !ok {
			raise("AttributeError", "'"+typeName(id)+"' object has no attribute 'startswith'")
		}
		if !strings.HasPrefix(s, "#") {
			return nil
		}
		return []*anchor{{name: s[1:], resource: spec.createResource(contents)}}
	}
}

var (
	sets2020 = keywordSets{
		inValue:     []string{"additionalProperties", "contains", "contentSchema", "else", "if", "items", "not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties"},
		inSubarray:  []string{"allOf", "anyOf", "oneOf", "prefixItems"},
		inSubvalues: []string{"$defs", "definitions", "dependentSchemas", "patternProperties", "properties"},
	}
	sets2019 = keywordSets{
		inValue:     []string{"additionalItems", "additionalProperties", "contains", "contentSchema", "else", "if", "not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties"},
		inSubarray:  []string{"allOf", "anyOf", "oneOf"},
		inSubvalues: []string{"$defs", "definitions", "dependentSchemas", "patternProperties", "properties"},
	}
	sets7 = keywordSets{
		inValue:     []string{"additionalItems", "additionalProperties", "contains", "else", "if", "not", "propertyNames", "then"},
		inSubarray:  []string{"allOf", "anyOf", "oneOf"},
		inSubvalues: []string{"definitions", "patternProperties", "properties"},
	}
	sets6 = keywordSets{
		inValue:     []string{"additionalItems", "additionalProperties", "contains", "not", "propertyNames"},
		inSubarray:  []string{"allOf", "anyOf", "oneOf"},
		inSubvalues: []string{"definitions", "patternProperties", "properties"},
	}
	sets4Sub = keywordSets{
		inValue:     []string{"not"},
		inSubarray:  []string{"allOf", "anyOf", "oneOf"},
		inSubvalues: []string{"definitions", "patternProperties", "properties"},
	}
	sets4Maybe = keywordSets{
		inValue:     []string{"additionalItems", "additionalProperties", "not"},
		inSubarray:  []string{"allOf", "anyOf", "oneOf"},
		inSubvalues: []string{"definitions", "patternProperties", "properties"},
	}
	sets3Sub = keywordSets{
		inSubarray:  []string{"extends"},
		inSubvalues: []string{"definitions", "patternProperties", "properties"},
	}
	sets3Maybe = keywordSets{
		inValue:     []string{"additionalItems", "additionalProperties"},
		inSubarray:  []string{"extends"},
		inSubvalues: []string{"definitions", "patternProperties", "properties"},
	}
)

var (
	spec202012 = &specification{name: "draft2020-12", idOf: dollarID,
		subresourcesOf: subresourcesOf(sets2020, true, false, false, false), anchorsIn: anchors2020,
		maybeInSubresource: maybeInSubresource(sets2020, nil)}
	spec201909 = &specification{name: "draft2019-09", idOf: dollarID,
		subresourcesOf: subresourcesOf(sets2019, true, true, false, false), anchorsIn: anchors2019,
		maybeInSubresource: maybeInSubresource(sets2019, []string{"items"})}
	spec7 = &specification{name: "draft-07", idOf: legacyDollarID,
		subresourcesOf: subresourcesOf(sets7, true, true, true, false), anchorsIn: legacyAnchor("$id", true),
		maybeInSubresource: maybeInSubresource(sets7, []string{"items", "dependencies"})}
	spec6 = &specification{name: "draft-06", idOf: legacyDollarID,
		subresourcesOf: subresourcesOf(sets6, true, true, true, false), anchorsIn: legacyAnchor("$id", true),
		maybeInSubresource: maybeInSubresource(sets6, []string{"items", "dependencies"})}
	spec4 = &specification{name: "draft-04", idOf: legacyID,
		subresourcesOf: subresourcesOf(sets4Sub, false, true, true, true), anchorsIn: legacyAnchor("id", false),
		maybeInSubresource: maybeInSubresource(sets4Maybe, []string{"items", "dependencies"})}
	spec3 = &specification{name: "draft-03", idOf: legacyID,
		subresourcesOf: subresourcesOf(sets3Sub, false, true, true, true), anchorsIn: legacyAnchor("id", false),
		maybeInSubresource: maybeInSubresource(sets3Maybe, []string{"items", "dependencies"})}
	specOpaque = &specification{name: "opaque", idOf: func(any) any { return nil },
		subresourcesOf: func(any) []any { return nil }, anchorsIn: func(*specification, any) []*anchor { return nil },
		maybeInSubresource: func(_ []any, r *resolver, _ *resource) *resolver { return r }}
)

func specsByDialect() map[string]*specification {
	return map[string]*specification{
		"https://json-schema.org/draft/2020-12/schema": spec202012,
		"https://json-schema.org/draft/2019-09/schema": spec201909,
		"http://json-schema.org/draft-07/schema":       spec7,
		"http://json-schema.org/draft-06/schema":       spec6,
		"http://json-schema.org/draft-04/schema":       spec4,
		"http://json-schema.org/draft-03/schema":       spec3,
	}
}

// Metaschema documents by id and the crawled registry jsonschema_specifications ships
var specifications = sync.OnceValues(func() (map[string]*pyjson.Object, *registry) {
	parsed, err := pyjson.Loads(metaschemaText)
	if err != nil {
		panic("pyjsonschema: metaschema bundle: " + err.Error())
	}
	bundle := parsed.(*pyjson.Object)
	docs := make(map[string]*pyjson.Object, len(bundle.Keys))
	g := &registry{resources: map[string]*resource{}, anchors: map[anchorKey]*anchor{}}
	var pairs []uriResource
	for _, uri := range bundle.Keys {
		doc := bundle.Vals[uri].(*pyjson.Object)
		docs[uri] = doc
		dialect := doc.Vals["$schema"].(string)
		spec := specsByDialect()[strings.TrimRight(dialect, "#")]
		res := spec.createResource(doc)
		id, _ := res.id(nil)
		pairs = append(pairs, uriResource{id, res})
	}
	return docs, g.withResources(pairs...).crawl(nil)
})
