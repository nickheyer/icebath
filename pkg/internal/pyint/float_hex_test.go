package pyint

import (
	"math"
	"testing"
)

func TestFloatHexMatchesCPython(t *testing.T) {
	cases := map[float64]string{
		1.0: "0x1.0000000000000p+0", 1000.0: "0x1.f400000000000p+9", 1700000000.5: "0x1.954fc40200000p+30", 0.1: "0x1.999999999999ap-4",
		-2.5: "-0x1.4000000000000p+1", 5e-324: "0x0.0000000000001p-1022", 2.2250738585072014e-308: "0x1.0000000000000p-1022",
		1.7976931348623157e308: "0x1.fffffffffffffp+1023", 3.0: "0x1.8000000000000p+1",
	}
	for x, want := range cases {
		if got := FloatHex(x); got != want {
			t.Errorf("FloatHex(%v) = %q want %q", x, got, want)
		}
	}
	if FloatHex(0) != "0x0.0p+0" || FloatHex(math.Copysign(0, -1)) != "-0x0.0p+0" || FloatHex(math.Inf(1)) != "inf" || FloatHex(math.Inf(-1)) != "-inf" || FloatHex(math.NaN()) != "nan" {
		t.Error("special values")
	}
}
