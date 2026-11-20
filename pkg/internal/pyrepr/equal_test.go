package pyrepr

import (
	"iter"
	"math"
	"math/big"
	"testing"
)

type eqMap [][2]any

func (m eqMap) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for _, kv := range m {
			if !yield(kv[0], kv[1]) {
				return
			}
		}
	}
}

func TestEqual(t *testing.T) {
	huge, _ := new(big.Int).SetString("100000000000000000000", 10)
	cases := []struct {
		a, b any
		want bool
	}{
		{nil, nil, true},
		{nil, "", false},
		{true, 1, true},
		{false, 0.0, true},
		{1, 1.0, true},
		{huge, 1e20, true},
		{"a", "a", true},
		{"1", 1, false},
		{math.NaN(), math.NaN(), false},
		{math.Inf(1), math.Inf(1), true},
		{[]any{1, "x"}, []any{1.0, "x"}, true},
		{[]any{1}, Tuple{1}, false},
		{eqMap{{"a", 1}, {"b", 2}}, eqMap{{"b", 2}, {"a", 1}}, true},
		{map[string]any{}, map[string]any{}, true},
		{map[string]any{"a": 1}, eqMap{{"a", 1.0}}, true},
		{eqMap{{"a", 1}}, map[string]any{"a": 1}, true},
		{map[string]any{"a": 1}, map[string]any{"a": 2}, false},
		{map[string]any{"a": 1}, map[string]any{"a": 1, "b": 2}, false},
		{map[string]any{}, []any{}, false},
		{eqMap{{"a", 1}}, eqMap{{"a", 1}, {"b", 2}}, false},
		{eqMap{{"a", 1}}, eqMap{{"a", 2}}, false},
	}
	for i, c := range cases {
		if got := Equal(c.a, c.b); got != c.want {
			t.Errorf("case %d Equal(%v, %v) = %v want %v", i, c.a, c.b, got, c.want)
		}
	}
}
