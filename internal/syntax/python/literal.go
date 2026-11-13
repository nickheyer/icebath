package python

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Content of a constant node or result of an evaluated literal expression
type Value interface{ value() }

// Text string, surrogate code points kept in their three byte generalized UTF-8 form
type Str string

// Byte string
type Bytes []byte

// Integer of any size
type Int struct{ *big.Int }

// Double precision float
type Float float64

// Number with real and imaginary parts, the value of an imaginary literal
type Complex complex128

// Boolean constant
type Bool bool

// Constant standing for no value
type None struct{}

// The ellipsis constant
type Ellipsis struct{}

// Evaluated tuple display
type TupleValue []Value

// Evaluated list display
type ListValue []Value

// Evaluated set display holding distinct members in first insertion order
type SetValue []Value

// Evaluated dict display holding distinct keys in first insertion order with their last values
type DictValue struct {
	Keys   []Value
	Values []Value
}

func (Str) value()        {}
func (Bytes) value()      {}
func (Int) value()        {}
func (Float) value()      {}
func (Complex) value()    {}
func (Bool) value()       {}
func (None) value()       {}
func (Ellipsis) value()   {}
func (TupleValue) value() {}
func (ListValue) value()  {}
func (SetValue) value()   {}
func (DictValue) value()  {}

// Most decimal digits an integer may have in text
const maxStrDigits = 4300

// Evaluates a literal expression of strings, bytes, numbers, tuples, lists, sets, dicts, booleans, None and Ellipsis
func LiteralEval(src string) (Value, error) {
	text := strings.TrimLeft(src, " \t")
	if strings.IndexByte(text, 0) >= 0 {
		return nil, nullBytesError()
	}
	e, err := parseExpression(string(translateLines([]byte(validText(text)), false)))
	if err != nil {
		return nil, err
	}
	return evalLiteral(e)
}

// Malformed node failure naming the node kind and its line
func malformed(e Expr) error {
	if e == nil {
		return &LiteralError{Kind: LiteralMalformed, Msg: "malformed node or string: None"}
	}
	name := fmt.Sprintf("%T", e)
	name = name[strings.LastIndexByte(name, '.')+1:]
	return &LiteralError{Kind: LiteralMalformed, Msg: fmt.Sprintf("malformed node or string on line %d: %s", e.Position().Line, name)}
}

// Number constant, failing for anything else including booleans
func evalNum(e Expr) (Value, error) {
	if c, ok := e.(*Constant); ok {
		switch c.Value.(type) {
		case Int, Float, Complex:
			return c.Value, nil
		}
	}
	return nil, malformed(e)
}

// Number constant optionally signed by a unary plus or minus
func evalSignedNum(e Expr) (Value, error) {
	u, ok := e.(*UnaryOp)
	if !ok || (u.Op != UAdd && u.Op != USub) {
		return evalNum(e)
	}
	v, err := evalNum(u.Operand)
	if err != nil || u.Op == UAdd {
		return v, err
	}
	switch x := v.(type) {
	case Int:
		return Int{new(big.Int).Neg(x.Int)}, nil
	case Float:
		return -x, nil
	case Complex:
		return Complex(complex(-real(x), -imag(x))), nil
	}
	return v, nil
}

func evalLiteral(e Expr) (Value, error) {
	switch x := e.(type) {
	case *Constant:
		return x.Value, nil
	case *Tuple:
		items, err := evalItems(x.Elts)
		return TupleValue(items), err
	case *List:
		items, err := evalItems(x.Elts)
		return ListValue(items), err
	case *Set:
		out := SetValue{}
		for _, el := range x.Elts {
			v, err := evalLiteral(el)
			if err != nil {
				return nil, err
			}
			if err := requireHashable(v, "set element"); err != nil {
				return nil, err
			}
			if !containsValue(out, v) {
				out = append(out, v)
			}
		}
		return out, nil
	case *Call:
		if f, ok := x.Func.(*Name); ok && f.ID == "set" && len(x.Args) == 0 && len(x.Keywords) == 0 {
			return SetValue{}, nil
		}
	case *Dict:
		return evalDict(x)
	case *BinOp:
		if x.Op == Add || x.Op == Sub {
			if v, done, err := evalComplexSum(x); done {
				return v, err
			}
		}
	}
	return evalSignedNum(e)
}

func evalItems(elts []Expr) ([]Value, error) {
	items := make([]Value, 0, len(elts))
	for _, el := range elts {
		v, err := evalLiteral(el)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, nil
}

// Dict display, each key and value evaluated and inserted in turn so the first failing pair decides the error
func evalDict(d *Dict) (Value, error) {
	if len(d.Keys) != len(d.Values) {
		return nil, malformed(d)
	}
	out := DictValue{}
	for i := range d.Keys {
		k, err := evalLiteral(d.Keys[i])
		if err != nil {
			return nil, err
		}
		v, err := evalLiteral(d.Values[i])
		if err != nil {
			return nil, err
		}
		if err := requireHashable(k, "dict key"); err != nil {
			return nil, err
		}
		found := false
		for j, existing := range out.Keys {
			if valuesEqual(existing, k) {
				out.Values[j], found = v, true
				break
			}
		}
		if !found {
			out.Keys = append(out.Keys, k)
			out.Values = append(out.Values, v)
		}
	}
	return out, nil
}

// Real plus or minus imaginary literal, done false when the operands are not a real and a complex number
func evalComplexSum(b *BinOp) (Value, bool, error) {
	left, err := evalSignedNum(b.Left)
	if err != nil {
		return nil, true, err
	}
	right, err := evalNum(b.Right)
	if err != nil {
		return nil, true, err
	}
	rc, ok := right.(Complex)
	if !ok {
		return nil, false, nil
	}
	var lf float64
	switch l := left.(type) {
	case Int:
		f, _ := new(big.Float).SetInt(l.Int).Float64()
		if math.IsInf(f, 0) {
			return nil, true, &LiteralError{Kind: LiteralOverflow, Msg: "int too large to convert to float"}
		}
		lf = f
	case Float:
		lf = float64(l)
	default:
		return nil, false, nil
	}
	if b.Op == Add {
		return Complex(complex(lf+real(rc), imag(rc))), true, nil
	}
	return Complex(complex(lf-real(rc), -imag(rc))), true, nil
}

// Fails for a list, dict or set, or a tuple holding one, as a member that cannot be hashed
func requireHashable(v Value, role string) error {
	if inner := unhashable(v); inner != "" {
		return &LiteralError{Kind: LiteralUnhashable, Msg: "cannot use '" + typeName(v) + "' as a " + role + " (unhashable type: '" + inner + "')"}
	}
	return nil
}

// Type name of the first part that cannot be hashed, empty when the whole hashes
func unhashable(v Value) string {
	switch x := v.(type) {
	case ListValue, DictValue, SetValue:
		return typeName(v)
	case TupleValue:
		for _, el := range x {
			if inner := unhashable(el); inner != "" {
				return inner
			}
		}
	}
	return ""
}

func typeName(v Value) string {
	switch v.(type) {
	case Str:
		return "str"
	case Bytes:
		return "bytes"
	case Int:
		return "int"
	case Float:
		return "float"
	case Complex:
		return "complex"
	case Bool:
		return "bool"
	case None:
		return "NoneType"
	case Ellipsis:
		return "ellipsis"
	case TupleValue:
		return "tuple"
	case ListValue:
		return "list"
	case SetValue:
		return "set"
	}
	return "dict"
}

func containsValue(set []Value, v Value) bool {
	for _, m := range set {
		if valuesEqual(m, v) {
			return true
		}
	}
	return false
}

// Real and imaginary parts of a number as exact floats, false for a value that is not a number
func numberParts(v Value) (*big.Float, float64, bool) {
	switch x := v.(type) {
	case Bool:
		if x {
			return big.NewFloat(1), 0, true
		}
		return big.NewFloat(0), 0, true
	case Int:
		return new(big.Float).SetInt(x.Int), 0, true
	case Float:
		if math.IsNaN(float64(x)) {
			return nil, 0, true
		}
		return big.NewFloat(float64(x)), 0, true
	case Complex:
		if math.IsNaN(real(x)) || math.IsNaN(imag(x)) {
			return nil, 0, true
		}
		return big.NewFloat(real(x)), imag(x), true
	}
	return nil, 0, false
}

// Equality of hashable literal values, numbers of every kind compared by value
func valuesEqual(a, b Value) bool {
	if ra, ia, ok := numberParts(a); ok {
		rb, ib, okB := numberParts(b)
		return okB && ra != nil && rb != nil && ia == ib && ra.Cmp(rb) == 0
	}
	switch x := a.(type) {
	case Str:
		y, ok := b.(Str)
		return ok && x == y
	case Bytes:
		y, ok := b.(Bytes)
		return ok && string(x) == string(y)
	case None:
		_, ok := b.(None)
		return ok
	case Ellipsis:
		_, ok := b.(Ellipsis)
		return ok
	case TupleValue:
		y, ok := b.(TupleValue)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valuesEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// Renders a value as the reference displays it, a string as itself and every other value as written in source
func Text(v Value) (string, error) {
	if s, ok := v.(Str); ok {
		return string(s), nil
	}
	var b strings.Builder
	if err := writeRepr(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeRepr(b *strings.Builder, v Value) error {
	switch x := v.(type) {
	case Str:
		b.WriteString(strRepr(string(x)))
	case Bytes:
		b.WriteString(bytesRepr(x))
	case Int:
		s, err := intText(x.Int)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case Float:
		b.WriteString(floatRepr(float64(x)))
	case Complex:
		b.WriteString(complexRepr(complex128(x)))
	case Bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case None:
		b.WriteString("None")
	case Ellipsis:
		b.WriteString("Ellipsis")
	case TupleValue:
		b.WriteByte('(')
		if err := writeItems(b, x); err != nil {
			return err
		}
		if len(x) == 1 {
			b.WriteByte(',')
		}
		b.WriteByte(')')
	case ListValue:
		b.WriteByte('[')
		if err := writeItems(b, x); err != nil {
			return err
		}
		b.WriteByte(']')
	case SetValue:
		if len(x) == 0 {
			b.WriteString("set()")
			return nil
		}
		b.WriteByte('{')
		if err := writeItems(b, x); err != nil {
			return err
		}
		b.WriteByte('}')
	case DictValue:
		b.WriteByte('{')
		for i := range x.Keys {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeRepr(b, x.Keys[i]); err != nil {
				return err
			}
			b.WriteString(": ")
			if err := writeRepr(b, x.Values[i]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	}
	return nil
}

func writeItems(b *strings.Builder, items []Value) error {
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		if err := writeRepr(b, it); err != nil {
			return err
		}
	}
	return nil
}

// Decimal text of an integer, refused past 4300 digits
func intText(x *big.Int) (string, error) {
	if x.BitLen() > 14280 {
		bound := new(big.Int).Exp(big.NewInt(10), big.NewInt(maxStrDigits), nil)
		if new(big.Int).Abs(x).Cmp(bound) >= 0 {
			return "", ErrDigitLimit
		}
	}
	return x.String(), nil
}

// Code point at the start of text decoded as generalized UTF-8, surrogates included
func decodeWTF8(s string) (rune, int) {
	if len(s) >= 3 && s[0] == 0xed && s[1] >= 0xa0 && s[1] <= 0xbf && s[2] >= 0x80 && s[2] <= 0xbf {
		return rune(s[0]&0x0f)<<12 | rune(s[1]&0x3f)<<6 | rune(s[2]&0x3f), 3
	}
	return utf8.DecodeRuneInString(s)
}

// Quoted string with the quote choice and escapes of the reference, printable characters kept
func strRepr(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	b := make([]byte, 0, len(s)+2)
	b = append(b, quote)
	for i := 0; i < len(s); {
		r, n := decodeWTF8(s[i:])
		i += n
		switch {
		case r == rune(quote) || r == '\\':
			b = append(b, '\\', byte(r))
		case r == '\t':
			b = append(b, `\t`...)
		case r == '\n':
			b = append(b, `\n`...)
		case r == '\r':
			b = append(b, `\r`...)
		case r < 0x20 || r == 0x7f:
			b = appendHexEscape(b, 'x', r, 2)
		case r < 0x7f || isPrintable(r):
			b = utf8.AppendRune(b, r)
		case r <= 0xff:
			b = appendHexEscape(b, 'x', r, 2)
		case r <= 0xffff:
			b = appendHexEscape(b, 'u', r, 4)
		default:
			b = appendHexEscape(b, 'U', r, 8)
		}
	}
	return string(append(b, quote))
}

// Quoted bytes with a b prefix, bytes outside printable ASCII escaped in hex
func bytesRepr(p []byte) string {
	quote := byte('\'')
	if strings.IndexByte(string(p), '\'') >= 0 && strings.IndexByte(string(p), '"') < 0 {
		quote = '"'
	}
	b := make([]byte, 0, len(p)+3)
	b = append(b, 'b', quote)
	for _, c := range p {
		switch {
		case c == quote || c == '\\':
			b = append(b, '\\', c)
		case c == '\t':
			b = append(b, `\t`...)
		case c == '\n':
			b = append(b, `\n`...)
		case c == '\r':
			b = append(b, `\r`...)
		case c < 0x20 || c >= 0x7f:
			b = appendHexEscape(b, 'x', rune(c), 2)
		default:
			b = append(b, c)
		}
	}
	return string(append(b, quote))
}

func appendHexEscape(b []byte, kind byte, r rune, width int) []byte {
	h := strconv.FormatInt(int64(r), 16)
	b = append(b, '\\', kind)
	for range width - len(h) {
		b = append(b, '0')
	}
	return append(b, h...)
}

// Shortest round trip digits, fixed between 1e-4 and 1e16 with a forced fraction and exponent form outside
func floatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	digits := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mant, expText, _ := strings.Cut(digits, "e")
	exp, _ := strconv.Atoi(expText)
	mant = strings.Replace(mant, ".", "", 1)
	sign := ""
	if f < 0 {
		sign = "-"
	}
	if exp < -4 || exp >= 16 {
		out := mant[:1]
		if len(mant) > 1 {
			out += "." + mant[1:]
		}
		esign := "+"
		if exp < 0 {
			esign, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + mant
	}
	if len(mant) <= exp+1 {
		return sign + mant + strings.Repeat("0", exp+1-len(mant)) + ".0"
	}
	return sign + mant[:exp+1] + "." + mant[exp+1:]
}

// Complex number as written, the bare imaginary part when the real part is positive zero
func complexRepr(c complex128) string {
	part := func(f float64) string { return strings.TrimSuffix(floatRepr(f), ".0") }
	re, im := real(c), imag(c)
	if re == 0 && !math.Signbit(re) {
		return part(im) + "j"
	}
	ims := part(im)
	if math.IsNaN(im) {
		ims = "+nan"
	} else if !strings.HasPrefix(ims, "-") {
		ims = "+" + ims
	}
	return "(" + part(re) + ims + "j)"
}
