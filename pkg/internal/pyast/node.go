package pyast

import (
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type fieldKind uint8

const (
	kNode fieldKind = iota
	kOptNode
	kNodeList
	kIdent
	kOptStr
	kIdentList
	kConst
	kInt
	kOptInt
)

type fieldSpec struct {
	name string
	kind fieldKind
}

type nodeSpec struct {
	name, base string
	fields     []fieldSpec
	attrs      bool
}

// Node is one ast.AST instance with its fields in _fields order and its location attributes
type Node struct {
	Type                                       string
	Lineno, ColOffset, EndLineno, EndColOffset int
	vals                                       []any
	spec                                       *nodeSpec
}

// Ellipsis is the ... constant
type Ellipsis struct{}

// PyRepr renders the constant as Python prints it
func (Ellipsis) PyRepr() string { return "Ellipsis" }

// Complex is a complex constant such as an imaginary literal
type Complex struct{ Real, Imag float64 }

// PyRepr renders the number as complex.__repr__
func (c Complex) PyRepr() string {
	if c.Real == 0 && !math.Signbit(c.Real) {
		return complexPart(c.Imag) + "j"
	}
	im := complexPart(c.Imag)
	if !strings.HasPrefix(im, "-") && !strings.HasPrefix(im, "+") {
		im = "+" + im
	}
	if math.IsNaN(c.Imag) {
		im = "+nan"
	}
	return "(" + complexPart(c.Real) + im + "j)"
}

// Float repr without the forced trailing .0, as the r format of complex parts
func complexPart(f float64) string {
	return strings.TrimSuffix(pyrepr.Float(f), ".0")
}

var schemaRows = [][4]string{
	{"Module", "mod", "body:n*,type_ignores:n*", "false"},
	{"Interactive", "mod", "body:n*", "false"},
	{"Expression", "mod", "body:n", "false"},
	{"FunctionType", "mod", "argtypes:n*,returns:n", "false"},
	{"FunctionDef", "stmt", "name:i,args:n,body:n*,decorator_list:n*,returns:n?,type_comment:s?,type_params:n*", "true"},
	{"AsyncFunctionDef", "stmt", "name:i,args:n,body:n*,decorator_list:n*,returns:n?,type_comment:s?,type_params:n*", "true"},
	{"ClassDef", "stmt", "name:i,bases:n*,keywords:n*,body:n*,decorator_list:n*,type_params:n*", "true"},
	{"Return", "stmt", "value:n?", "true"},
	{"Delete", "stmt", "targets:n*", "true"},
	{"Assign", "stmt", "targets:n*,value:n,type_comment:s?", "true"},
	{"TypeAlias", "stmt", "name:n,type_params:n*,value:n", "true"},
	{"AugAssign", "stmt", "target:n,op:n,value:n", "true"},
	{"AnnAssign", "stmt", "target:n,annotation:n,value:n?,simple:d", "true"},
	{"For", "stmt", "target:n,iter:n,body:n*,orelse:n*,type_comment:s?", "true"},
	{"AsyncFor", "stmt", "target:n,iter:n,body:n*,orelse:n*,type_comment:s?", "true"},
	{"While", "stmt", "test:n,body:n*,orelse:n*", "true"},
	{"If", "stmt", "test:n,body:n*,orelse:n*", "true"},
	{"With", "stmt", "items:n*,body:n*,type_comment:s?", "true"},
	{"AsyncWith", "stmt", "items:n*,body:n*,type_comment:s?", "true"},
	{"Match", "stmt", "subject:n,cases:n*", "true"},
	{"Raise", "stmt", "exc:n?,cause:n?", "true"},
	{"Try", "stmt", "body:n*,handlers:n*,orelse:n*,finalbody:n*", "true"},
	{"TryStar", "stmt", "body:n*,handlers:n*,orelse:n*,finalbody:n*", "true"},
	{"Assert", "stmt", "test:n,msg:n?", "true"},
	{"Import", "stmt", "names:n*", "true"},
	{"ImportFrom", "stmt", "module:s?,names:n*,level:d?", "true"},
	{"Global", "stmt", "names:i*", "true"},
	{"Nonlocal", "stmt", "names:i*", "true"},
	{"Expr", "stmt", "value:n", "true"},
	{"Pass", "stmt", "", "true"},
	{"Break", "stmt", "", "true"},
	{"Continue", "stmt", "", "true"},
	{"BoolOp", "expr", "op:n,values:n*", "true"},
	{"NamedExpr", "expr", "target:n,value:n", "true"},
	{"BinOp", "expr", "left:n,op:n,right:n", "true"},
	{"UnaryOp", "expr", "op:n,operand:n", "true"},
	{"Lambda", "expr", "args:n,body:n", "true"},
	{"IfExp", "expr", "test:n,body:n,orelse:n", "true"},
	{"Dict", "expr", "keys:n*,values:n*", "true"},
	{"Set", "expr", "elts:n*", "true"},
	{"ListComp", "expr", "elt:n,generators:n*", "true"},
	{"SetComp", "expr", "elt:n,generators:n*", "true"},
	{"DictComp", "expr", "key:n,value:n,generators:n*", "true"},
	{"GeneratorExp", "expr", "elt:n,generators:n*", "true"},
	{"Await", "expr", "value:n", "true"},
	{"Yield", "expr", "value:n?", "true"},
	{"YieldFrom", "expr", "value:n", "true"},
	{"Compare", "expr", "left:n,ops:n*,comparators:n*", "true"},
	{"Call", "expr", "func:n,args:n*,keywords:n*", "true"},
	{"FormattedValue", "expr", "value:n,conversion:d,format_spec:n?", "true"},
	{"Interpolation", "expr", "value:n,str:c,conversion:d,format_spec:n?", "true"},
	{"JoinedStr", "expr", "values:n*", "true"},
	{"TemplateStr", "expr", "values:n*", "true"},
	{"Constant", "expr", "value:c,kind:s?", "true"},
	{"Attribute", "expr", "value:n,attr:i,ctx:n", "true"},
	{"Subscript", "expr", "value:n,slice:n,ctx:n", "true"},
	{"Starred", "expr", "value:n,ctx:n", "true"},
	{"Name", "expr", "id:i,ctx:n", "true"},
	{"List", "expr", "elts:n*,ctx:n", "true"},
	{"Tuple", "expr", "elts:n*,ctx:n", "true"},
	{"Slice", "expr", "lower:n?,upper:n?,step:n?", "true"},
	{"Load", "expr_context", "", "false"},
	{"Store", "expr_context", "", "false"},
	{"Del", "expr_context", "", "false"},
	{"And", "boolop", "", "false"},
	{"Or", "boolop", "", "false"},
	{"Add", "operator", "", "false"},
	{"Sub", "operator", "", "false"},
	{"Mult", "operator", "", "false"},
	{"MatMult", "operator", "", "false"},
	{"Div", "operator", "", "false"},
	{"Mod", "operator", "", "false"},
	{"Pow", "operator", "", "false"},
	{"LShift", "operator", "", "false"},
	{"RShift", "operator", "", "false"},
	{"BitOr", "operator", "", "false"},
	{"BitXor", "operator", "", "false"},
	{"BitAnd", "operator", "", "false"},
	{"FloorDiv", "operator", "", "false"},
	{"Invert", "unaryop", "", "false"},
	{"Not", "unaryop", "", "false"},
	{"UAdd", "unaryop", "", "false"},
	{"USub", "unaryop", "", "false"},
	{"Eq", "cmpop", "", "false"},
	{"NotEq", "cmpop", "", "false"},
	{"Lt", "cmpop", "", "false"},
	{"LtE", "cmpop", "", "false"},
	{"Gt", "cmpop", "", "false"},
	{"GtE", "cmpop", "", "false"},
	{"Is", "cmpop", "", "false"},
	{"IsNot", "cmpop", "", "false"},
	{"In", "cmpop", "", "false"},
	{"NotIn", "cmpop", "", "false"},
	{"ExceptHandler", "excepthandler", "type:n?,name:s?,body:n*", "true"},
	{"MatchValue", "pattern", "value:n", "true"},
	{"MatchSingleton", "pattern", "value:c", "true"},
	{"MatchSequence", "pattern", "patterns:n*", "true"},
	{"MatchMapping", "pattern", "keys:n*,patterns:n*,rest:s?", "true"},
	{"MatchClass", "pattern", "cls:n,patterns:n*,kwd_attrs:i*,kwd_patterns:n*", "true"},
	{"MatchStar", "pattern", "name:s?", "true"},
	{"MatchAs", "pattern", "pattern:n?,name:s?", "true"},
	{"MatchOr", "pattern", "patterns:n*", "true"},
	{"TypeIgnore", "type_ignore", "lineno:d,tag:i", "false"},
	{"TypeVar", "type_param", "name:i,bound:n?,default_value:n?", "true"},
	{"ParamSpec", "type_param", "name:i,default_value:n?", "true"},
	{"TypeVarTuple", "type_param", "name:i,default_value:n?", "true"},
	{"comprehension", "comprehension", "target:n,iter:n,ifs:n*,is_async:d", "false"},
	{"arguments", "arguments", "posonlyargs:n*,args:n*,vararg:n?,kwonlyargs:n*,kw_defaults:n*,kwarg:n?,defaults:n*", "false"},
	{"arg", "arg", "arg:i,annotation:n?,type_comment:s?", "true"},
	{"keyword", "keyword", "arg:s?,value:n", "true"},
	{"alias", "alias", "name:i,asname:s?", "true"},
	{"withitem", "withitem", "context_expr:n,optional_vars:n?", "false"},
	{"match_case", "match_case", "pattern:n,guard:n?,body:n*", "false"},
}

var specs = buildSpecs()

// Field tables of every concrete node class from the schema rows
func buildSpecs() map[string]*nodeSpec {
	specs := map[string]*nodeSpec{}
	kinds := map[string]fieldKind{"n": kNode, "n?": kOptNode, "n*": kNodeList, "i": kIdent, "s?": kOptStr, "i*": kIdentList, "c": kConst, "d": kInt, "d?": kOptInt}
	for _, r := range schemaRows {
		s := &nodeSpec{name: r[0], base: r[1], attrs: r[3] == "true"}
		if r[2] != "" {
			for _, f := range strings.Split(r[2], ",") {
				name, k, _ := strings.Cut(f, ":")
				s.fields = append(s.fields, fieldSpec{name, kinds[k]})
			}
		}
		specs[s.name] = s
	}
	return specs
}

// NewNode builds a node of the named class from field values given in _fields order
func NewNode(typ string, vals ...any) *Node {
	s := specs[typ]
	if s == nil {
		panic("pyast: unknown node type " + typ)
	}
	if len(vals) != len(s.fields) {
		panic(fmt.Sprintf("pyast: %s takes %d fields, got %d", typ, len(s.fields), len(vals)))
	}
	for i, f := range s.fields {
		if vals[i] == nil {
			switch f.kind {
			case kNodeList:
				vals[i] = []*Node{}
			case kIdentList:
				vals[i] = []string{}
			}
		}
	}
	return &Node{Type: typ, vals: vals, spec: s}
}

// Base names the abstract ASDL class of the node such as stmt, expr or pattern
func (n *Node) Base() string { return n.spec.base }

// HasLocation reports whether the class carries lineno and col_offset attributes
func (n *Node) HasLocation() bool { return n.spec.attrs }

// Fields lists the _fields names in order
func (n *Node) Fields() []string {
	out := make([]string, len(n.spec.fields))
	for i, f := range n.spec.fields {
		out[i] = f.name
	}
	return out
}

func (n *Node) index(name string) int {
	for i, f := range n.spec.fields {
		if f.name == name {
			return i
		}
	}
	return -1
}

// Get returns a field value, nil when the class has no such field or the field holds None
func (n *Node) Get(name string) any {
	if n == nil {
		return nil
	}
	if i := n.index(name); i >= 0 {
		return n.vals[i]
	}
	return nil
}

// Set replaces a field value
func (n *Node) Set(name string, v any) {
	if i := n.index(name); i >= 0 {
		n.vals[i] = v
		return
	}
	panic("pyast: " + n.Type + " has no field " + name)
}

// Child returns a node valued field or nil
func (n *Node) Child(name string) *Node {
	c, _ := n.Get(name).(*Node)
	return c
}

// Children returns a list of nodes field or nil
func (n *Node) Children(name string) []*Node {
	c, _ := n.Get(name).([]*Node)
	return c
}

// Str returns an identifier or string field and whether it is set
func (n *Node) Str(name string) (string, bool) {
	s, ok := n.Get(name).(string)
	return s, ok
}

// Names returns an identifier list field
func (n *Node) Names(name string) []string {
	s, _ := n.Get(name).([]string)
	return s
}

// Int returns an int valued field and whether it is set
func (n *Node) Int(name string) (int, bool) {
	v, ok := n.Get(name).(int)
	return v, ok
}

// Is reports whether the node exists and has one of the class names
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

// IterChildNodes yields direct child nodes in field order like ast.iter_child_nodes
func IterChildNodes(n *Node) []*Node {
	var out []*Node
	for _, val := range n.vals {
		switch v := val.(type) {
		case *Node:
			if v != nil {
				out = append(out, v)
			}
		case []*Node:
			for _, c := range v {
				if c != nil {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// Walk yields every node of the tree breadth first like ast.walk
func Walk(n *Node) []*Node {
	queue := []*Node{n}
	for i := 0; i < len(queue); i++ {
		queue = append(queue, IterChildNodes(queue[i])...)
	}
	return queue
}

// DocstringError is the TypeError get_docstring raises for a node that cannot hold a docstring
type DocstringError struct{ Type string }

func (e *DocstringError) Error() string { return pyrepr.StrRepr(e.Type) + " can't have docstrings" }

// GetDocstring returns the docstring of a module, class or function, cleaned like inspect.cleandoc when clean is set
func GetDocstring(n *Node, clean bool) (string, bool, error) {
	if !n.Is("AsyncFunctionDef", "FunctionDef", "ClassDef", "Module") {
		return "", false, &DocstringError{n.Type}
	}
	body := n.Children("body")
	if len(body) == 0 || body[0].Type != "Expr" {
		return "", false, nil
	}
	v := body[0].Child("value")
	if !v.Is("Constant") {
		return "", false, nil
	}
	text, ok := v.Get("value").(string)
	if !ok {
		return "", false, nil
	}
	if clean {
		text = Cleandoc(text)
	}
	return text, true, nil
}

// Cleandoc strips the uniform indentation and blank edge lines of a docstring as inspect.cleandoc
func Cleandoc(doc string) string {
	lines := strings.Split(expandTabs(doc), "\n")
	margin := math.MaxInt
	for _, line := range lines[1:] {
		content := pystr.Len(strings.TrimLeft(line, " "))
		if content > 0 {
			margin = min(margin, pystr.Len(line)-content)
		}
	}
	lines[0] = strings.TrimLeft(lines[0], " ")
	if margin < math.MaxInt {
		for i := 1; i < len(lines); i++ {
			r := pystr.Runes(lines[i])
			if margin < len(r) {
				lines[i] = pystr.FromRunes(r[margin:])
			} else {
				lines[i] = ""
			}
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// Replaces tabs with spaces up to the next multiple of eight columns as str.expandtabs
func expandTabs(s string) string {
	var b strings.Builder
	col := 0
	for _, r := range pystr.Runes(s) {
		switch r {
		case '\t':
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case '\n', '\r':
			b.WriteRune(r)
			col = 0
		default:
			pystr.WriteRune(&b, r)
			col++
		}
	}
	return b.String()
}

// Dump renders the tree as ast.dump with annotate_fields and the include_attributes choice
func Dump(n *Node, includeAttributes bool) (string, error) {
	var b strings.Builder
	if err := dumpInto(&b, n, includeAttributes); err != nil {
		return "", err
	}
	return b.String(), nil
}

func dumpInto(b *strings.Builder, n *Node, attrs bool) error {
	b.WriteString(n.Type)
	b.WriteByte('(')
	first := true
	sep := func() {
		if !first {
			b.WriteString(", ")
		}
		first = false
	}
	for i, f := range n.spec.fields {
		v := n.vals[i]
		switch f.kind {
		case kOptNode, kOptStr, kOptInt:
			if v == nil || v == (*Node)(nil) {
				continue
			}
		case kNodeList:
			if len(v.([]*Node)) == 0 {
				continue
			}
		case kIdentList:
			if len(v.([]string)) == 0 {
				continue
			}
		}
		sep()
		b.WriteString(f.name)
		b.WriteByte('=')
		if err := dumpValue(b, v, attrs); err != nil {
			return err
		}
	}
	if attrs && n.spec.attrs {
		sep()
		fmt.Fprintf(b, "lineno=%d, col_offset=%d, end_lineno=%d, end_col_offset=%d", n.Lineno, n.ColOffset, n.EndLineno, n.EndColOffset)
	}
	b.WriteByte(')')
	return nil
}

func dumpValue(b *strings.Builder, v any, attrs bool) error {
	switch x := v.(type) {
	case *Node:
		if x == nil {
			b.WriteString("None")
			return nil
		}
		return dumpInto(b, x, attrs)
	case []*Node:
		b.WriteByte('[')
		for i, c := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := dumpValue(b, c, attrs); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyrepr.StrRepr(s))
		}
		b.WriteByte(']')
		return nil
	}
	s, err := ConstantRepr(v)
	if err != nil {
		return err
	}
	b.WriteString(s)
	return nil
}

// IntDigitsError is the ValueError CPython raises converting an int over 4300 decimal digits to text
type IntDigitsError struct{}

func (e *IntDigitsError) Error() string {
	return "Exceeds the limit (4300 digits) for integer string conversion; use sys.set_int_max_str_digits() to increase the limit"
}

var maxStrDigitsBound = new(big.Int).Exp(big.NewInt(10), big.NewInt(4300), nil)

// ConstantRepr renders a constant value with repr, raising the int digit limit error CPython raises
func ConstantRepr(v any) (string, error) {
	switch x := v.(type) {
	case *big.Int:
		if new(big.Int).Abs(x).Cmp(maxStrDigitsBound) >= 0 {
			return "", &IntDigitsError{}
		}
		return x.String(), nil
	case int:
		return fmt.Sprint(x), nil
	case float64:
		return pyrepr.Float(x), nil
	}
	return pyrepr.Repr(v), nil
}
