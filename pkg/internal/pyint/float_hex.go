package pyint

import (
	"math"
	"strconv"
	"strings"
)

// float.hex() of a float, thirteen hexadecimal mantissa digits and an unpadded binary exponent
func FloatHex(x float64) string {
	if math.IsNaN(x) {
		return "nan"
	}
	if math.IsInf(x, 0) {
		if x < 0 {
			return "-inf"
		}
		return "inf"
	}
	sign := ""
	if math.Signbit(x) {
		sign = "-"
	}
	if x == 0 {
		return sign + "0x0.0p+0"
	}
	m, e := math.Frexp(math.Abs(x))
	shift := 1 - max(-1021-e, 0)
	m = math.Ldexp(m, shift)
	e -= shift
	lead := int(m)
	m -= float64(lead)
	var digits strings.Builder
	for i := 0; i < 13; i++ {
		m *= 16.0
		d := int(m)
		digits.WriteByte("0123456789abcdef"[d])
		m -= float64(d)
	}
	exp := strconv.Itoa(e)
	if e >= 0 {
		exp = "+" + exp
	}
	return sign + "0x" + strconv.Itoa(lead) + "." + digits.String() + "p" + exp
}
