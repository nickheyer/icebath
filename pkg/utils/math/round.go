package mathutil

import (
	"math"
	"strconv"
)

// Places beyond every decimal digit a float64 can carry (its shortest form spans 1e-324 to 1e308 with at most 17 digits), so that digits past it change nothing and digits+exponent cannot overflow
const maxDigits = 400

// x rounded to digits places after the decimal point, before it when digits is negative, an exact tie in its shortest decimal form going to the even digit
func Round(x float64, digits int) float64 {
	switch {
	case x == 0 || math.IsNaN(x) || math.IsInf(x, 0) || digits > maxDigits:
		return x
	case digits < -maxDigits:
		return math.Copysign(0, x)
	}
	var buf [32]byte
	text := strconv.AppendFloat(buf[:0], math.Abs(x), 'e', -1, 64)
	mark := len(text) - 1
	for text[mark] != 'e' {
		mark--
	}
	exp, _ := strconv.Atoi(string(text[mark+1:]))
	var mantissa [24]byte
	ds := append(mantissa[:0], text[0])
	if mark > 1 {
		ds = append(ds, text[2:mark]...)
	}
	keep := exp + 1 + digits
	if keep >= len(ds) {
		return x
	}
	if keep < 0 {
		return math.Copysign(0, x)
	}
	up := ds[keep] > '5'
	if ds[keep] == '5' {
		up = keep > 0 && (ds[keep-1]-'0')%2 == 1
		for _, d := range ds[keep+1:] {
			if d != '0' {
				up = true
				break
			}
		}
	}
	kept := ds[:keep]
	if up {
		i := len(kept) - 1
		for ; i >= 0 && kept[i] == '9'; i-- {
			kept[i] = '0'
		}
		if i >= 0 {
			kept[i]++
		} else {
			kept = append([]byte{'1'}, kept...)
		}
	}
	if len(kept) == 0 {
		return math.Copysign(0, x)
	}
	out := append(kept, 'e')
	out = strconv.AppendInt(out, int64(-digits), 10)
	r, _ := strconv.ParseFloat(string(out), 64)
	return math.Copysign(r, x)
}
