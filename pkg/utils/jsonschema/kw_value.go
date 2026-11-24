package jsonschemautil

import (
	"strconv"
	"unicode/utf8"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Set of JSON types a "type" keyword allows
type typeSet uint8

const (
	typeNull typeSet = 1 << iota
	typeBoolean
	typeObject
	typeArray
	typeNumber
	typeInteger
	typeString
	typeAny
)

// Type a name stands for in the dialect, false for a name it does not know
func typeBit(name string, d *dialect) (typeSet, bool) {
	switch name {
	case "null":
		return typeNull, true
	case "boolean":
		return typeBoolean, true
	case "object":
		return typeObject, true
	case "array":
		return typeArray, true
	case "number":
		return typeNumber, true
	case "integer":
		return typeInteger, true
	case "string":
		return typeString, true
	case "any":
		return typeAny, d.anyType
	}
	return 0, false
}

// Whether a value has one of the types, an integer being any number without a fractional part
func (t typeSet) match(v any) bool {
	if t&typeAny != 0 {
		return true
	}
	switch x := v.(type) {
	case nil:
		return t&typeNull != 0
	case bool:
		return t&typeBoolean != 0
	case string:
		return t&typeString != 0
	case []any:
		return t&typeArray != 0
	case map[string]any:
		return t&typeObject != 0
	case *jsonutil.Object:
		if x == nil {
			return t&typeNull != 0
		}
		return t&typeObject != 0
	}
	n, ok := numberOf(v)
	if !ok {
		return false
	}
	return t&typeNumber != 0 || t&typeInteger != 0 && n.isInteger()
}

// type: the value has one of the named types; in draft 3 the list may also hold schemas, one of which passing is enough
func (c *compiler) typeCheck(n *node, l loc, value any) (checkFn, error) {
	var names, labels []string
	var set typeSet
	var schemas []*node
	add := func(name string) error {
		bit, ok := typeBit(name, l.dialect)
		if !ok {
			return c.bad(l, "/type", "unknown type %s", quote(name, schemaQuoteLimit))
		}
		set |= bit
		names = append(names, name)
		return nil
	}
	switch x := value.(type) {
	case string:
		if err := add(x); err != nil {
			return nil, err
		}
	case []any:
		for i, item := range x {
			if s, ok := item.(string); ok {
				if err := add(s); err != nil {
					return nil, err
				}
				continue
			}
			if l.dialect.draft != Draft3 {
				return nil, c.bad(l, "/type", "type must be a string or an array of strings")
			}
			label := strconv.Itoa(i)
			sn, err := c.sub(item, l, "/type/"+label)
			if err != nil {
				return nil, err
			}
			schemas = append(schemas, sn)
			labels = append(labels, label)
		}
	default:
		return nil, c.bad(l, "/type", "type must be a string or an array of strings")
	}
	n.types, n.hasTypes = set, true
	expected := quoteList(names, "or")
	if len(schemas) == 0 {
		return func(st *state, v any, _ *evaluated) bool {
			if set.match(v) {
				return true
			}
			if !st.quick {
				st.fail("type", quoteValue(v)+" is not of type "+expected, v, nil)
			}
			return false
		}, nil
	}
	head := " matches none of the schemas in type"
	if len(names) > 0 {
		head = " is not of type " + expected + " and matches none of the schemas in type"
	}
	return func(st *state, v any, ev *evaluated) bool {
		if set.match(v) {
			return true
		}
		start := len(st.errs)
		var ends []int
		for i, sn := range schemas {
			st.push(labels[i])
			ok := st.inPlace(sn, v, ev)
			st.pop()
			if ok {
				st.drop(start)
				return true
			}
			ends = append(ends, len(st.errs)-start)
		}
		if st.quick || st.halt != nil {
			return false
		}
		causes := st.takeErrors(start)
		st.fail("type", alternatives(quoteValue(v)+head, causes, ends, labels, len(st.path)), v, causes)
		return false
	}, nil
}

// enum: the value equals one of the listed values
func (c *compiler) enum(l loc, value any) (checkFn, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, c.bad(l, "/enum", "enum must be an array")
	}
	var set valueSet
	for _, item := range list {
		set.add(item)
	}
	text := quoteSchema(list)
	return func(st *state, v any, _ *evaluated) bool {
		if set.has(v) {
			return true
		}
		if !st.quick {
			st.fail("enum", quoteValue(v)+" is not one of "+text, v, nil)
		}
		return false
	}, nil
}

// const: the value equals the given one
func constCheck(value any) checkFn {
	var set valueSet
	set.add(value)
	text := quoteSchema(value)
	return func(st *state, v any, _ *evaluated) bool {
		if set.has(v) {
			return true
		}
		if !st.quick {
			st.fail("const", quoteValue(v)+" is not the expected value "+text, v, nil)
		}
		return false
	}
}

// format: a string conforms to the format when the format is asserted, nil when it is only noted
func (c *compiler) format(l loc, value any) checkFn {
	name, ok := value.(string)
	if !ok {
		return nil
	}
	valid := c.cfg.format(name, l.dialect.draft)
	if valid == nil {
		return nil
	}
	text := quote(name, schemaQuoteLimit)
	return func(st *state, v any, _ *evaluated) bool {
		s, ok := v.(string)
		if !ok || valid(s) {
			return true
		}
		if !st.quick {
			st.fail("format", quoteValue(v)+" is not a valid "+text, v, nil)
		}
		return false
	}
}

// multipleOf, and divisibleBy of draft 3: a number is a whole multiple of the divisor, both taken as the decimals they are written as
func (c *compiler) multipleOf(l loc, key string, value any) (checkFn, error) {
	d, ok := numberOf(value)
	dr := d.rat()
	if !ok || dr == nil || dr.Sign() <= 0 {
		return nil, c.bad(l, "/"+key, "%s must be a number greater than 0", key)
	}
	text := quoteSchema(value)
	return func(st *state, v any, _ *evaluated) bool {
		x, ok := numberOf(v)
		if !ok || x.multipleOf(d, dr) {
			return true
		}
		if !st.quick {
			st.fail(key, quoteValue(v)+" is not a multiple of "+text, v, nil)
		}
		return false
	}, nil
}

// minimum, maximum, exclusiveMinimum and exclusiveMaximum as numbers, draft 6 on
func (c *compiler) bound(l loc, key string, value any) (checkFn, error) {
	lower := key == "minimum" || key == "exclusiveMinimum"
	strict := key == "exclusiveMinimum" || key == "exclusiveMaximum"
	return c.boundCheck(l, key, value, lower, strict)
}

// minimum and maximum of drafts 3 and 4, made exclusive by a true exclusiveMinimum or exclusiveMaximum beside them
func (c *compiler) legacyBound(l loc, o *jsonutil.Object, key string, value any) (checkFn, error) {
	lower := key == "minimum"
	flag := "exclusiveMaximum"
	if lower {
		flag = "exclusiveMinimum"
	}
	strict, _ := o.Value(flag).(bool)
	return c.boundCheck(l, key, value, lower, strict)
}

func (c *compiler) boundCheck(l loc, key string, value any, lower, strict bool) (checkFn, error) {
	limit, ok := numberOf(value)
	if !ok {
		return nil, c.bad(l, "/"+key, "%s must be a number", key)
	}
	var phrase string
	switch {
	case lower && strict:
		phrase = " is less than or equal to the exclusive minimum of "
	case lower:
		phrase = " is less than the minimum of "
	case strict:
		phrase = " is greater than or equal to the exclusive maximum of "
	default:
		phrase = " is greater than the maximum of "
	}
	text := quoteSchema(value)
	return func(st *state, v any, _ *evaluated) bool {
		x, ok := numberOf(v)
		if !ok {
			return true
		}
		order, ordered := x.compare(limit)
		if !ordered {
			return true
		}
		if lower {
			order = -order
		}
		if order < 0 || order == 0 && !strict {
			return true
		}
		if !st.quick {
			st.fail(key, quoteValue(v)+phrase+text, v, nil)
		}
		return false
	}, nil
}

// Non-negative integer a count keyword holds, false for anything else
func nonNegative(value any) (int, bool) {
	n, ok := numberOf(value)
	if !ok || !n.isInteger() || n.float() < 0 {
		return 0, false
	}
	if !n.exact || n.i > int64(maxInt) {
		return maxInt, true
	}
	return int(n.i), true
}

// Largest int
const maxInt = int(^uint(0) >> 1)

// minLength and maxLength: the length of a string in characters
func (c *compiler) length(l loc, key string, value any) (checkFn, error) {
	limit, ok := nonNegative(value)
	if !ok {
		return nil, c.bad(l, "/"+key, "%s must be a non-negative integer", key)
	}
	lower := key == "minLength"
	return func(st *state, v any, _ *evaluated) bool {
		s, ok := v.(string)
		if !ok || !lower && len(s) <= limit {
			return true
		}
		n := utf8.RuneCountInString(s)
		if lower && n >= limit || !lower && n <= limit {
			return true
		}
		if !st.quick {
			st.fail(key, countMessage(v, n, limit, lower, "character", "characters"), v, nil)
		}
		return false
	}, nil
}

// pattern: a string matches the ECMA-262 regular expression somewhere
func (c *compiler) pattern(l loc, value any) (checkFn, error) {
	p, ok := value.(string)
	if !ok {
		return nil, c.bad(l, "/pattern", "pattern must be a string")
	}
	re, err := c.regex(l, "/pattern", p)
	if err != nil {
		return nil, err
	}
	text := quote(p, schemaQuoteLimit)
	return func(st *state, v any, _ *evaluated) bool {
		s, ok := v.(string)
		if !ok {
			return true
		}
		if st.matches(re, "pattern", s) {
			return true
		}
		if !st.quick {
			st.fail("pattern", quoteValue(v)+" does not match the pattern "+text, v, nil)
		}
		return false
	}, nil
}
