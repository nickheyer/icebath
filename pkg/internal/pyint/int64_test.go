package pyint

import (
	"math/big"
	"testing"
)

func TestInt64CoercesLikePythonIntIntoACLong(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int64
	}{{int64(7), 7}, {"  12 ", 12}, {3.9, 3}, {true, 1}, {-2.5, -2}} {
		got, err := Int64(c.in)
		if err != nil || got != c.want {
			t.Fatalf("Int64(%#v) = %d, %v", c.in, got, err)
		}
	}
	huge, _ := new(big.Int).SetString("99999999999999999999", 10)
	if _, err := Int64(huge); err == nil || err.(*Error).Kind != "OverflowError" || err.Error() != "Python int too large to convert to C long" {
		t.Fatalf("overflow error %v", err)
	}
	if _, err := Int64("x"); err == nil || err.(*Error).Kind != "ValueError" {
		t.Fatalf("bad literal error %v", err)
	}
	if _, err := Int64(nil); err == nil || err.(*Error).Kind != "TypeError" {
		t.Fatalf("none error %v", err)
	}
}
