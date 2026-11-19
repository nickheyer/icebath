package pyast

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// PyTypeName names the Python class
func (Complex) PyTypeName() string { return "complex" }

// PyTypeName names the Python class
func (Ellipsis) PyTypeName() string { return "ellipsis" }

// Lists the items of a Python list value
func listItems(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []*Node:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = n
		}
		return out, true
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

func pyErr(class, format string, args ...any) error {
	return &LimitError{Class: class, Msg: fmt.Sprintf(format, args...)}
}

type reprState struct {
	nodes map[*Node]bool
	lists map[*any]bool
}

// Renders a value as repr does with the 3.14 AST repr for nodes, failing as CPython fails
func objRepr(v any) (string, error) {
	return (&reprState{nodes: map[*Node]bool{}, lists: map[*any]bool{}}).repr(v)
}

func (st *reprState) repr(v any) (string, error) {
	switch x := v.(type) {
	case *Node:
		if x == nil {
			return "None", nil
		}
		return st.node(x, 3)
	case pyrepr.Tuple:
		return st.seq([]any(x), "(", ")", len(x) == 1)
	}
	if items, ok := listItems(v); ok {
		if len(items) > 0 {
			if l, ok := v.([]any); ok {
				if st.lists[&l[0]] {
					return "[...]", nil
				}
				st.lists[&l[0]] = true
				defer delete(st.lists, &l[0])
			}
		}
		return st.seq(items, "[", "]", false)
	}
	return ConstantRepr(v)
}

func (st *reprState) seq(items []any, open, close string, comma bool) (string, error) {
	parts := make([]string, len(items))
	for i, it := range items {
		s, err := st.repr(it)
		if err != nil {
			return "", err
		}
		parts[i] = s
	}
	if comma {
		return open + parts[0] + "," + close, nil
	}
	return open + strings.Join(parts, ", ") + close, nil
}

// Renders a node as ast_repr_max_depth with lists cut to their first and last items
func (st *reprState) node(n *Node, depth int) (string, error) {
	if depth <= 0 || st.nodes[n] {
		return n.Type + "(...)", nil
	}
	if len(n.spec.fields) == 0 {
		return n.Type + "()", nil
	}
	st.nodes[n] = true
	defer delete(st.nodes, n)
	var b strings.Builder
	b.WriteString(n.Type + "(")
	for i, f := range n.spec.fields {
		v := n.vals[i]
		var s string
		var err error
		if tup, ok := v.(pyrepr.Tuple); ok {
			s, err = st.list([]any(tup), "(", ")", depth)
		} else if items, ok := listItems(v); ok {
			s, err = st.list(items, "[", "]", depth)
		} else if c, ok := v.(*Node); ok && c != nil {
			s, err = st.node(c, depth-1)
		} else {
			s, err = st.repr(v)
		}
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(f.name + "=" + s)
	}
	b.WriteByte(')')
	return b.String(), nil
}

// Renders a field list as ast_repr_list, first and last items around an ellipsis
func (st *reprState) list(items []any, open, close string, depth int) (string, error) {
	if len(items) == 0 {
		return open + close, nil
	}
	pick := []any{items[0]}
	if len(items) > 1 {
		pick = append(pick, items[len(items)-1])
	}
	var b strings.Builder
	b.WriteString(open)
	for i, it := range pick {
		if i > 0 {
			b.WriteString(", ")
		}
		var s string
		var err error
		if c, ok := it.(*Node); ok && c != nil {
			s, err = st.node(c, depth-1)
		} else {
			s, err = st.repr(it)
		}
		if err != nil {
			return "", err
		}
		b.WriteString(s)
		if i == 0 && len(items) > 2 {
			b.WriteString(", ...")
		}
	}
	b.WriteString(close)
	return b.String(), nil
}
