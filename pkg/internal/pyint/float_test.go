package pyint

import (
	"math"
	"math/big"
	"testing"
)

func TestFloatMatchesCPython(t *testing.T) {
	ok := map[any]float64{" 1.5 ": 1.5, "1_000.5": 1000.5, "1.": 1, ".5": 0.5, "1e3": 1000, "-2E-1": -0.2,
		"١٢": 12, true: 1, false: 0, 3: 3, int64(-4): -4, 2.25: 2.25, "1e400": math.Inf(1)}
	for in, want := range ok {
		got, err := Float(in)
		if err != nil || got != want {
			t.Fatalf("Float(%#v) = %v, %v want %v", in, got, err, want)
		}
	}
	for _, s := range []string{"inf", "-Infinity", "+INF"} {
		if f, err := Float(s); err != nil || !math.IsInf(f, 0) {
			t.Fatalf("Float(%q) = %v, %v", s, f, err)
		}
	}
	if f, err := Float(" NaN "); err != nil || !math.IsNaN(f) {
		t.Fatalf("nan = %v, %v", f, err)
	}
	for _, bad := range []string{"", "abc", "1__0", "_1", "1_", "0x10", "1e", "--1", "1.5.2", "infinit"} {
		if _, err := Float(bad); err == nil || err.(*Error).Kind != "ValueError" {
			t.Fatalf("Float(%q) err = %v", bad, err)
		}
	}
	if _, err := Float("x y"); err.Error() != "could not convert string to float: 'x y'" {
		t.Fatalf("message %v", err)
	}
	if _, err := Float(nil); err == nil || err.(*Error).Kind != "TypeError" {
		t.Fatalf("None err = %v", err)
	}
	huge, _ := new(big.Int).SetString("1"+string(make([]byte, 0))+"0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000", 10)
	if _, err := Float(huge); err == nil || err.(*Error).Kind != "OverflowError" {
		t.Fatalf("huge err = %v", err)
	}
}
