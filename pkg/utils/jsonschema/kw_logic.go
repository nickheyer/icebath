package jsonschemautil

import (
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// $ref, and $dynamicRef when dynamic: the referenced schema applied to the same value, a dynamic reference to a dynamic anchor resolved again in the dynamic scope
func (c *compiler) ref(l loc, value any, dynamic bool) (checkFn, error) {
	key := "$ref"
	if dynamic {
		key = "$dynamicRef"
	}
	ref, ok := value.(string)
	if !ok {
		return nil, c.bad(l, "/"+key, "%s must be a string", key)
	}
	target, at, name, err := c.resolve(l, ref, dynamic)
	if err != nil {
		return nil, err
	}
	n, err := c.compile(target, at)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return func(st *state, v any, ev *evaluated) bool { return st.inPlace(n, v, ev) }, nil
	}
	c.dynamic = true
	return func(st *state, v any, ev *evaluated) bool { return st.inPlace(st.dynamicTarget(n, name), v, ev) }, nil
}

// $recursiveRef: the referenced schema applied to the same value, moved outward through the dynamic scope when it holds "$recursiveAnchor": true
func (c *compiler) recursiveRef(l loc, value any) (checkFn, error) {
	ref, ok := value.(string)
	if !ok {
		return nil, c.bad(l, "/$recursiveRef", "$recursiveRef must be a string")
	}
	target, at, _, err := c.resolve(l, ref, false)
	if err != nil {
		return nil, err
	}
	n, err := c.compile(target, at)
	if err != nil {
		return nil, err
	}
	if ref != "#" || at.ptr != "" || !at.res.recursive {
		return func(st *state, v any, ev *evaluated) bool { return st.inPlace(n, v, ev) }, nil
	}
	c.dynamic = true
	return func(st *state, v any, ev *evaluated) bool { return st.inPlace(st.recursiveTarget(n), v, ev) }, nil
}

// Nodes of an array of schemas under key and the position tokens of their schema paths
func (c *compiler) subschemas(l loc, key string, value any) ([]*node, []string, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, nil, c.bad(l, "/"+key, "%s must be an array of schemas", key)
	}
	nodes := make([]*node, len(list))
	tokens := make([]string, len(list))
	for i, item := range list {
		tokens[i] = strconv.Itoa(i)
		n, err := c.sub(item, l, "/"+key+"/"+tokens[i])
		if err != nil {
			return nil, nil, err
		}
		nodes[i] = n
	}
	return nodes, tokens, nil
}

// allOf, and the array form of extends: every schema applied to the same value
func (c *compiler) allOf(l loc, key string, value any) (checkFn, error) {
	subs, tokens, err := c.subschemas(l, key, value)
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		valid := true
		for i, sub := range subs {
			st.push(tokens[i])
			ok := st.inPlace(sub, v, ev)
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

// anyOf: at least one schema passes; when none does, one error carrying every failure as its causes
func (c *compiler) anyOf(l loc, value any) (checkFn, error) {
	subs, tokens, err := c.subschemas(l, "anyOf", value)
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		start := len(st.errs)
		var ends []int
		matched := false
		for i, sub := range subs {
			st.push(tokens[i])
			var ok bool
			if matched || st.quick {
				ok = st.passes(sub, v, ev)
			} else {
				ok = st.inPlace(sub, v, ev)
			}
			st.pop()
			switch {
			case ok && !matched:
				matched = true
				st.drop(start)
				if ev == nil {
					return true
				}
			case !ok && !matched:
				ends = append(ends, len(st.errs)-start)
			}
		}
		if matched {
			return true
		}
		if st.quick || st.halt != nil {
			return false
		}
		causes := st.takeErrors(start)
		st.fail("anyOf", alternatives(quoteValue(v)+" is not valid under any of the given schemas", causes, ends, tokens, len(st.path)), v, causes)
		return false
	}, nil
}

// oneOf: exactly one schema passes; none passing gives one error carrying every failure as its causes
func (c *compiler) oneOf(l loc, value any) (checkFn, error) {
	subs, tokens, err := c.subschemas(l, "oneOf", value)
	if err != nil {
		return nil, err
	}
	return func(st *state, v any, ev *evaluated) bool {
		start := len(st.errs)
		var ends, more []int
		first := -1
		var kept *evaluated
		for i, sub := range subs {
			child := freshEvaluated(ev)
			st.push(tokens[i])
			var ok bool
			if first < 0 && !st.quick {
				ok = st.validate(sub, v, child)
			} else {
				ok = st.test(sub, v, child)
			}
			st.pop()
			switch {
			case !ok:
				if first < 0 {
					ends = append(ends, len(st.errs)-start)
				}
			case first < 0:
				first, kept = i, child
				st.drop(start)
			default:
				if st.quick {
					return false
				}
				more = append(more, i)
			}
		}
		if first < 0 {
			if st.quick || st.halt != nil {
				return false
			}
			causes := st.takeErrors(start)
			st.fail("oneOf", alternatives(quoteValue(v)+" is not valid under any of the given schemas", causes, ends, tokens, len(st.path)), v, causes)
			return false
		}
		if len(more) > 0 {
			st.fail("oneOf", quoteValue(v)+" is valid under more than one of the given schemas (alternatives "+
				positionList(append([]int{first}, more...))+") but must be valid under exactly one", v, nil)
			return false
		}
		if ev != nil {
			ev.merge(kept)
		}
		return true
	}, nil
}

// not: the schema must fail on the value
func (c *compiler) not(l loc, value any) (checkFn, error) {
	n, err := c.sub(value, l, "/not")
	if err != nil {
		return nil, err
	}
	text := quoteSchema(value)
	return func(st *state, v any, _ *evaluated) bool {
		if !st.test(n, v, nil) {
			return true
		}
		if !st.quick {
			st.fail("not", quoteValue(v)+" should not be valid under "+text, v, nil)
		}
		return false
	}, nil
}

// if with its then and else: then applies when the if schema passes, else when it fails
func (c *compiler) ifThenElse(l loc, o *jsonutil.Object, value any) (checkFn, error) {
	cond, err := c.sub(value, l, "/if")
	if err != nil {
		return nil, err
	}
	var then, otherwise *node
	if t, ok := o.Get("then"); ok {
		if then, err = c.sub(t, l, "/then"); err != nil {
			return nil, err
		}
	}
	if e, ok := o.Get("else"); ok {
		if otherwise, err = c.sub(e, l, "/else"); err != nil {
			return nil, err
		}
	}
	return func(st *state, v any, ev *evaluated) bool {
		if then == nil && otherwise == nil {
			if ev != nil {
				st.passes(cond, v, ev)
			}
			return true
		}
		branch, token := otherwise, "else"
		if st.passes(cond, v, ev) {
			branch, token = then, "then"
		}
		if branch == nil {
			return true
		}
		old := st.rename(token)
		ok := st.inPlace(branch, v, ev)
		st.rename(old)
		return ok
	}, nil
}
