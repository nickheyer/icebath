package pyast

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type nodeRecRow struct {
	Name, Vary string
	M, N       int
	Err        []string
	AtN        []string `json:"at_n"`
	Wrap, Step json.RawMessage
	Leaf       json.RawMessage
}

// Replaces the marker Name of a template, or the Expr or MatchValue holding it when the filler is not an expression
func fillHole(t *testing.T, v any, x *Node) bool {
	items, isList := listItems(v)
	if !isList {
		n, ok := v.(*Node)
		if !ok || n == nil {
			return false
		}
		for i, f := range n.spec.fields {
			c, _ := n.vals[i].(*Node)
			if c != nil && (holeName(c) || x.Base() != "expr" && c.Is("Expr", "MatchValue") && holeName(c.Child("value"))) {
				n.vals[i] = x
				return true
			}
			if l, ok := n.vals[i].([]*Node); ok {
				for j, c := range l {
					if c != nil && (holeName(c) || x.Base() != "expr" && c.Is("Expr", "MatchValue") && holeName(c.Child("value"))) {
						l[j] = x
						return true
					}
				}
			}
			if fillHole(t, n.vals[i], x) {
				return true
			}
			_ = f
		}
		return false
	}
	for _, it := range items {
		if fillHole(t, it, x) {
			return true
		}
	}
	return false
}

// Copies the nodes and lists of a decoded template, sharing the immutable constants
func cloneTree(v any) any {
	switch x := v.(type) {
	case *Node:
		if x == nil {
			return x
		}
		c := *x
		c.vals = make([]any, len(x.vals))
		for i, e := range x.vals {
			c.vals[i] = cloneTree(e)
		}
		return &c
	case []*Node:
		out := make([]*Node, len(x))
		for i, e := range x {
			out[i] = cloneTree(e).(*Node)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneTree(e)
		}
		return out
	case nodeAttrs:
		out := nodeAttrs{}
		for k, e := range x {
			out[k] = cloneTree(e)
		}
		return out
	}
	return v
}

func holeName(n *Node) bool {
	id, _ := n.Get("id").(string)
	return n.Is("Name") && id == "\x00"
}

func rawJSON(t *testing.T, raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Builds the family tree with n chain steps, or a constant nested n deep under m steps
func (r nodeRecRow) build(t *testing.T, n int) *Node {
	var x *Node
	steps := n
	if r.Vary == "leaf" {
		var kind string
		json.Unmarshal(r.Leaf, &kind)
		var c any = pyrepr.Tuple{}
		if kind == "fset" {
			c = FrozenSet{}
		}
		for i := 0; i < n; i++ {
			if kind == "fset" {
				c = FrozenSet{c}
			} else {
				c = pyrepr.Tuple{c}
			}
		}
		x = NewNode("Constant", c, nil)
		x.Lineno, x.EndLineno = 1, 1
		steps = r.M
	} else {
		x = decodePyValue(t, rawJSON(t, r.Leaf)).(*Node)
	}
	var step *Node
	if steps > 0 {
		step = decodePyValue(t, rawJSON(t, r.Step)).(*Node)
	}
	for i := 0; i < steps; i++ {
		s := cloneTree(step).(*Node)
		if !fillHole(t, s, x) {
			t.Fatalf("%s: step without a hole", r.Name)
		}
		x = s
	}
	w := decodePyValue(t, rawJSON(t, r.Wrap)).(*Node)
	if !fillHole(t, w, x) {
		t.Fatalf("%s: wrapper without a hole", r.Name)
	}
	return w
}

func TestParseNodeCyclicTreeRaisesRecursionError(t *testing.T) {
	e := NewNode("UnaryOp", NewNode("USub"), nil)
	e.Set("operand", e)
	_, err := ParseNode(NewNode("Module", []*Node{NewNode("Expr", e)}, []*Node{}), Options{})
	if le, ok := err.(*LimitError); !ok || le.Msg != fmt.Sprintf("Stack overflow (used %d kB) while traversing 'expr' node", nodeStack.kB) {
		t.Fatalf("got %v", err)
	}
}
