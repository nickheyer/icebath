package pyast

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Missing stands for a field or attribute the AST object lacks, as after del node.attr
type Missing struct{}

// FrozenSet is a frozenset constant holding its elements in iteration order
type FrozenSet []any

// PyRepr renders the set as frozenset.__repr__
func (s FrozenSet) PyRepr() string {
	if len(s) == 0 {
		return "frozenset()"
	}
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = pyrepr.Repr(v)
	}
	return "frozenset({" + strings.Join(parts, ", ") + "})"
}

// PyTypeName names the Python class
func (FrozenSet) PyTypeName() string { return "frozenset" }

// PyTypeName names the Python class
func (Complex) PyTypeName() string { return "complex" }

// PyTypeName names the Python class
func (Ellipsis) PyTypeName() string { return "ellipsis" }

type nodeAttrs map[string]any

// SetAttr sets any attribute like setattr, a field, a location attribute or another name, and Missing removes it
func (n *Node) SetAttr(name string, v any) {
	if i := n.index(name); i >= 0 {
		n.vals[i] = v
		return
	}
	k := len(n.spec.fields)
	if len(n.vals) == k {
		n.vals = append(n.vals[:k:k], nodeAttrs{})
	}
	n.vals[k].(nodeAttrs)[name] = v
}

// Returns the attribute value as getattr sees it, Missing when the object has none
func getattr(v any, name string) any {
	n, ok := v.(*Node)
	if !ok || n == nil {
		return Missing{}
	}
	if i := n.index(name); i >= 0 {
		return n.vals[i]
	}
	if k := len(n.spec.fields); len(n.vals) > k {
		if x, ok := n.vals[k].(nodeAttrs)[name]; ok {
			return x
		}
	}
	if n.spec.attrs {
		switch name {
		case "lineno":
			return n.Lineno
		case "col_offset":
			return n.ColOffset
		case "end_lineno":
			return n.EndLineno
		case "end_col_offset":
			return n.EndColOffset
		}
	}
	return Missing{}
}

func isNone(v any) bool {
	n, ok := v.(*Node)
	return v == nil || ok && n == nil
}

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

// Names the Python class of a value as _PyType_Name does
func typeName(v any) string {
	if n, ok := v.(*Node); ok {
		if n == nil {
			return "NoneType"
		}
		return n.Type
	}
	if _, ok := listItems(v); ok {
		return "list"
	}
	return pyrepr.TypeName(v)
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
	case FrozenSet:
		if len(x) == 0 {
			return "frozenset()", nil
		}
		s, err := st.seq([]any(x), "{", "}", false)
		return "frozenset(" + s + ")", err
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
		if _, gone := v.(Missing); gone {
			return "", pyErr("AttributeError", "'%s' object has no attribute '%s'", n.Type, f.name)
		}
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

// Formats a TypeError or ValueError carrying the repr of a value, the repr failure winning as in PyErr_Format
func reprErr(class, format string, v any) error {
	s, err := objRepr(v)
	if err != nil {
		return err
	}
	return pyErr(class, format, s)
}

// ASDL types of every constructor field in _fields order, generated from Parser/Python.asdl
var fieldTypes = map[string]string{
	"Module":           "stmt,type_ignore",
	"Interactive":      "stmt",
	"Expression":       "expr",
	"FunctionType":     "expr,expr",
	"FunctionDef":      "identifier,arguments,stmt,expr,expr,string,type_param",
	"AsyncFunctionDef": "identifier,arguments,stmt,expr,expr,string,type_param",
	"ClassDef":         "identifier,expr,keyword,stmt,expr,type_param",
	"Return":           "expr",
	"Delete":           "expr",
	"Assign":           "expr,expr,string",
	"TypeAlias":        "expr,type_param,expr",
	"AugAssign":        "expr,operator,expr",
	"AnnAssign":        "expr,expr,expr,int",
	"For":              "expr,expr,stmt,stmt,string",
	"AsyncFor":         "expr,expr,stmt,stmt,string",
	"While":            "expr,stmt,stmt",
	"If":               "expr,stmt,stmt",
	"With":             "withitem,stmt,string",
	"AsyncWith":        "withitem,stmt,string",
	"Match":            "expr,match_case",
	"Raise":            "expr,expr",
	"Try":              "stmt,excepthandler,stmt,stmt",
	"TryStar":          "stmt,excepthandler,stmt,stmt",
	"Assert":           "expr,expr",
	"Import":           "alias",
	"ImportFrom":       "identifier,alias,int",
	"Global":           "identifier",
	"Nonlocal":         "identifier",
	"Expr":             "expr",
	"Pass":             "",
	"Break":            "",
	"Continue":         "",
	"BoolOp":           "boolop,expr",
	"NamedExpr":        "expr,expr",
	"BinOp":            "expr,operator,expr",
	"UnaryOp":          "unaryop,expr",
	"Lambda":           "arguments,expr",
	"IfExp":            "expr,expr,expr",
	"Dict":             "expr,expr",
	"Set":              "expr",
	"ListComp":         "expr,comprehension",
	"SetComp":          "expr,comprehension",
	"DictComp":         "expr,expr,comprehension",
	"GeneratorExp":     "expr,comprehension",
	"Await":            "expr",
	"Yield":            "expr",
	"YieldFrom":        "expr",
	"Compare":          "expr,cmpop,expr",
	"Call":             "expr,expr,keyword",
	"FormattedValue":   "expr,int,expr",
	"Interpolation":    "expr,constant,int,expr",
	"JoinedStr":        "expr",
	"TemplateStr":      "expr",
	"Constant":         "constant,string",
	"Attribute":        "expr,identifier,expr_context",
	"Subscript":        "expr,expr,expr_context",
	"Starred":          "expr,expr_context",
	"Name":             "identifier,expr_context",
	"List":             "expr,expr_context",
	"Tuple":            "expr,expr_context",
	"Slice":            "expr,expr,expr",
	"Load":             "",
	"Store":            "",
	"Del":              "",
	"And":              "",
	"Or":               "",
	"Add":              "",
	"Sub":              "",
	"Mult":             "",
	"MatMult":          "",
	"Div":              "",
	"Mod":              "",
	"Pow":              "",
	"LShift":           "",
	"RShift":           "",
	"BitOr":            "",
	"BitXor":           "",
	"BitAnd":           "",
	"FloorDiv":         "",
	"Invert":           "",
	"Not":              "",
	"UAdd":             "",
	"USub":             "",
	"Eq":               "",
	"NotEq":            "",
	"Lt":               "",
	"LtE":              "",
	"Gt":               "",
	"GtE":              "",
	"Is":               "",
	"IsNot":            "",
	"In":               "",
	"NotIn":            "",
	"comprehension":    "expr,expr,expr,int",
	"ExceptHandler":    "expr,identifier,stmt",
	"arguments":        "arg,arg,arg,arg,expr,arg,expr",
	"arg":              "identifier,expr,string",
	"keyword":          "identifier,expr",
	"alias":            "identifier,identifier",
	"withitem":         "expr,expr",
	"match_case":       "pattern,expr,stmt",
	"MatchValue":       "expr",
	"MatchSingleton":   "constant",
	"MatchSequence":    "pattern",
	"MatchMapping":     "expr,pattern,identifier",
	"MatchClass":       "expr,pattern,identifier,pattern",
	"MatchStar":        "identifier",
	"MatchAs":          "pattern,identifier",
	"MatchOr":          "pattern",
	"TypeIgnore":       "int,string",
	"TypeVar":          "identifier,expr,expr",
	"ParamSpec":        "identifier,expr",
	"TypeVarTuple":     "identifier,expr",
}
