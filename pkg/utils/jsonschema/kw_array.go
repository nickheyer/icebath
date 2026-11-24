package jsonschemautil

import (
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// items of draft 2020-12: the schema applied to every item after those prefixItems covers
func (c *compiler) items(l loc, o *jsonutil.Object, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/items")
	if err != nil {
		return nil, err
	}
	prefix := 0
	if p, ok := o.Value("prefixItems").([]any); ok {
		prefix = len(p)
	}
	return func(st *state, v any, ev *evaluated) bool {
		arr, ok := v.([]any)
		if !ok || len(arr) <= prefix {
			return true
		}
		if ev != nil {
			ev.allItems = true
		}
		switch n.boolean {
		case boolTrue:
			return true
		case boolFalse:
			if !st.quick {
				st.fail("items", "expected at most "+count(prefix, "item", "items")+" but found "+strconv.Itoa(len(arr)-prefix)+
					" extra: "+quoteValue(arr[prefix:]), v, nil)
			}
			return false
		}
		valid := true
		for i := prefix; i < len(arr); i++ {
			if !st.member(n, arr[i], step{index: i}) {
				valid = false
				if st.quick {
					return false
				}
			}
		}
		return valid
	}, nil
}

// items of drafts 3 to 2019-09: one schema for every item, or an array of schemas for the leading items
func (c *compiler) legacyItems(l loc, value any) (checkFn, error) {
	if list, ok := value.([]any); ok {
		return c.tuple(l, "items", list)
	}
	n, err := c.sub(value, l, "/items")
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		if ev != nil {
			ev.allItems = true
		}
		if n.boolean == boolTrue {
			return true
		}
		valid := true
		for i, item := range arr {
			if !st.member(n, item, step{index: i}) {
				valid = false
				if st.quick {
					return false
				}
			}
		}
		return valid
	}, nil
}

// prefixItems of draft 2020-12
func (c *compiler) prefixItems(l loc, value any) (checkFn, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, c.bad(l, "/prefixItems", "prefixItems must be an array of schemas")
	}
	return c.tuple(l, "prefixItems", list)
}

// Each schema of a list applied to the item at its position
func (c *compiler) tuple(l loc, key string, list []any) (checkFn, error) {
	subs, tokens, err := c.subschemas(l, key, list)
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		n := min(len(arr), len(subs))
		if ev != nil {
			ev.leading(n)
		}
		valid := true
		for i := range n {
			st.push(tokens[i])
			ok := st.member(subs[i], arr[i], step{index: i})
			st.pop()
			if !ok {
				valid = false
				if st.quick {
					return false
				}
			}
		}
		return valid
	}, nil
}

// additionalItems: the schema applied to the items after an array of items schemas, nothing when items is not an array
func (c *compiler) additionalItems(l loc, o *jsonutil.Object, value any) (checkFn, error) {
	list, ok := o.Value("items").([]any)
	if !ok {
		return nil, nil
	}
	n, err := c.sub(value, l, "/additionalItems")
	if err != nil {
		return nil, err
	}
	from := len(list)
	return func(st *state, v any, ev *evaluated) bool {
		arr, ok := v.([]any)
		if !ok || len(arr) <= from {
			return true
		}
		if ev != nil {
			ev.allItems = true
		}
		switch n.boolean {
		case boolTrue:
			return true
		case boolFalse:
			if !st.quick {
				extra := arr[from:]
				st.fail("additionalItems", "additional items are not allowed ("+valueList(extra)+" "+wasWere(len(extra))+" unexpected)", v, nil)
			}
			return false
		}
		valid := true
		for i := from; i < len(arr); i++ {
			if !st.member(n, arr[i], step{index: i}) {
				valid = false
				if st.quick {
					return false
				}
			}
		}
		return valid
	}, nil
}

// contains of drafts 6 and 7: at least one item passes the schema
func (c *compiler) containsAny(l loc, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/contains")
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, _ *evaluated) bool {
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		for i, item := range arr {
			if st.memberPasses(n, item, step{index: i}) {
				return true
			}
		}
		if !st.quick {
			st.fail("contains", quoteValue(v)+` has no item that matches the "contains" schema`, v, nil)
		}
		return false
	}, nil
}

// contains of draft 2019-09 on: the number of items passing the schema lies within minContains, 1 when absent, and maxContains
func (c *compiler) contains(l loc, o *jsonutil.Object, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/contains")
	if err != nil {
		return nil, err
	}
	least, most := 1, -1
	if raw, ok := o.Get("minContains"); ok {
		if least, ok = nonNegative(raw); !ok {
			return nil, c.bad(l, "/minContains", "minContains must be a non-negative integer")
		}
	}
	if raw, ok := o.Get("maxContains"); ok {
		if most, ok = nonNegative(raw); !ok {
			return nil, c.bad(l, "/maxContains", "maxContains must be a non-negative integer")
		}
	}
	marks := l.dialect.containsEvaluates
	return func(st *state, v any, ev *evaluated) bool {
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		track := marks && ev != nil
		matches := 0
		for i, item := range arr {
			if !st.memberPasses(n, item, step{index: i}) {
				continue
			}
			matches++
			if track {
				ev.item(i)
			} else if st.quick && most >= 0 && matches > most {
				return false
			}
		}
		switch {
		case most >= 0 && matches > most:
			if !st.quick {
				old := st.rename("maxContains")
				st.fail("maxContains", "too many items of "+quoteValue(v)+` match the "contains" schema: expected at most `+
					strconv.Itoa(most)+", found "+strconv.Itoa(matches), v, nil)
				st.rename(old)
			}
			return false
		case matches < least && matches == 0:
			if !st.quick {
				st.fail("contains", quoteValue(v)+` has no item that matches the "contains" schema`, v, nil)
			}
			return false
		case matches < least:
			if !st.quick {
				old := st.rename("minContains")
				st.fail("minContains", "too few items of "+quoteValue(v)+` match the "contains" schema: expected at least `+
					strconv.Itoa(least)+", found "+strconv.Itoa(matches), v, nil)
				st.rename(old)
			}
			return false
		}
		return true
	}, nil
}

// minItems and maxItems
func (c *compiler) itemCount(l loc, key string, value any) (checkFn, error) {
	limit, ok := nonNegative(value)
	if !ok {
		return nil, c.bad(l, "/"+key, "%s must be a non-negative integer", key)
	}
	lower := key == "minItems"
	return func(st *state, v any, _ *evaluated) bool {
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		n := len(arr)
		if lower && n >= limit || !lower && n <= limit {
			return true
		}
		if !st.quick {
			st.fail(key, countMessage(v, n, limit, lower, "item", "items"), v, nil)
		}
		return false
	}, nil
}

// uniqueItems: no two items are equal, nil when it is not true
func uniqueItems(value any) checkFn {
	if on, _ := value.(bool); !on {
		return nil
	}
	return func(st *state, v any, _ *evaluated) bool {
		arr, ok := v.([]any)
		if !ok || len(arr) < 2 {
			return true
		}
		i, j, dup := firstDuplicate(arr)
		if !dup {
			return true
		}
		if !st.quick {
			st.fail("uniqueItems", quoteValue(v)+" has non-unique elements (items "+strconv.Itoa(i)+" and "+strconv.Itoa(j)+" are equal)", v, nil)
		}
		return false
	}
}

// unevaluatedItems: the schema applied to the items no other keyword of the schema or of the subschemas that passed evaluated
func (c *compiler) unevaluatedItems(l loc, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/unevaluatedItems")
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		if ev == nil {
			ev = &evaluated{}
		}
		if ev.allItems {
			return true
		}
		valid := true
		var extras []any
		for i := ev.items; i < len(arr); i++ {
			if ev.hasItem(i) {
				continue
			}
			switch n.boolean {
			case boolTrue:
			case boolFalse:
				if st.quick {
					return false
				}
				extras = append(extras, arr[i])
			default:
				if !st.member(n, arr[i], step{index: i}) {
					valid = false
					if st.quick {
						return false
					}
				}
			}
		}
		ev.allItems = true
		if len(extras) > 0 {
			st.fail("unevaluatedItems", "unevaluated items are not allowed ("+valueList(extras)+" "+wasWere(len(extras))+" unexpected)", v, nil)
			return false
		}
		return valid
	}, nil
}
