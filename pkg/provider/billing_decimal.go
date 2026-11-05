package provider

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"unicode"
)

// ErrDecimalInvalidOperation is the decimal.InvalidOperation signal the default Python context traps
var ErrDecimalInvalidOperation = errors.New("[<class 'decimal.InvalidOperation'>]")

// ErrDecimalMemory is the MemoryError Python raises when a fixed point rendering cannot be allocated
var ErrDecimalMemory = errors.New("MemoryError")

const (
	decFinite byte = iota
	decNaN
	decSNaN
	decInf
)

const decContextPrec = 28
const decContextEmax = 999999
const decFormatMaxLen = math.MaxInt32

var (
	decMaxEmax  = big.NewInt(999999999999999999)
	decMinEtiny = big.NewInt(-1999999999999999997)
	decTen      = big.NewInt(10)
)

// PyDecimal is an exact Python Decimal, a signed coefficient and exponent or an infinity or NaN
type PyDecimal struct {
	neg     bool
	kind    byte
	coef    *big.Int
	exp     int64
	payload string
}

// DecimalFromInt is the exact Decimal of an integer
func DecimalFromInt(n int64) PyDecimal {
	c := big.NewInt(n)
	return PyDecimal{neg: n < 0, coef: c.Abs(c)}
}

// ParseDecimal reads text like the Python Decimal constructor, underscores dropped and Unicode digits accepted
func ParseDecimal(text string) (PyDecimal, error) {
	var b strings.Builder
	for _, r := range pyStripSpace(text) {
		switch {
		case r == '_':
		case r > 0 && r <= 127:
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.Is(unicode.Nd, r):
			b.WriteByte(byte('0' + unicodeDigitValue(r)))
		default:
			return PyDecimal{}, ErrDecimalInvalidOperation
		}
	}
	a := b.String()
	var d PyDecimal
	if a != "" && (a[0] == '+' || a[0] == '-') {
		d.neg, a = a[0] == '-', a[1:]
	}
	low := strings.ToLower(a)
	switch {
	case low == "inf" || low == "infinity":
		d.kind = decInf
		return d, nil
	case strings.HasPrefix(low, "nan") || strings.HasPrefix(low, "snan"):
		d.kind, a = decNaN, a[3:]
		if low[0] == 's' {
			d.kind, a = decSNaN, a[1:]
		}
		if !decAllDigits(a) {
			return PyDecimal{}, ErrDecimalInvalidOperation
		}
		d.payload = strings.TrimLeft(a, "0")
		return d, nil
	}
	mant, expText := a, ""
	if i := strings.IndexAny(a, "eE"); i >= 0 {
		mant, expText = a[:i], a[i+1:]
		sign := ""
		if expText != "" && (expText[0] == '+' || expText[0] == '-') {
			sign, expText = expText[:1], expText[1:]
		}
		if !decAllDigits(expText) || expText == "" {
			return PyDecimal{}, ErrDecimalInvalidOperation
		}
		expText = sign + expText
	}
	intPart, fracPart, _ := strings.Cut(mant, ".")
	if !decAllDigits(intPart) || !decAllDigits(fracPart) || intPart+fracPart == "" {
		return PyDecimal{}, ErrDecimalInvalidOperation
	}
	exp := new(big.Int)
	if expText != "" {
		exp.SetString(expText, 10)
	}
	exp.Sub(exp, big.NewInt(int64(len(fracPart))))
	d.coef, _ = new(big.Int).SetString(intPart+fracPart, 10)
	adj := new(big.Int).Set(exp)
	if d.coef.Sign() != 0 {
		adj.Add(adj, big.NewInt(int64(len(d.coef.String())-1)))
	}
	if adj.Cmp(decMaxEmax) > 0 || exp.Cmp(decMinEtiny) < 0 {
		return PyDecimal{}, ErrDecimalInvalidOperation
	}
	d.exp = exp.Int64()
	return d, nil
}

func decAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// IsNaN reports a quiet or signaling NaN
func (d PyDecimal) IsNaN() bool { return d.kind == decNaN || d.kind == decSNaN }

func (d PyDecimal) digits() string { return d.coef.String() }

func (d PyDecimal) adjusted() int64 { return d.exp + int64(len(d.digits())) - 1 }

// Python str of the Decimal in scientific notation rules
func (d PyDecimal) String() string {
	sign := ""
	if d.neg {
		sign = "-"
	}
	switch d.kind {
	case decInf:
		return sign + "Infinity"
	case decNaN:
		return sign + "NaN" + d.payload
	case decSNaN:
		return sign + "sNaN" + d.payload
	}
	coef := d.digits()
	leftdigits := d.exp + int64(len(coef))
	dotplace := int64(1)
	if d.exp <= 0 && leftdigits > -6 {
		dotplace = leftdigits
	}
	var intpart, fracpart string
	switch {
	case dotplace <= 0:
		intpart, fracpart = "0", "."+strings.Repeat("0", int(-dotplace))+coef
	case dotplace >= int64(len(coef)):
		intpart = coef + strings.Repeat("0", int(dotplace-int64(len(coef))))
	default:
		intpart, fracpart = coef[:dotplace], "."+coef[dotplace:]
	}
	expStr := ""
	if leftdigits != dotplace {
		e := leftdigits - dotplace
		expStr = "E+" + big.NewInt(e).String()
		if e < 0 {
			expStr = "E" + big.NewInt(e).String()
		}
	}
	return sign + intpart + fracpart + expStr
}

// Compare order of two non NaN values, infinities included, negative zero equal to zero
func (d PyDecimal) cmpNumeric(o PyDecimal) int {
	if d.kind == decInf || o.kind == decInf {
		a, b := decInfRank(d), decInfRank(o)
		switch {
		case a == b:
			return 0
		case a < b:
			return -1
		}
		return 1
	}
	dz, oz := d.coef.Sign() == 0, o.coef.Sign() == 0
	switch {
	case dz && oz:
		return 0
	case dz:
		return decSignOf(!o.neg) * -1
	case oz:
		return decSignOf(!d.neg)
	case o.neg && !d.neg:
		return 1
	case d.neg && !o.neg:
		return -1
	}
	da, oa := d.adjusted(), o.adjusted()
	if da != oa {
		if da > oa {
			return decSignOf(!d.neg)
		}
		return -decSignOf(!d.neg)
	}
	dc, oc := new(big.Int).Set(d.coef), new(big.Int).Set(o.coef)
	if d.exp > o.exp {
		dc.Mul(dc, new(big.Int).Exp(decTen, big.NewInt(d.exp-o.exp), nil))
	} else if o.exp > d.exp {
		oc.Mul(oc, new(big.Int).Exp(decTen, big.NewInt(o.exp-d.exp), nil))
	}
	c := dc.Cmp(oc)
	if d.neg {
		return -c
	}
	return c
}

func decSignOf(positive bool) int {
	if positive {
		return 1
	}
	return -1
}

// Python _isinfinity, one or minus one for an infinity and zero otherwise
func decInfRank(d PyDecimal) int {
	if d.kind != decInf {
		return 0
	}
	return decSignOf(!d.neg)
}

// Equal is Python ==, false with a quiet NaN and InvalidOperation with a signaling one
func (d PyDecimal) Equal(o PyDecimal) (bool, error) {
	if d.kind == decSNaN || o.kind == decSNaN {
		return false, ErrDecimalInvalidOperation
	}
	if d.IsNaN() || o.IsNaN() {
		return false, nil
	}
	return d.cmpNumeric(o) == 0, nil
}

// Cmp is the order Python less and greater operators use, InvalidOperation when either side is a NaN
func (d PyDecimal) Cmp(o PyDecimal) (int, error) {
	if d.IsNaN() || o.IsNaN() {
		return 0, ErrDecimalInvalidOperation
	}
	return d.cmpNumeric(o), nil
}

// Coefficient rounded half even after dropping k low digits
func decRoundHalfEven(c *big.Int, k int64) *big.Int {
	if k <= 0 {
		return new(big.Int).Mul(c, new(big.Int).Exp(decTen, big.NewInt(-k), nil))
	}
	if k > int64(len(c.String()))+1 {
		return new(big.Int)
	}
	m := new(big.Int).Exp(decTen, big.NewInt(k), nil)
	q, r := new(big.Int).QuoRem(c, m, new(big.Int))
	switch r.Mul(r, big.NewInt(2)).Cmp(m) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// Value moved to the exponent with half even rounding like Python _rescale
func (d PyDecimal) rescale(exp int64) PyDecimal {
	return PyDecimal{neg: d.neg, coef: decRoundHalfEven(d.coef, exp-d.exp), exp: exp}
}

// ToIntegralValue rounds half even to exponent zero, NaN kept and a signaling NaN raising
func (d PyDecimal) ToIntegralValue() (PyDecimal, error) {
	switch d.kind {
	case decSNaN:
		return PyDecimal{}, ErrDecimalInvalidOperation
	case decNaN, decInf:
		return d, nil
	}
	if d.exp >= 0 {
		return d, nil
	}
	return d.rescale(0), nil
}

// Quantize rounds half even to the exponent under the default 28 digit context, raising where Python does
func (d PyDecimal) Quantize(exp int64) (PyDecimal, error) {
	switch d.kind {
	case decSNaN, decInf:
		return PyDecimal{}, ErrDecimalInvalidOperation
	case decNaN:
		return d, nil
	}
	if exp > decContextEmax || exp < -decContextEmax-decContextPrec+1 {
		return PyDecimal{}, ErrDecimalInvalidOperation
	}
	if d.coef.Sign() == 0 {
		return PyDecimal{neg: d.neg, coef: new(big.Int), exp: exp}, nil
	}
	adj := d.adjusted()
	if adj > decContextEmax || adj-exp+1 > decContextPrec {
		return PyDecimal{}, ErrDecimalInvalidOperation
	}
	ans := d.rescale(exp)
	if ans.coef.Sign() != 0 && ans.adjusted() > decContextEmax || len(ans.digits()) > decContextPrec {
		return PyDecimal{}, ErrDecimalInvalidOperation
	}
	return ans, nil
}

// FormatFixed renders Python format spec f, or ,f with thousands separators when grouped
func (d PyDecimal) FormatFixed(grouped bool) (string, error) {
	sign := ""
	if d.neg {
		sign = "-"
	}
	switch d.kind {
	case decInf:
		return sign + "Infinity", nil
	case decNaN:
		return sign + "NaN" + d.payload, nil
	case decSNaN:
		return sign + "sNaN" + d.payload, nil
	}
	coef := d.digits()
	if d.exp > decFormatMaxLen-int64(len(coef)) || -d.exp > decFormatMaxLen {
		return "", ErrDecimalMemory
	}
	intpart, fracpart := coef, ""
	if d.exp >= 0 {
		if coef != "0" {
			intpart = coef + strings.Repeat("0", int(d.exp))
		}
	} else {
		n := int64(len(coef))
		if -d.exp >= n {
			intpart, fracpart = "0", strings.Repeat("0", int(-d.exp-n))+coef
		} else {
			intpart, fracpart = coef[:n+d.exp], coef[n+d.exp:]
		}
	}
	if grouped {
		var b strings.Builder
		for i, c := range intpart {
			if i > 0 && (len(intpart)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(c)
		}
		intpart = b.String()
	}
	if fracpart != "" {
		return sign + intpart + "." + fracpart, nil
	}
	return sign + intpart, nil
}
