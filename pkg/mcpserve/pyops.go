// Package mcpserve runs icebath as a stdio MCP server over the messaging conversations, optionally with icebath tools
package mcpserve

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Python exception raised by a dict, string or comparison operation, str giving Msg
type pyError struct {
	Kind string
	Msg  string
}

func (e *pyError) Error() string { return e.Msg }

// Ordered view of a dict value, false for anything Python would not treat as a dict
func asDict(v any) (*jsonutil.Object, bool) {
	switch t := v.(type) {
	case *jsonutil.Object:
		return t, t != nil
	case map[string]any:
		return jsonutil.AsObject(t)
	}
	return nil, false
}

// Raises the AttributeError of calling method on a value without it
func noAttribute(v any, method string) {
	panic(&pyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", anyutil.Kind(v), method)})
}

// Value of d.get(key, def), an AttributeError when d is no dict
func pyGet(d any, key string, def any) any {
	switch t := d.(type) {
	case *jsonutil.Object:
		if t != nil {
			if v, ok := t.Get(key); ok {
				return v
			}
			return def
		}
	case map[string]any:
		if v, ok := t[key]; ok {
			return v
		}
		return def
	}
	noAttribute(d, "get")
	return nil
}

// Value of d[key], the KeyError of a missing key or the TypeError of subscripting a value that is no dict
func pyIndex(d any, key string) any {
	o, ok := asDict(d)
	if !ok {
		panic(&pyError{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", anyutil.Kind(d))})
	}
	v, present := o.Get(key)
	if !present {
		panic(&pyError{"KeyError", strconv.Quote(key)})
	}
	return v
}

// Keys and values of d.items() in dict order, an AttributeError when d is no dict
func pyItems(d any) ([]string, map[string]any) {
	o, ok := asDict(d)
	if !ok {
		noAttribute(d, "items")
	}
	return o.Keys(), o.Map()
}

// Value of s.lower(), an AttributeError when s is no str
func pyLower(v any) string {
	s, ok := v.(string)
	if !ok {
		noAttribute(v, "lower")
	}
	return strings.ToLower(s)
}

// Raises the TypeError a set membership test raises for an unhashable value
func pyHashable(v any) {
	switch v.(type) {
	case []any, map[string]any, *jsonutil.Object:
		panic(&pyError{"TypeError", fmt.Sprintf("unhashable type: '%s'", anyutil.Kind(v))})
	}
}

// Python number of v as a float for ordering, false for a value that is no int, bool or float
func pyNumber(v any) (*big.Float, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case float64:
		if math.IsNaN(t) {
			return nil, true
		}
		return big.NewFloat(t), true
	}
	if n, ok := anyutil.Int(v); ok {
		return new(big.Float).SetInt64(n), true
	}
	return nil, false
}

// Python a < b for str, numbers and lists, the TypeError of < for any other pairing
func pyLess(a, b any) bool {
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return pyStrLess(sa, sb)
		}
	}
	na, aNum := pyNumber(a)
	nb, bNum := pyNumber(b)
	if aNum && bNum {
		if na == nil || nb == nil {
			return false
		}
		return na.Cmp(nb) < 0
	}
	la, aList := a.([]any)
	lb, bList := b.([]any)
	if aList && bList {
		for i := 0; i < len(la) && i < len(lb); i++ {
			if !anyutil.Equal(la[i], lb[i]) {
				return pyLess(la[i], lb[i])
			}
		}
		return len(la) < len(lb)
	}
	panic(&pyError{"TypeError", fmt.Sprintf("'<' not supported between instances of '%s' and '%s'", anyutil.Kind(a), anyutil.Kind(b))})
}

// Code point order of two strings as Python compares str
func pyStrLess(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] != rb[i] {
			return ra[i] < rb[i]
		}
	}
	return len(ra) < len(rb)
}

// Stable sort of items by key, descending like sorted(reverse=True) keeping equal keys in their order
func sortByKey[T any](items []T, key func(T) any, descending bool) {
	sort.SliceStable(items, func(i, j int) bool {
		if descending {
			return pyLess(key(items[j]), key(items[i]))
		}
		return pyLess(key(items[i]), key(items[j]))
	})
}

// json.dumps of v, indented by two spaces when indent is set, raising what json.dumps raises
func dumps(v any, indent bool) string {
	var text string
	var err error
	if indent {
		text, err = jsonutil.Encode(v, jsonutil.Indent(2))
	} else {
		text, err = jsonutil.Encode(v)
	}
	if err != nil {
		panic(err)
	}
	return text
}

// Text of a raised value as str(exc) renders it
func raisedText(r any) string {
	if e, ok := r.(error); ok {
		return e.Error()
	}
	return anyutil.String(r)
}
