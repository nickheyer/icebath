package pythonutil

import (
	"iter"
	"strings"
	"sync"
)

type fieldKind uint8

const (
	fieldNode fieldKind = iota
	fieldOptNode
	fieldNodeList
	fieldIdent
	fieldOptStr
	fieldIdentList
	fieldConst
	fieldInt
	fieldOptInt
)

type fieldSpec struct {
	name string
	kind fieldKind
}

type nodeSpec struct {
	fields  []fieldSpec
	located bool
}

// One syntax tree node: Type is the class name the Python ast module uses, lines count from 1 and columns are 0-based UTF-8 byte offsets, all zero for a node without a position
type Node struct {
	Type                       string
	Line, Col, EndLine, EndCol int
	vals                       []any
	spec                       *nodeSpec
}

// Value of the ... constant
type Ellipsis struct{}

// Class name, fields as name:kind and whether the class carries a position, kinds n node, n? optional node, n* node list, i identifier, s? optional string, i* identifier list, c constant, d int, d? optional int
var schemaRows = [...][3]string{
	{"Module", "body:n*,type_ignores:n*", "false"},
	{"Expression", "body:n", "false"},
	{"FunctionDef", "name:i,args:n,body:n*,decorator_list:n*,returns:n?,type_comment:s?,type_params:n*", "true"},
	{"AsyncFunctionDef", "name:i,args:n,body:n*,decorator_list:n*,returns:n?,type_comment:s?,type_params:n*", "true"},
	{"ClassDef", "name:i,bases:n*,keywords:n*,body:n*,decorator_list:n*,type_params:n*", "true"},
	{"Return", "value:n?", "true"},
	{"Delete", "targets:n*", "true"},
	{"Assign", "targets:n*,value:n,type_comment:s?", "true"},
	{"TypeAlias", "name:n,type_params:n*,value:n", "true"},
	{"AugAssign", "target:n,op:n,value:n", "true"},
	{"AnnAssign", "target:n,annotation:n,value:n?,simple:d", "true"},
	{"For", "target:n,iter:n,body:n*,orelse:n*,type_comment:s?", "true"},
	{"AsyncFor", "target:n,iter:n,body:n*,orelse:n*,type_comment:s?", "true"},
	{"While", "test:n,body:n*,orelse:n*", "true"},
	{"If", "test:n,body:n*,orelse:n*", "true"},
	{"With", "items:n*,body:n*,type_comment:s?", "true"},
	{"AsyncWith", "items:n*,body:n*,type_comment:s?", "true"},
	{"Match", "subject:n,cases:n*", "true"},
	{"Raise", "exc:n?,cause:n?", "true"},
	{"Try", "body:n*,handlers:n*,orelse:n*,finalbody:n*", "true"},
	{"TryStar", "body:n*,handlers:n*,orelse:n*,finalbody:n*", "true"},
	{"Assert", "test:n,msg:n?", "true"},
	{"Import", "names:n*", "true"},
	{"ImportFrom", "module:s?,names:n*,level:d?", "true"},
	{"Global", "names:i*", "true"},
	{"Nonlocal", "names:i*", "true"},
	{"Expr", "value:n", "true"},
	{"Pass", "", "true"},
	{"Break", "", "true"},
	{"Continue", "", "true"},
	{"BoolOp", "op:n,values:n*", "true"},
	{"NamedExpr", "target:n,value:n", "true"},
	{"BinOp", "left:n,op:n,right:n", "true"},
	{"UnaryOp", "op:n,operand:n", "true"},
	{"Lambda", "args:n,body:n", "true"},
	{"IfExp", "test:n,body:n,orelse:n", "true"},
	{"Dict", "keys:n*,values:n*", "true"},
	{"Set", "elts:n*", "true"},
	{"ListComp", "elt:n,generators:n*", "true"},
	{"SetComp", "elt:n,generators:n*", "true"},
	{"DictComp", "key:n,value:n,generators:n*", "true"},
	{"GeneratorExp", "elt:n,generators:n*", "true"},
	{"Await", "value:n", "true"},
	{"Yield", "value:n?", "true"},
	{"YieldFrom", "value:n", "true"},
	{"Compare", "left:n,ops:n*,comparators:n*", "true"},
	{"Call", "func:n,args:n*,keywords:n*", "true"},
	{"FormattedValue", "value:n,conversion:d,format_spec:n?", "true"},
	{"Interpolation", "value:n,str:c,conversion:d,format_spec:n?", "true"},
	{"JoinedStr", "values:n*", "true"},
	{"TemplateStr", "values:n*", "true"},
	{"Constant", "value:c,kind:s?", "true"},
	{"Attribute", "value:n,attr:i,ctx:n", "true"},
	{"Subscript", "value:n,slice:n,ctx:n", "true"},
	{"Starred", "value:n,ctx:n", "true"},
	{"Name", "id:i,ctx:n", "true"},
	{"List", "elts:n*,ctx:n", "true"},
	{"Tuple", "elts:n*,ctx:n", "true"},
	{"Slice", "lower:n?,upper:n?,step:n?", "true"},
	{"ExceptHandler", "type:n?,name:s?,body:n*", "true"},
	{"MatchValue", "value:n", "true"},
	{"MatchSingleton", "value:c", "true"},
	{"MatchSequence", "patterns:n*", "true"},
	{"MatchMapping", "keys:n*,patterns:n*,rest:s?", "true"},
	{"MatchClass", "cls:n,patterns:n*,kwd_attrs:i*,kwd_patterns:n*", "true"},
	{"MatchStar", "name:s?", "true"},
	{"MatchAs", "pattern:n?,name:s?", "true"},
	{"MatchOr", "patterns:n*", "true"},
	{"TypeVar", "name:i,bound:n?,default_value:n?", "true"},
	{"ParamSpec", "name:i,default_value:n?", "true"},
	{"TypeVarTuple", "name:i,default_value:n?", "true"},
	{"comprehension", "target:n,iter:n,ifs:n*,is_async:d", "false"},
	{"arguments", "posonlyargs:n*,args:n*,vararg:n?,kwonlyargs:n*,kw_defaults:n*,kwarg:n?,defaults:n*", "false"},
	{"arg", "arg:i,annotation:n?,type_comment:s?", "true"},
	{"keyword", "arg:s?,value:n", "true"},
	{"alias", "name:i,asname:s?", "true"},
	{"withitem", "context_expr:n,optional_vars:n?", "false"},
	{"match_case", "pattern:n,guard:n?,body:n*", "false"},
}

// Field tables of every class that carries fields, built on first use
var specs = sync.OnceValue(func() map[string]*nodeSpec {
	out := make(map[string]*nodeSpec, len(schemaRows))
	for _, r := range schemaRows {
		s := &nodeSpec{located: r[2] == "true"}
		for f := range strings.SplitSeq(r[1], ",") {
			if f == "" {
				continue
			}
			name, k, _ := strings.Cut(f, ":")
			s.fields = append(s.fields, fieldSpec{name, fieldKindOf(k)})
		}
		out[r[0]] = s
	}
	return out
})

func fieldKindOf(code string) fieldKind {
	switch code {
	case "n":
		return fieldNode
	case "n?":
		return fieldOptNode
	case "n*":
		return fieldNodeList
	case "i":
		return fieldIdent
	case "s?":
		return fieldOptStr
	case "i*":
		return fieldIdentList
	case "c":
		return fieldConst
	case "d":
		return fieldInt
	}
	return fieldOptInt
}

// Spec of the classes without fields or position: expression contexts and operators
var leafSpec nodeSpec

var (
	ctxLoad  = &Node{Type: "Load", spec: &leafSpec}
	ctxStore = &Node{Type: "Store", spec: &leafSpec}
	ctxDel   = &Node{Type: "Del", spec: &leafSpec}
)

// Shared operator nodes, the most frequent first
var operators = [...]Node{
	{Type: "Add", spec: &leafSpec}, {Type: "Eq", spec: &leafSpec}, {Type: "Sub", spec: &leafSpec},
	{Type: "Mult", spec: &leafSpec}, {Type: "Not", spec: &leafSpec}, {Type: "And", spec: &leafSpec},
	{Type: "Or", spec: &leafSpec}, {Type: "In", spec: &leafSpec}, {Type: "Is", spec: &leafSpec},
	{Type: "IsNot", spec: &leafSpec}, {Type: "NotEq", spec: &leafSpec}, {Type: "USub", spec: &leafSpec},
	{Type: "NotIn", spec: &leafSpec}, {Type: "Mod", spec: &leafSpec}, {Type: "Div", spec: &leafSpec},
	{Type: "Lt", spec: &leafSpec}, {Type: "Gt", spec: &leafSpec}, {Type: "LtE", spec: &leafSpec},
	{Type: "GtE", spec: &leafSpec}, {Type: "BitOr", spec: &leafSpec}, {Type: "FloorDiv", spec: &leafSpec},
	{Type: "Pow", spec: &leafSpec}, {Type: "BitAnd", spec: &leafSpec}, {Type: "MatMult", spec: &leafSpec},
	{Type: "LShift", spec: &leafSpec}, {Type: "RShift", spec: &leafSpec}, {Type: "BitXor", spec: &leafSpec},
	{Type: "Invert", spec: &leafSpec}, {Type: "UAdd", spec: &leafSpec},
}

// Shared node of an operator class
func opNode(name string) *Node {
	for i := range operators {
		if operators[i].Type == name {
			return &operators[i]
		}
	}
	panic("pythonutil: unknown operator " + name)
}

// Builds a node of the named class from its field values in field order, empty lists standing in for unset list fields
func newNode(typ string, vals ...any) *Node {
	s := specs()[typ]
	if s == nil {
		panic("pythonutil: unknown node type " + typ)
	}
	if len(vals) != len(s.fields) {
		panic("pythonutil: wrong field count for " + typ)
	}
	for i, f := range s.fields {
		if vals[i] == nil {
			switch f.kind {
			case fieldNodeList:
				vals[i] = []*Node{}
			case fieldIdentList:
				vals[i] = []string{}
			}
		}
	}
	return &Node{Type: typ, vals: vals, spec: s}
}

func (n *Node) index(name string) int {
	if n.spec == nil {
		return -1
	}
	for i, f := range n.spec.fields {
		if f.name == name {
			return i
		}
	}
	return -1
}

// Replaces a field value, panicking for a field the class does not have
func (n *Node) set(name string, v any) {
	if i := n.index(name); i >= 0 {
		n.vals[i] = v
		return
	}
	panic("pythonutil: " + n.Type + " has no field " + name)
}

// Value of a field, nil when absent or None; a Constant value is a bool, int64, *big.Int past int64, float64, complex128, string, []byte or Ellipsis
func (n *Node) Get(name string) any {
	if n == nil {
		return nil
	}
	if i := n.index(name); i >= 0 {
		return n.vals[i]
	}
	return nil
}

// Node a field holds, nil when the field is unset or holds no node
func (n *Node) Child(name string) *Node {
	c, _ := n.Get(name).(*Node)
	return c
}

// Nodes of a list field, nil when the field holds no node list
func (n *Node) Children(name string) []*Node {
	c, _ := n.Get(name).([]*Node)
	return c
}

// Identifier or string of a field and whether it is set
func (n *Node) Str(name string) (string, bool) {
	s, ok := n.Get(name).(string)
	return s, ok
}

// Identifiers of a list field such as the names of a global statement
func (n *Node) Names(name string) []string {
	s, _ := n.Get(name).([]string)
	return s
}

// Integer of a field such as an import level and whether it is set
func (n *Node) Int(name string) (int, bool) {
	v, ok := n.Get(name).(int)
	return v, ok
}

// Whether the node exists and is of one of the classes
func (n *Node) Is(types ...string) bool {
	if n == nil {
		return false
	}
	for _, t := range types {
		if n.Type == t {
			return true
		}
	}
	return false
}

// Appends the direct child nodes of n in field order
func appendChildNodes(dst []*Node, n *Node) []*Node {
	for _, val := range n.vals {
		switch v := val.(type) {
		case *Node:
			if v != nil {
				dst = append(dst, v)
			}
		case []*Node:
			for _, c := range v {
				if c != nil {
					dst = append(dst, c)
				}
			}
		}
	}
	return dst
}

// Direct child nodes in field order, contexts and operators included
func ChildNodes(n *Node) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if n == nil {
			return
		}
		for _, val := range n.vals {
			switch v := val.(type) {
			case *Node:
				if v != nil && !yield(v) {
					return
				}
			case []*Node:
				for _, c := range v {
					if c != nil && !yield(c) {
						return
					}
				}
			}
		}
	}
}

// Every node of the tree breadth first, the root first
func Walk(n *Node) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if n == nil {
			return
		}
		queue := []*Node{n}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if !yield(cur) {
				return
			}
			queue = appendChildNodes(queue, cur)
		}
	}
}

// Deepest nesting a tree may reach, past which parsing fails so every recursive walk over a tree stays bounded
const maxDepth = 50_000

type depthEntry struct {
	n           *Node
	depth, line int
}

// Nil when no path from the root holds more than maxDepth nodes, else the limit error at the first node found past it
func checkDepth(root *Node) error {
	stack := make([]depthEntry, 1, 64)
	stack[0] = depthEntry{root, 1, 0}
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		line := e.line
		if e.n.Line > 0 {
			line = e.n.Line
		}
		if e.depth > maxDepth {
			return &LimitError{Line: line, Limit: maxDepth}
		}
		deep := e.depth >= maxDepth
		for _, val := range e.n.vals {
			switch v := val.(type) {
			case *Node:
				if v != nil && (len(v.vals) > 0 || deep) {
					stack = append(stack, depthEntry{v, e.depth + 1, line})
				}
			case []*Node:
				for _, c := range v {
					if c != nil && (len(c.vals) > 0 || deep) {
						stack = append(stack, depthEntry{c, e.depth + 1, line})
					}
				}
			}
		}
	}
	return nil
}
