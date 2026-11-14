package pyint

import (
	"math"
	"math/big"
	"testing"
)

func TestOfMatchesPythonInt(t *testing.T) {
	ok := map[any]int64{" 12 ": 12, "-3": -3, "1_000": 1000, "١٢": 12, true: 1, false: 0, 7.9: 7, -7.9: -7, int64(5): 5}
	for in, want := range ok {
		n, err := Of(in)
		if err != nil || n.Int64() != want {
			t.Fatalf("%v: %v %v", in, n, err)
		}
	}
	bad := []struct {
		in   any
		kind string
	}{{"1__0", "ValueError"}, {"_1", "ValueError"}, {"abc", "ValueError"}, {"", "ValueError"}, {math.NaN(), "ValueError"}, {math.Inf(1), "OverflowError"}, {nil, "TypeError"}, {[]any{}, "TypeError"}}
	for _, c := range bad {
		in, kind := c.in, c.kind
		if _, err := Of(in); err == nil || err.(*Error).Kind != kind {
			t.Fatalf("%v: %v", in, err)
		}
	}
}

func TestNativeKeepsValuesPastTheIntRange(t *testing.T) {
	if v, ok := Native(big.NewInt(42)).(int); !ok || v != 42 {
		t.Fatalf("small value %v", Native(big.NewInt(42)))
	}
	huge, _ := Parse("123456789012345678901234567890")
	if v, ok := Native(huge).(*big.Int); !ok || v.String() != "123456789012345678901234567890" {
		t.Fatalf("huge value %v", Native(huge))
	}
}
