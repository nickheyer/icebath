package pythonutil

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Value of a tuple literal
type Tuple []any

// Value of a set literal, its distinct members in source order
type Set []any

// Value of a dict literal, keys in source order, a repeated key keeping its first place and its last value
type Dict struct{ Keys, Values []any }

// Value of a Python literal expression: nil for None, bool, int64, *big.Int past int64, float64, complex128, string, []byte, Ellipsis, []any for a list, Tuple, Set or *Dict
func LiteralEval(src string) (any, error) {
	tree, err := parseExpression(strings.TrimLeft(src, " \t"))
	if err != nil {
		return nil, err
	}
	return literalValue(tree.Child("body"))
}

// Items of a Python list literal as text, a string item as it is, None items left out and any other item in its literal form, false when src is not a list literal
func LiteralStrings(src string) ([]string, bool) {
	v, err := LiteralEval(strings.TrimSpace(src))
	items, ok := v.([]any)
	if err != nil || !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		if s, isStr := it.(string); isStr {
			out = append(out, s)
		} else {
			out = append(out, Literal(it))
		}
	}
	return out, true
}

func notLiteral(n *Node) error {
	if n == nil {
		return &LiteralError{Msg: "not a literal: dict unpacking"}
	}
	return &LiteralError{Line: n.Line, Msg: "not a literal: " + exprName(n)}
}

// Number a Constant holds, an error for anything else, booleans included
func literalNumber(n *Node) (any, error) {
	if n.Is("Constant") {
		switch v := n.Get("value").(type) {
		case int64, *big.Int, float64, complex128:
			return v, nil
		}
	}
	return nil, notLiteral(n)
}

// Number with an optional unary plus or minus
func literalSignedNumber(n *Node) (any, error) {
	if n.Is("UnaryOp") && n.Child("op").Is("UAdd", "USub") {
		v, err := literalNumber(n.Child("operand"))
		if err != nil || n.Child("op").Is("UAdd") {
			return v, err
		}
		return negateNumber(v), nil
	}
	return literalNumber(n)
}

func negateNumber(v any) any {
	switch x := v.(type) {
	case int64:
		if x == math.MinInt64 {
			return new(big.Int).Neg(big.NewInt(x))
		}
		return -x
	case *big.Int:
		neg := new(big.Int).Neg(x)
		if neg.IsInt64() {
			return neg.Int64()
		}
		return neg
	case float64:
		return -x
	case complex128:
		return -x
	}
	return v
}

// Real number as a float, false for a complex number or an integer too large for a float
func realFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, !math.IsInf(f, 0)
	case float64:
		return x, true
	}
	return 0, false
}

func literalValue(n *Node) (any, error) {
	if n == nil {
		return nil, notLiteral(nil)
	}
	switch n.Type {
	case "Constant":
		return n.Get("value"), nil
	case "Tuple", "List", "Set":
		elts := n.Children("elts")
		items := make([]any, 0, len(elts))
		var seen map[string]bool
		for _, e := range elts {
			v, err := literalValue(e)
			if err != nil {
				return nil, err
			}
			if n.Type == "Set" {
				if kind := unhashable(v); kind != "" {
					return nil, &LiteralError{Line: e.Line, Msg: "cannot use " + kind + " as a set element"}
				}
				if k, ok := literalKey(v); ok {
					if seen[k] {
						continue
					}
					if seen == nil {
						seen = map[string]bool{}
					}
					seen[k] = true
				}
			}
			items = append(items, v)
		}
		switch n.Type {
		case "Tuple":
			return Tuple(items), nil
		case "Set":
			return Set(items), nil
		}
		return items, nil
	case "Call":
		if f := n.Child("func"); f.Is("Name") && len(n.Children("args")) == 0 && len(n.Children("keywords")) == 0 {
			if id, _ := f.Str("id"); id == "set" {
				return Set{}, nil
			}
		}
	case "Dict":
		keys, vals := n.Children("keys"), n.Children("values")
		if len(keys) != len(vals) {
			return nil, notLiteral(n)
		}
		d := &Dict{Keys: make([]any, 0, len(keys)), Values: make([]any, 0, len(vals))}
		index := map[string]int{}
		for i := range keys {
			k, err := literalValue(keys[i])
			if err != nil {
				return nil, err
			}
			v, err := literalValue(vals[i])
			if err != nil {
				return nil, err
			}
			if kind := unhashable(k); kind != "" {
				return nil, &LiteralError{Line: keys[i].Line, Msg: "cannot use " + kind + " as a dict key"}
			}
			if key, ok := literalKey(k); ok {
				if at, dup := index[key]; dup {
					d.Values[at] = v
					continue
				}
				index[key] = len(d.Keys)
			}
			d.Keys = append(d.Keys, k)
			d.Values = append(d.Values, v)
		}
		return d, nil
	case "BinOp":
		if op := n.Child("op"); op.Is("Add", "Sub") {
			left, err := literalSignedNumber(n.Child("left"))
			if err != nil {
				return nil, err
			}
			right, err := literalNumber(n.Child("right"))
			if err != nil {
				return nil, err
			}
			rc, isComplex := right.(complex128)
			lf, isReal := realFloat(left)
			if isComplex && isReal {
				if op.Is("Add") {
					return complex(lf+real(rc), imag(rc)), nil
				}
				return complex(lf-real(rc), -imag(rc)), nil
			}
		}
	}
	return literalSignedNumber(n)
}

// Kind of the first member that cannot be hashed, list, set or dict, empty when the value can be a key
func unhashable(v any) string {
	switch x := v.(type) {
	case []any:
		return "list"
	case Set:
		return "set"
	case *Dict:
		return "dict"
	case Tuple:
		for _, e := range x {
			if kind := unhashable(e); kind != "" {
				return kind
			}
		}
	}
	return ""
}

// Key equal for exactly the values Python holds equal, numbers by value across int, float, complex and bool, false for NaN and unhashable values
func literalKey(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "N", true
	case Ellipsis:
		return "E", true
	case bool:
		if x {
			return "n1", true
		}
		return "n0", true
	case int64:
		return "n" + strconv.FormatInt(x, 10), true
	case int:
		return "n" + strconv.Itoa(x), true
	case *big.Int:
		return "n" + x.String(), true
	case float64:
		return floatKey(x)
	case complex128:
		if imag(x) == 0 {
			return floatKey(real(x))
		}
		re, okRe := floatKey(real(x))
		im, okIm := floatKey(imag(x))
		return "c" + strconv.Itoa(len(re)) + ":" + re + im, okRe && okIm
	case string:
		return "s" + x, true
	case []byte:
		return "b" + string(x), true
	case Tuple:
		var b strings.Builder
		b.WriteByte('t')
		for _, e := range x {
			k, ok := literalKey(e)
			if !ok {
				return "", false
			}
			b.WriteString(strconv.Itoa(len(k)))
			b.WriteByte(':')
			b.WriteString(k)
		}
		return b.String(), true
	}
	return "", false
}

// Key of a float, an integral value sharing the key of the equal integer
func floatKey(f float64) (string, bool) {
	switch {
	case math.IsNaN(f):
		return "", false
	case math.IsInf(f, 1):
		return "f+inf", true
	case math.IsInf(f, -1):
		return "f-inf", true
	case f == math.Trunc(f):
		i, _ := big.NewFloat(f).Int(nil)
		return "n" + i.String(), true
	}
	return "f" + strconv.FormatFloat(f, 'g', -1, 64), true
}
