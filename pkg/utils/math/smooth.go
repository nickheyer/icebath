package mathutil

import "math"

// Each value replaced by the mean of the width values centred on it, the earlier side taking the extra one for an even width and positions past either end counting as zero, a NaN or infinity reaching only the means whose window holds it; as long as values, a copy for a width below two
func MovingAverage(values []float64, width int) []float64 {
	n := len(values)
	out := make([]float64, n)
	if width < 2 {
		copy(out, values)
		return out
	}
	before := width / 2
	after := width - 1 - before
	inv := 1 / float64(width)
	var s windowSum
	for j := range min(after+1, n) {
		s.add(values[j]*inv, 1)
	}
	for i := range n {
		out[i] = s.value()
		if j := i - before; j >= 0 {
			s.add(values[j]*inv, -1)
		}
		if j := i + 1 + after; j < n {
			s.add(values[j]*inv, 1)
		}
	}
	return out
}

// Sliding sum of the finite values in a window, carrying the rounding error of every addition so a long run does not drift, with the NaNs and infinities in the window counted apart so they leave no trace once they slide out
type windowSum struct {
	sum, carry          float64
	nan, posInf, negInf int
}

// Adds v to the window for sign 1, takes it back out for sign -1
func (s *windowSum) add(v float64, sign int) {
	switch {
	case math.IsNaN(v):
		s.nan += sign
	case math.IsInf(v, 1):
		s.posInf += sign
	case math.IsInf(v, -1):
		s.negInf += sign
	default:
		v *= float64(sign)
		t := s.sum + v
		if math.Abs(s.sum) >= math.Abs(v) {
			s.carry += (s.sum - t) + v
		} else {
			s.carry += (v - t) + s.sum
		}
		s.sum = t
	}
}

func (s *windowSum) value() float64 {
	switch {
	case s.nan > 0 || s.posInf > 0 && s.negInf > 0:
		return math.NaN()
	case s.posInf > 0:
		return math.Inf(1)
	case s.negInf > 0:
		return math.Inf(-1)
	}
	return s.sum + s.carry
}
