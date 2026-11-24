package jsonschemautil

import (
	"slices"
	"strconv"

	"github.com/dlclark/regexp2/v2"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Object member a keyword names with the schema it applies there
type namedSchema struct {
	name string
	node *node
}

// properties: each named member present passes its schema; in draft 3 a member whose schema says "required": true must be present
func (c *compiler) properties(l loc, value any, draft3 bool) (checkFn, error) {
	o, ok := value.(*jsonutil.Object)
	if !ok {
		return nil, c.bad(l, "/properties", "properties must be an object of schemas")
	}
	props := make([]namedSchema, 0, o.Len())
	var required []string
	for name, sv := range o.All() {
		n, err := c.sub(sv, l, "/properties/"+escapeToken(name))
		if err != nil {
			return nil, err
		}
		props = append(props, namedSchema{name: name, node: n})
		if so, ok := sv.(*jsonutil.Object); ok && draft3 {
			if on, _ := so.Value("required").(bool); on {
				required = append(required, name)
			}
		}
	}
	return func(st *state, v any, ev *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok {
			return true
		}
		valid := true
		for i := range props {
			p := &props[i]
			val, has := obj.get(p.name)
			if !has {
				continue
			}
			if ev != nil {
				ev.prop(p.name)
			}
			st.push(p.name)
			ok := st.member(p.node, val, step{key: p.name, index: -1})
			st.pop()
			if !ok {
				valid = false
				if st.quick {
					return false
				}
			}
		}
		for _, name := range required {
			if _, has := obj.get(name); has {
				continue
			}
			if st.quick {
				return false
			}
			valid = false
			st.push(name)
			st.push("required")
			st.fail("required", quote(name, schemaQuoteLimit)+" is a required property", v, nil)
			st.pop()
			st.pop()
		}
		return valid
	}, nil
}

// Pattern of patternProperties with the schema members matching it pass
type patternSchema struct {
	pattern string
	re      *regexp2.Regexp
	node    *node
}

// patternProperties: each member whose name matches a pattern passes that pattern's schema
func (c *compiler) patternProperties(l loc, value any) (checkFn, error) {
	o, ok := value.(*jsonutil.Object)
	if !ok {
		return nil, c.bad(l, "/patternProperties", "patternProperties must be an object of schemas")
	}
	list := make([]patternSchema, 0, o.Len())
	for p, sv := range o.All() {
		rel := "/patternProperties/" + escapeToken(p)
		re, err := c.regex(l, rel, p)
		if err != nil {
			return nil, err
		}
		n, err := c.sub(sv, l, rel)
		if err != nil {
			return nil, err
		}
		list = append(list, patternSchema{pattern: p, re: re, node: n})
	}
	return func(st *state, v any, ev *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok || obj.len() == 0 {
			return true
		}
		keys := obj.keys()
		valid := true
		for i := range list {
			ps := &list[i]
			for _, k := range keys {
				if !st.matches(ps.re, "patternProperties", k) {
					if st.halt != nil {
						return false
					}
					continue
				}
				if ev != nil {
					ev.prop(k)
				}
				val, _ := obj.get(k)
				st.push(ps.pattern)
				ok := st.member(ps.node, val, step{key: k, index: -1})
				st.pop()
				if !ok {
					valid = false
					if st.quick {
						return false
					}
				}
			}
		}
		return valid
	}, nil
}

// additionalProperties: the schema applied to every member neither properties names nor patternProperties matches
func (c *compiler) additionalProperties(l loc, o *jsonutil.Object, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/additionalProperties")
	if err != nil {
		return nil, err
	}
	named := map[string]struct{}{}
	if props, ok := o.Value("properties").(*jsonutil.Object); ok {
		for _, k := range props.Keys() {
			named[k] = struct{}{}
		}
	}
	var patterns []*regexp2.Regexp
	var patternNames []string
	if pp, ok := o.Value("patternProperties").(*jsonutil.Object); ok {
		for _, p := range pp.Keys() {
			re, err := c.regex(l, "/patternProperties/"+escapeToken(p), p)
			if err != nil {
				return nil, err
			}
			patterns = append(patterns, re)
			patternNames = append(patternNames, p)
		}
	}
	suffix := ""
	if len(patternNames) > 0 {
		sorted := slices.Sorted(slices.Values(patternNames))
		suffix = ", matching none of the patterns " + quoteList(sorted, "")
	}
	return func(st *state, v any, ev *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok || n.boolean == boolTrue && ev == nil {
			return true
		}
		valid := true
		var extras []string
		for k, val := range obj.all() {
			if _, ok := named[k]; ok {
				continue
			}
			matched := false
			for _, re := range patterns {
				if st.matches(re, "additionalProperties", k) {
					matched = true
					break
				}
			}
			if st.halt != nil {
				return false
			}
			if matched {
				continue
			}
			if ev != nil {
				ev.prop(k)
			}
			switch n.boolean {
			case boolTrue:
			case boolFalse:
				if st.quick {
					return false
				}
				extras = append(extras, k)
			default:
				if !st.member(n, val, step{key: k, index: -1}) {
					valid = false
					if st.quick {
						return false
					}
				}
			}
		}
		if len(extras) > 0 {
			slices.Sort(extras)
			st.fail("additionalProperties", "additional properties are not allowed ("+nameList(extras)+" "+wasWere(len(extras))+
				" unexpected"+suffix+")", v, nil)
			return false
		}
		return valid
	}, nil
}

// propertyNames: every member name, as a string, passes the schema
func (c *compiler) propertyNames(l loc, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/propertyNames")
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, _ *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok || n.boolean == boolTrue {
			return true
		}
		valid := true
		for k := range obj.all() {
			if !st.derived(n, k) {
				valid = false
				if st.quick {
					return false
				}
			}
		}
		return valid
	}, nil
}

// required: every listed member is present
func (c *compiler) required(l loc, value any) (checkFn, error) {
	names, ok := stringList(value)
	if !ok {
		return nil, c.bad(l, "/required", "required must be an array of strings")
	}
	if len(names) == 0 {
		return nil, nil
	}
	return func(st *state, v any, _ *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok {
			return true
		}
		valid := true
		for _, name := range names {
			if _, has := obj.get(name); has {
				continue
			}
			if st.quick {
				return false
			}
			valid = false
			st.fail("required", quote(name, schemaQuoteLimit)+" is a required property", v, nil)
		}
		return valid
	}, nil
}

// Strings of an array of strings, false for anything else
func stringList(value any) ([]string, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// minProperties and maxProperties
func (c *compiler) propertyCount(l loc, key string, value any) (checkFn, error) {
	limit, ok := nonNegative(value)
	if !ok {
		return nil, c.bad(l, "/"+key, "%s must be a non-negative integer", key)
	}
	lower := key == "minProperties"
	return func(st *state, v any, _ *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok {
			return true
		}
		n := obj.len()
		if lower && n >= limit || !lower && n <= limit {
			return true
		}
		if !st.quick {
			st.fail(key, countMessage(v, n, limit, lower, "property", "properties"), v, nil)
		}
		return false
	}, nil
}

// Rule of dependencies, dependentRequired or dependentSchemas for one member: when it is present, the names must be too and the schema must pass
type dependency struct {
	prop  string
	names []string
	node  *node
}

// The check of a list of dependencies under key
func dependencyCheck(key string, deps []dependency) checkFn {
	return func(st *state, v any, ev *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok {
			return true
		}
		valid := true
		for i := range deps {
			dep := &deps[i]
			if _, has := obj.get(dep.prop); !has {
				continue
			}
			st.push(dep.prop)
			for _, name := range dep.names {
				if _, has := obj.get(name); has {
					continue
				}
				if st.quick {
					st.pop()
					return false
				}
				valid = false
				st.fail(key, quote(name, schemaQuoteLimit)+" is a required property when "+quote(dep.prop, schemaQuoteLimit)+" is present", v, nil)
			}
			if dep.node != nil && !st.inPlace(dep.node, v, ev) {
				valid = false
				if st.quick {
					st.pop()
					return false
				}
			}
			st.pop()
		}
		return valid
	}
}

// dependencies of drafts 3 to 7: per member, an array of names, a schema, or in draft 3 a single name
func (c *compiler) dependencies(l loc, value any, draft3 bool) (checkFn, error) {
	o, ok := value.(*jsonutil.Object)
	if !ok {
		return nil, c.bad(l, "/dependencies", "dependencies must be an object")
	}
	deps := make([]dependency, 0, o.Len())
	for prop, dv := range o.All() {
		dep := dependency{prop: prop}
		switch x := dv.(type) {
		case []any:
			names, ok := stringList(x)
			if !ok {
				return nil, c.bad(l, "/dependencies/"+escapeToken(prop), "a dependency list must hold strings")
			}
			dep.names = names
		case string:
			if !draft3 {
				return nil, c.bad(l, "/dependencies/"+escapeToken(prop), "a dependency must be an array of strings or a schema")
			}
			dep.names = []string{x}
		default:
			n, err := c.sub(dv, l, "/dependencies/"+escapeToken(prop))
			if err != nil {
				return nil, err
			}
			dep.node = n
		}
		deps = append(deps, dep)
	}
	return dependencyCheck("dependencies", deps), nil
}

// dependentRequired: per member, names that must be present beside it
func (c *compiler) dependentRequired(l loc, value any) (checkFn, error) {
	o, ok := value.(*jsonutil.Object)
	if !ok {
		return nil, c.bad(l, "/dependentRequired", "dependentRequired must be an object of string arrays")
	}
	deps := make([]dependency, 0, o.Len())
	for prop, dv := range o.All() {
		names, ok := stringList(dv)
		if !ok {
			return nil, c.bad(l, "/dependentRequired/"+escapeToken(prop), "a dependentRequired entry must be an array of strings")
		}
		deps = append(deps, dependency{prop: prop, names: names})
	}
	return dependencyCheck("dependentRequired", deps), nil
}

// dependentSchemas: per member, a schema the whole object must pass when the member is present
func (c *compiler) dependentSchemas(l loc, value any) (checkFn, error) {
	o, ok := value.(*jsonutil.Object)
	if !ok {
		return nil, c.bad(l, "/dependentSchemas", "dependentSchemas must be an object of schemas")
	}
	deps := make([]dependency, 0, o.Len())
	for prop, sv := range o.All() {
		n, err := c.sub(sv, l, "/dependentSchemas/"+escapeToken(prop))
		if err != nil {
			return nil, err
		}
		deps = append(deps, dependency{prop: prop, node: n})
	}
	return dependencyCheck("dependentSchemas", deps), nil
}

// unevaluatedProperties: the schema applied to the members no other keyword of the schema or of the subschemas that passed evaluated
func (c *compiler) unevaluatedProperties(l loc, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/unevaluatedProperties")
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		obj, ok := objectOf(v)
		if !ok {
			return true
		}
		if ev == nil {
			ev = &evaluated{}
		}
		if ev.allProps {
			return true
		}
		start := len(st.errs)
		var extras, invalid []string
		for k, val := range obj.all() {
			if ev.hasProp(k) {
				continue
			}
			switch n.boolean {
			case boolTrue:
			case boolFalse:
				if st.quick {
					return false
				}
				extras = append(extras, k)
			default:
				if !st.member(n, val, step{key: k, index: -1}) {
					if st.quick {
						return false
					}
					invalid = append(invalid, k)
				}
			}
		}
		ev.allProps = true
		switch {
		case len(extras) > 0:
			slices.Sort(extras)
			st.fail("unevaluatedProperties", "unevaluated properties are not allowed ("+nameList(extras)+" "+wasWere(len(extras))+" unexpected)", v, nil)
			return false
		case len(invalid) > 0:
			causes := st.takeErrors(start)
			st.fail("unevaluatedProperties", "unevaluated properties are not valid under the given schema ("+nameList(invalid)+" "+
				wasWere(len(invalid))+" unevaluated and invalid)", v, causes)
			return false
		}
		return true
	}, nil
}

// disallow of draft 3: the value has none of the listed types and passes none of the listed schemas
func (c *compiler) disallow(l loc, value any) (checkFn, error) {
	list, isList := value.([]any)
	if !isList {
		list = []any{value}
	}
	type banned struct {
		name string
		bit  typeSet
		node *node
	}
	var all []banned
	for i, item := range list {
		if s, ok := item.(string); ok {
			bit, known := typeBit(s, l.dialect)
			if !known {
				return nil, c.bad(l, "/disallow", "unknown type %s", quote(s, schemaQuoteLimit))
			}
			all = append(all, banned{name: s, bit: bit})
			continue
		}
		rel := "/disallow"
		if isList {
			rel += "/" + strconv.Itoa(i)
		}
		n, err := c.sub(item, l, rel)
		if err != nil {
			return nil, err
		}
		all = append(all, banned{node: n})
	}
	return func(st *state, v any, _ *evaluated) bool {
		valid := true
		for _, b := range all {
			if b.node == nil && !b.bit.match(v) || b.node != nil && !st.test(b.node, v, nil) {
				continue
			}
			if st.quick {
				return false
			}
			valid = false
			if b.node == nil {
				st.fail("disallow", "type "+quote(b.name, schemaQuoteLimit)+" is disallowed for "+quoteValue(v), v, nil)
			} else {
				st.fail("disallow", quoteValue(v)+" matches a disallowed schema", v, nil)
			}
		}
		return valid
	}, nil
}

// extends of draft 3: a schema, or each of an array of schemas, applied to the same value
func (c *compiler) extends(l loc, value any) (checkFn, error) {
	if _, ok := value.([]any); ok {
		return c.allOf(l, "extends", value)
	}
	n, err := c.sub(value, l, "/extends")
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool { return st.inPlace(n, v, ev) }, nil
}
