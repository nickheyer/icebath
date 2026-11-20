package pyint

import "strconv"

// Python round of a float to ndigits decimals, exact ties going to the even digit
func Round(x float64, ndigits int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', ndigits, 64), 64)
	return v
}
