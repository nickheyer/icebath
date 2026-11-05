package pyrepr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// float.__repr__, shortest round trip digits in fixed form between 1e-4 and 1e16 and exponent form outside
func Float(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	digits := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mant, expText, _ := strings.Cut(digits, "e")
	exp, _ := strconv.Atoi(expText)
	mant = strings.Replace(mant, ".", "", 1)
	sign := ""
	if f < 0 {
		sign = "-"
	}
	if exp < -4 || exp >= 16 {
		out := mant[:1]
		if len(mant) > 1 {
			out += "." + mant[1:]
		}
		esign := "+"
		if exp < 0 {
			esign = "-"
			exp = -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + mant
	}
	if len(mant) <= exp+1 {
		return sign + mant + strings.Repeat("0", exp+1-len(mant)) + ".0"
	}
	return sign + mant[:exp+1] + "." + mant[exp+1:]
}
