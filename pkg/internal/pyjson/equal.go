package pyjson

import (
	"math"
	"math/big"
)

// Python equality of decoded JSON values, dicts ignoring key order and numbers comparing across int, float and bool
func Equal(a, b any) bool {
	if an, ok := pyNumber(a); ok {
		bn, ok := pyNumber(b)
		return ok && numbersEqual(an, bn)
	}
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !Equal(av[i], bv[i]) {
				return false
			}
		}
		return true
	case *Object:
		if av == nil {
			bv, ok := b.(*Object)
			return ok && bv == nil
		}
		return dictEqual(av.Vals, b)
	case map[string]any:
		return dictEqual(av, b)
	}
	return a == b
}

// Python dict equality of a value map against an ordered or plain dict, key order ignored
func dictEqual(av map[string]any, b any) bool {
	bv, ok := Dict(b)
	if !ok || len(av) != len(bv) {
		return false
	}
	for k, v := range av {
		w, has := bv[k]
		if !has || !Equal(v, w) {
			return false
		}
	}
	return true
}

// Numeric value of a bool, integer or float as a big.Float or a float64 marker for NaN and infinities
func pyNumber(v any) (*big.Float, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int:
		return new(big.Float).SetInt64(int64(n)), true
	case int64:
		return new(big.Float).SetInt64(n), true
	case *big.Int:
		if n == nil {
			return nil, false
		}
		return new(big.Float).SetPrec(uint(n.BitLen()) + 64).SetInt(n), true
	case float64:
		if math.IsNaN(n) {
			return nil, true
		}
		return new(big.Float).SetPrec(64).SetFloat64(n), true
	}
	return nil, false
}

// Exact comparison where a nil operand stands for NaN, which equals nothing
func numbersEqual(a, b *big.Float) bool {
	return a != nil && b != nil && a.Cmp(b) == 0
}
