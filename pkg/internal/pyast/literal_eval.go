package pyast

import (
	"errors"
	"fmt"
	"iter"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Dict is a literal dict keeping insertion order, its keys compared with Python equality
type Dict struct{ Keys, Values []any }

// PyItems yields the pairs in insertion order for pyrepr
func (d *Dict) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for i := range d.Keys {
			if !yield(d.Keys[i], d.Values[i]) {
				return
			}
		}
	}
}

func (d *Dict) set(k, v any) {
	for i, existing := range d.Keys {
		if pyrepr.Equal(existing, k) {
			d.Values[i] = v
			return
		}
	}
	d.Keys = append(d.Keys, k)
	d.Values = append(d.Values, v)
}

// Set is a literal set holding distinct members in insertion order
type Set []any

// PyRepr renders the set as set.__repr__ does for its members
func (s Set) PyRepr() string {
	if len(s) == 0 {
		return "set()"
	}
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = pyrepr.Repr(v)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// PyTypeName names the Python type for pyrepr
func (Set) PyTypeName() string { return "set" }

// PyTypeName names the Python type for pyrepr
func (*Dict) PyTypeName() string { return "dict" }

// LiteralError is the ValueError or TypeError literal_eval raises for a node that is not a literal
type LiteralError struct{ Class, Msg string }

func (e *LiteralError) Error() string { return e.Msg }

// LiteralEval evaluates a Python literal expression from source text like ast.literal_eval(str)
func LiteralEval(source string) (any, error) {
	tree, err := ParseExpression(strings.TrimLeft(source, " \t"))
	if err != nil {
		return nil, err
	}
	return LiteralEvalNode(tree)
}

// LiteralEvalNode evaluates an expression node, or the body of an Expression, like ast.literal_eval(node)
func LiteralEvalNode(n *Node) (v any, err error) {
	if n.Is("Expression") {
		n = n.Child("body")
	}
	defer func() {
		if r := recover(); r != nil {
			if le, ok := r.(*LiteralError); ok {
				v, err = nil, le
				return
			}
			panic(r)
		}
	}()
	return convertLiteral(n), nil
}

func malformed(n *Node) {
	msg := "malformed node or string"
	if n == nil {
		panic(&LiteralError{"ValueError", msg + ": None"})
	}
	if n.HasLocation() && n.Lineno != 0 {
		msg += fmt.Sprintf(" on line %d", n.Lineno)
	}
	r, err := n.Repr()
	if err != nil {
		panic(&LiteralError{"ValueError", err.Error()})
	}
	panic(&LiteralError{"ValueError", msg + ": " + r})
}

func isNumber(v any) bool {
	switch v.(type) {
	case *big.Int, float64, Complex:
		return true
	}
	return false
}

func convertNum(n *Node) any {
	if n == nil || !n.Is("Constant") || !isNumber(n.Get("value")) {
		malformed(n)
	}
	return n.Get("value")
}

func convertSignedNum(n *Node) any {
	if n.Is("UnaryOp") && n.Child("op").Is("UAdd", "USub") {
		v := convertNum(n.Child("operand"))
		if n.Child("op").Type == "UAdd" {
			return v
		}
		switch x := v.(type) {
		case *big.Int:
			return new(big.Int).Neg(x)
		case float64:
			return -x
		case Complex:
			return Complex{-x.Real, -x.Imag}
		}
	}
	return convertNum(n)
}

// Whether a literal value can be a dict key or set member
func hashable(v any) bool {
	switch x := v.(type) {
	case []any, *Dict, Set:
		return false
	case pyrepr.Tuple:
		for _, e := range x {
			if !hashable(e) {
				return false
			}
		}
	}
	return true
}

// Raises the TypeError dict and set displays raise for an unhashable member
func requireHashable(v any, role string) {
	if !hashable(v) {
		name := pyrepr.TypeName(v)
		panic(&LiteralError{"TypeError", "cannot use '" + name + "' as a " + role + " (unhashable type: '" + name + "')"})
	}
}

func convertLiteral(n *Node) any {
	if n == nil {
		malformed(nil)
	}
	switch n.Type {
	case "Constant":
		return n.Get("value")
	case "Tuple":
		out := pyrepr.Tuple{}
		for _, e := range n.Children("elts") {
			out = append(out, convertLiteral(e))
		}
		return out
	case "List":
		out := []any{}
		for _, e := range n.Children("elts") {
			out = append(out, convertLiteral(e))
		}
		return out
	case "Set":
		out := Set{}
		for _, e := range n.Children("elts") {
			v := convertLiteral(e)
			requireHashable(v, "set element")
			dup := false
			for _, m := range out {
				if pyrepr.Equal(m, v) {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, v)
			}
		}
		return out
	case "Call":
		if f := n.Child("func"); f.Is("Name") && len(n.Children("args")) == 0 && len(n.Children("keywords")) == 0 {
			if id, _ := f.Str("id"); id == "set" {
				return Set{}
			}
		}
	case "Dict":
		keys, vals := n.Children("keys"), n.Children("values")
		if len(keys) != len(vals) {
			malformed(n)
		}
		ks := make([]any, len(keys))
		for i, k := range keys {
			ks[i] = convertLiteral(k)
		}
		vs := make([]any, len(vals))
		for i, v := range vals {
			vs[i] = convertLiteral(v)
		}
		d := &Dict{}
		for i := range ks {
			requireHashable(ks[i], "dict key")
			d.set(ks[i], vs[i])
		}
		return d
	case "BinOp":
		if op := n.Child("op"); op.Is("Add", "Sub") {
			left := convertSignedNum(n.Child("left"))
			right := convertNum(n.Child("right"))
			if rc, ok := right.(Complex); ok {
				var lf float64
				switch l := left.(type) {
				case *big.Int:
					lf, _ = new(big.Float).SetInt(l).Float64()
				case float64:
					lf = l
				default:
					return convertSignedNum(n)
				}
				if op.Type == "Add" {
					return Complex{lf + rc.Real, rc.Imag}
				}
				return Complex{lf - rc.Real, -rc.Imag}
			}
		}
	}
	return convertSignedNum(n)
}

// ParseExpression parses source in eval mode like ast.parse(source, mode='eval'), returning an Expression
func ParseExpression(source string) (*Node, error) {
	n, _, err := ParseWith(source, Options{Mode: "eval"})
	return n, err
}

// Comma separated expressions of eval input, a tuple when a comma appears
func (p *parser) expressions() *Node {
	t := p.cur()
	first := p.expression()
	if !p.isOp(",") {
		return first
	}
	elts := []*Node{first}
	for p.isOp(",") {
		p.next()
		if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "~") {
			break
		}
		elts = append(elts, p.expression())
	}
	return p.mkFrom(t, "Tuple", elts, ctxLoad)
}

// Repr renders the node as the 3.14 AST repr, three levels deep with long lists elided
func (n *Node) Repr() (string, error) {
	var b strings.Builder
	err := n.reprDepth(&b, 3)
	return b.String(), err
}

func (n *Node) reprDepth(b *strings.Builder, depth int) error {
	if depth <= 0 {
		b.WriteString(n.Type + "(...)")
		return nil
	}
	b.WriteString(n.Type + "(")
	for i, f := range n.spec.fields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(f.name + "=")
		switch v := n.vals[i].(type) {
		case []*Node:
			if err := reprNodeList(b, v, depth); err != nil {
				return err
			}
		case []string:
			items := make([]any, len(v))
			for j, s := range v {
				items[j] = s
			}
			b.WriteString(pyrepr.Repr(items))
		case *Node:
			if v == nil {
				b.WriteString("None")
			} else if err := v.reprDepth(b, depth-1); err != nil {
				return err
			}
		default:
			s, err := ConstantRepr(v)
			if err != nil {
				return err
			}
			b.WriteString(s)
		}
	}
	b.WriteString(")")
	return nil
}

func reprNodeList(b *strings.Builder, list []*Node, depth int) error {
	if len(list) == 0 {
		b.WriteString("[]")
		return nil
	}
	if depth <= 0 {
		b.WriteString("[...]")
		return nil
	}
	item := func(n *Node) error {
		if n == nil {
			b.WriteString("None")
			return nil
		}
		return n.reprDepth(b, depth-1)
	}
	b.WriteString("[")
	if err := item(list[0]); err != nil {
		return err
	}
	if len(list) > 2 {
		b.WriteString(", ...")
	}
	if len(list) > 1 {
		b.WriteString(", ")
		if err := item(list[len(list)-1]); err != nil {
			return err
		}
	}
	b.WriteString("]")
	return nil
}

// Items of a list literal rendered with str, false on ValueError, SyntaxError or a non list, other exceptions returned
func LiteralStrList(text string) ([]string, bool, error) {
	v, err := LiteralEval(pystr.Strip(text))
	if err != nil {
		var se *SyntaxError
		var le *LiteralError
		if errors.As(err, &se) || (errors.As(err, &le) && le.Class == "ValueError") {
			return nil, false, nil
		}
		return nil, false, err
	}
	items, ok := v.([]any)
	if !ok {
		return nil, false, nil
	}
	out := make([]string, len(items))
	for i, item := range items {
		if err := intDigitsCheck(item); err != nil {
			return nil, false, err
		}
		out[i] = pyrepr.Str(item)
	}
	return out, true, nil
}

// The ValueError str raises for an int past the 4300 digit limit anywhere inside a literal value
func intDigitsCheck(v any) error {
	switch x := v.(type) {
	case *big.Int:
		_, err := ConstantRepr(x)
		return err
	case []any:
		for _, e := range x {
			if err := intDigitsCheck(e); err != nil {
				return err
			}
		}
	case pyrepr.Tuple:
		return intDigitsCheck([]any(x))
	case Set:
		return intDigitsCheck([]any(x))
	case *Dict:
		if err := intDigitsCheck(x.Keys); err != nil {
			return err
		}
		return intDigitsCheck(x.Values)
	}
	return nil
}
