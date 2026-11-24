package memory

import (
	"errors"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
)

// Python int or float result of summing stored cells, float once any operand was a float
type InsightsNum struct {
	I     int64
	F     float64
	Float bool
}

// Numeric value as a float64
func (n InsightsNum) Value() float64 {
	if n.Float {
		return n.F
	}
	return float64(n.I)
}

func (n InsightsNum) py() any {
	if n.Float {
		return n.F
	}
	return n.I
}

// Python int or float of a value accumulated by the insights arithmetic
func numOf(v any) InsightsNum {
	switch x := v.(type) {
	case int64:
		return InsightsNum{I: x}
	case float64:
		return InsightsNum{F: x, Float: true}
	case InsightsNum:
		return x
	}
	return InsightsNum{}
}

// Value of x or 0 the way Python short circuits a falsy cell
func or0(v any) any {
	if anyutil.IsTrue(v) {
		return v
	}
	return int64(0)
}

func pyTypeError(op string, a, b any) error {
	return fmt.Errorf("cannot apply %s to %s and %s", op, errchain.Kind(a), errchain.Kind(b))
}

// Python a + b or a - b over int and float cells, a TypeError for any other operand
func pyArith(op string, a, b any) (any, error) {
	if n, ok := a.(InsightsNum); ok {
		a = n.py()
	}
	if n, ok := b.(InsightsNum); ok {
		b = n.py()
	}
	ai, aInt := a.(int64)
	bi, bInt := b.(int64)
	af, aFl := a.(float64)
	bf, bFl := b.(float64)
	if (!aInt && !aFl) || (!bInt && !bFl) {
		return nil, pyTypeError(op, a, b)
	}
	if aInt && bInt {
		if op == "-" {
			return ai - bi, nil
		}
		return ai + bi, nil
	}
	if aInt {
		af = float64(ai)
	}
	if bInt {
		bf = float64(bi)
	}
	if op == "-" {
		return af - bf, nil
	}
	return af + bf, nil
}

// Python float() of a stored cell, bytes read as their text like float(b"1.5")
func pyFloat(v any) (float64, error) {
	switch x := v.(type) {
	case InsightsNum:
		return x.Value(), nil
	case []byte:
		v = string(x)
	}
	return anyutil.ToFloat(v)
}

// Python int() of a stored cell, truncating floats and parsing decimal text
func pyIntCell(v any) (int64, error) {
	switch x := v.(type) {
	case InsightsNum:
		v = x.py()
	case []byte:
		v = string(x)
	}
	n, err := anyutil.ToInt(v)
	if f, isFloat := v.(float64); errors.Is(err, anyutil.ErrRange) && !(isFloat && math.IsInf(f, 0)) {
		return 0, fmt.Errorf("%s is too large for a 64-bit integer", errchain.Show(v))
	}
	if err != nil {
		return 0, errchain.Reword(err, "%s is not a whole number", errchain.Show(v))
	}
	return n, nil
}

// Python three way comparison of None, numbers, str and bytes, a TypeError for mixed kinds
func pyCompare(a, b any) (int, error) {
	if n, ok := a.(InsightsNum); ok {
		a = n.py()
	}
	if n, ok := b.(InsightsNum); ok {
		b = n.py()
	}
	af, aNum := insightsNumber(a)
	bf, bNum := insightsNumber(b)
	switch {
	case aNum && bNum:
		ai, aInt := a.(int64)
		bi, bInt := b.(int64)
		if aInt && bInt {
			return cmpOrdered(ai, bi), nil
		}
		return cmpOrdered(af, bf), nil
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return cmpOrdered(as, bs), nil
		}
	}
	if ab, ok := a.([]byte); ok {
		if bb, ok := b.([]byte); ok {
			return cmpOrdered(string(ab), string(bb)), nil
		}
	}
	return 0, fmt.Errorf("cannot compare %s with %s", errchain.Kind(a), errchain.Kind(b))
}

func insightsNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func cmpOrdered[T int64 | float64 | string](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Python format(value, spec) for the fixed specs the reports use
func pyFormat(v any, spec string) string {
	if n, ok := v.(InsightsNum); ok {
		v = n.py()
	}
	s, err := templateutil.Format("{v:"+spec+"}", map[string]any{"v": v})
	if err != nil {
		panic("insights format spec " + spec + ": " + err.Error())
	}
	return s
}
