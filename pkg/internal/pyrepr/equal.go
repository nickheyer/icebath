package pyrepr

import (
	"math"
	"math/big"
)

// Python equality over the value model with bool, int and float comparing numerically
func Equal(a, b any) bool {
	if na, fa, okA := number(a); okA {
		nb, fb, okB := number(b)
		if !okB {
			return false
		}
		if fa != nil && math.IsNaN(*fa) || fb != nil && math.IsNaN(*fb) {
			return false
		}
		return exactFloat(na, fa).Cmp(exactFloat(nb, fb)) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case Tuple:
		y, ok := b.(Tuple)
		return ok && equalSeq(x, y)
	case []any:
		y, ok := b.([]any)
		return ok && equalSeq(x, y)
	case Mapping:
		y, ok := b.(Mapping)
		return ok && equalMapping(x, y)
	}
	return false
}

// Element wise equality of two sequences of the same length
func equalSeq(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Dict equality, the same key set with equal values under every key
func equalMapping(a, b Mapping) bool {
	type item struct{ k, v any }
	var ib []item
	for k, v := range b.PyItems() {
		ib = append(ib, item{k, v})
	}
	n := 0
	for k, v := range a.PyItems() {
		n++
		found := false
		for _, it := range ib {
			if Equal(k, it.k) {
				found = Equal(v, it.v)
				break
			}
		}
		if !found {
			return false
		}
	}
	return n == len(ib)
}

// Numeric value of a bool, int or float, the float kept apart for NaN checks
func number(v any) (*big.Int, *float64, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return big.NewInt(1), nil, true
		}
		return big.NewInt(0), nil, true
	}
	if n, ok := Int(v); ok {
		return n, nil, true
	}
	if f, ok := floatOf(v); ok {
		return nil, &f, true
	}
	return nil, nil, false
}

// Exact big float of an int or a finite or infinite float
func exactFloat(n *big.Int, f *float64) *big.Float {
	if f != nil {
		return new(big.Float).SetFloat64(*f)
	}
	return new(big.Float).SetInt(n)
}
