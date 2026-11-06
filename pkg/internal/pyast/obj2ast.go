package pyast

import (
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bytes of C stack each obj2ast function of Python-ast.c takes in CPython 3.14.8 on this host, type_ignore inlined into mod
var obj2astFrames = map[string]int{
	"mod": 224, "stmt": 208, "expr": 192, "pattern": 160, "excepthandler": 176, "type_param": 176, "type_ignore": 0,
	"comprehension": 144, "arguments": 176, "arg": 176, "keyword": 128, "alias": 128, "withitem": 96, "match_case": 160,
}

// Soft stack budget of obj2ast and validate below the is_ast part of builtin_compile_impl, fit from node_recursion_rows.json.gz
var nodeStack = struct{ obj2ast, validate, kB int }{obj2ast: 8344896, validate: 8344896, kB: 8152}

type obj2ast struct {
	limit, kB int
	seen      func(x any, d int)
}

func (c *obj2ast) check(d int, owner string) error {
	if d >= c.limit {
		return pyErr("RecursionError", "Stack overflow (used %d kB) while traversing '%s' node", c.kB, owner)
	}
	return nil
}

func simpleSum(typ string) bool {
	switch typ {
	case "expr_context", "boolop", "operator", "unaryop", "cmpop":
		return true
	}
	return false
}

func complexSum(typ string) bool {
	switch typ {
	case "mod", "stmt", "expr", "excepthandler", "pattern", "type_ignore", "type_param":
		return true
	}
	return false
}

// Converts an int object to a C int as PyLong_AsInt, bool included
func toCInt(x any) (int, error) {
	if b, ok := x.(bool); ok {
		if b {
			return 1, nil
		}
		return 0, nil
	}
	v, ok := pyrepr.Int(x)
	if !ok {
		return 0, reprErr("ValueError", "invalid integer value: %s", x)
	}
	if !v.IsInt64() || v.Int64() < math.MinInt32 || v.Int64() > math.MaxInt32 {
		return 0, pyErr("OverflowError", "Python int too large to convert to C int")
	}
	return int(v.Int64()), nil
}

// Converts one field value by its ASDL type as the obj2ast helpers do
func (c *obj2ast) value(typ string, x any, d int) (any, error) {
	switch typ {
	case "identifier":
		if s, ok := x.(string); ok {
			return s, nil
		}
		if isNone(x) {
			return nil, nil
		}
		return nil, pyErr("TypeError", "AST identifier must be of type str")
	case "string":
		switch x.(type) {
		case string, []byte:
			return x, nil
		}
		return nil, pyErr("TypeError", "AST string must be of type str")
	case "constant":
		return normConst(x), nil
	case "int":
		return toCInt(x)
	}
	if simpleSum(typ) {
		if n, ok := x.(*Node); ok && n != nil && n.spec.base == typ {
			return NewNode(n.Type), nil
		}
		return nil, reprErr("TypeError", "expected some sort of "+typ+", but got %s", x)
	}
	var n *Node
	var err error
	if complexSum(typ) {
		n, err = c.sum(typ, x, d)
	} else {
		n, err = c.product(typ, x, d)
	}
	if n == nil {
		return nil, err
	}
	return n, err
}

// Keeps a constant object, Go integer kinds becoming the big.Int form the parser and folder use for Python ints
func normConst(x any) any {
	switch v := x.(type) {
	case bool:
		return v
	case pyrepr.Tuple:
		out := make(pyrepr.Tuple, len(v))
		for i, it := range v {
			out[i] = normConst(it)
		}
		return out
	case FrozenSet:
		out := make(FrozenSet, len(v))
		for i, it := range v {
			out[i] = normConst(it)
		}
		return out
	}
	if isNone(x) {
		return nil
	}
	if b, ok := pyrepr.Int(x); ok {
		return b
	}
	return x
}

// Reads the location attributes, the end pair defaulting to the start unless the ASDL requires it
func (c *obj2ast) loc(x any, owner string, endRequired bool, d int) ([4]int, error) {
	var v [4]int
	for i, a := range [...]string{"lineno", "col_offset", "end_lineno", "end_col_offset"} {
		y := getattr(x, a)
		_, gone := y.(Missing)
		if i >= 2 && !endRequired && (gone || isNone(y)) {
			v[i] = v[i-2]
			continue
		}
		if gone {
			return v, pyErr("TypeError", "required field \"%s\" missing from %s", a, owner)
		}
		if err := c.check(d, owner); err != nil {
			return v, err
		}
		n, err := toCInt(y)
		if err != nil {
			return v, err
		}
		v[i] = n
	}
	return v, nil
}

// Converts every field of a constructor or product in _fields order
func (c *obj2ast) fields(src any, spec *nodeSpec, d int) ([]any, error) {
	owner := spec.name
	vals := make([]any, len(spec.fields))
	for i, f := range spec.fields {
		x := getattr(src, f.name)
		_, gone := x.(Missing)
		switch f.kind {
		case kNodeList, kIdentList:
			var items []any
			if !gone {
				var ok bool
				if items, ok = listItems(x); !ok {
					return nil, pyErr("TypeError", "%s field \"%s\" must be a list, not a %.200s", owner, f.name, typeName(x))
				}
			}
			conv := make([]any, len(items))
			for j, it := range items {
				if err := c.check(d, owner); err != nil {
					return nil, err
				}
				v, err := c.value(f.typ, it, d)
				if err != nil {
					return nil, err
				}
				conv[j] = v
			}
			vals[i] = seqValue(f.kind, conv)
		default:
			if (f.kind == kOptNode || f.kind == kOptStr || f.kind == kOptInt) && (gone || isNone(x)) {
				if f.kind == kOptInt {
					vals[i] = 0
				}
				continue
			}
			if gone {
				return nil, pyErr("TypeError", "required field \"%s\" missing from %s", f.name, owner)
			}
			if err := c.check(d, owner); err != nil {
				return nil, err
			}
			v, err := c.value(f.typ, x, d)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
	}
	return vals, nil
}

// Builds the Go list of a converted sequence, a list with None identifiers staying a generic list
func seqValue(k fieldKind, conv []any) any {
	if k == kNodeList {
		nodes := make([]*Node, len(conv))
		for j, v := range conv {
			nodes[j], _ = v.(*Node)
		}
		return nodes
	}
	names := make([]string, len(conv))
	for j, v := range conv {
		s, ok := v.(string)
		if !ok {
			return conv
		}
		names[j] = s
	}
	return names
}

// Raises the ValueError the _PyAST constructors raise for a required node or identifier left NULL
func construct(spec *nodeSpec, vals []any, loc [4]int) (*Node, error) {
	for i, f := range spec.fields {
		if vals[i] == nil && (f.kind == kNode && !simpleSum(f.typ) || f.kind == kIdent && f.typ == "identifier") {
			return nil, pyErr("ValueError", "field '%s' is required for %s", f.name, spec.name)
		}
	}
	n := NewNode(spec.name, vals...)
	n.Lineno, n.ColOffset, n.EndLineno, n.EndColOffset = loc[0], loc[1], loc[2], loc[3]
	return n, nil
}

// Converts a sum type object, attributes first, then the class check, then the constructor fields
func (c *obj2ast) sum(base string, x any, p int) (*Node, error) {
	if isNone(x) {
		return nil, nil
	}
	d := p + obj2astFrames[base]
	if c.seen != nil {
		c.seen(x, d)
	}
	var loc [4]int
	switch base {
	case "stmt", "expr", "excepthandler", "pattern", "type_param":
		var err error
		if loc, err = c.loc(x, base, base == "pattern" || base == "type_param", d); err != nil {
			return nil, err
		}
	}
	n, ok := x.(*Node)
	if !ok || n.spec.base != base {
		return nil, reprErr("TypeError", "expected some sort of "+base+", but got %s", x)
	}
	vals, err := c.fields(n, n.spec, d)
	if err != nil {
		return nil, err
	}
	return construct(n.spec, vals, loc)
}

// Converts a product type object, which may be any object carrying the fields, then its attributes
func (c *obj2ast) product(name string, x any, p int) (*Node, error) {
	d := p + obj2astFrames[name]
	if c.seen != nil {
		c.seen(x, d)
	}
	spec := specs[name]
	vals, err := c.fields(x, spec, d)
	if err != nil {
		return nil, err
	}
	var loc [4]int
	if spec.attrs {
		if loc, err = c.loc(x, name, false, d); err != nil {
			return nil, err
		}
	}
	return construct(spec, vals, loc)
}

// Checks the root class against the mode and converts the tree as PyAST_obj2mod
func obj2mod(n *Node, mode string) (*Node, error) {
	want := map[string]string{"exec": "Module", "eval": "Expression", "single": "Interactive", "func_type": "FunctionType"}[mode]
	if n.Type != want {
		return nil, pyErr("TypeError", "expected %s node, got %.400s", want, n.Type)
	}
	c := &obj2ast{limit: nodeStack.obj2ast, kB: nodeStack.kB}
	return c.sum("mod", n, 0)
}
