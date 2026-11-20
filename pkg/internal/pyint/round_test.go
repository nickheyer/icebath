package pyint

import (
	"math"
	"testing"
)

func TestRoundMatchesCPython(t *testing.T) {
	cases := []struct {
		x    float64
		n    int
		want float64
	}{
		{10.0, 1, 10.0},
		{2.675, 2, 2.67},
		{0.25, 1, 0.2},
		{0.35, 1, 0.3},
		{0.45, 1, 0.5},
		{-1.25, 1, -1.2},
		{12.04999, 1, 12.0},
		{1234.5678, 0, 1235.0},
		{2.5, 0, 2.0},
		{3.5, 0, 4.0},
	}
	for _, c := range cases {
		if got := Round(c.x, c.n); got != c.want {
			t.Errorf("round(%v, %d) = %v, want %v", c.x, c.n, got, c.want)
		}
	}
	if !math.IsInf(Round(math.Inf(1), 1), 1) || !math.IsNaN(Round(math.NaN(), 1)) {
		t.Fatal("round must keep inf and nan")
	}
}
