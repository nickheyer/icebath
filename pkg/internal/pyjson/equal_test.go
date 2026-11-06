package pyjson

import (
	"math"
	"math/big"
	"testing"
)

func TestEqualFollowsPythonEquality(t *testing.T) {
	a, _ := Loads(`{"a": 1, "b": [true, 2.0, null, "x"]}`)
	b, _ := Loads(`{"b": [1, 2, null, "x"], "a": 1.0}`)
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	cases := []struct {
		x, y any
		want bool
	}{
		{a, b, true},
		{a, `{"a": 1}`, false},
		{int64(1), true, true},
		{int64(0), false, true},
		{int64(2), 2.5, false},
		{math.NaN(), math.NaN(), false},
		{huge, new(big.Int).Set(huge), true},
		{huge, 1.2345678901234568e29, false},
		{nil, nil, true},
		{"1", int64(1), false},
		{[]any{int64(1)}, []any{int64(1), int64(2)}, false},
	}
	for i, c := range cases {
		if got := Equal(c.x, c.y); got != c.want {
			t.Errorf("case %d Equal(%v, %v) = %v want %v", i, c.x, c.y, got, c.want)
		}
	}
}
