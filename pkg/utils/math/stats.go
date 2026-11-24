package mathutil

import (
	"math"
	"slices"
)

// Signed integer and floating point sample types
type sample interface {
	~int8 | ~int16 | ~int32 | ~int64 | ~int | ~float32 | ~float64
}

// Root mean square of the samples, zero when there are none
func RMS[T sample](samples []T) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		f := float64(v)
		sum += f * f
	}
	if sum > 0x1p-900 && sum <= math.MaxFloat64 {
		return math.Sqrt(sum / float64(len(samples)))
	}
	return scaledRMS(samples)
}

// Root mean square computed against the largest magnitude, for squares that overflow or underflow; NaN when a sample is NaN, infinite when one is infinite
func scaledRMS[T sample](samples []T) float64 {
	var scale float64
	for _, v := range samples {
		scale = max(scale, math.Abs(float64(v)))
	}
	if scale == 0 || math.IsInf(scale, 1) || math.IsNaN(scale) {
		return scale
	}
	var sum float64
	for _, v := range samples {
		f := float64(v) / scale
		sum += f * f
	}
	return scale * math.Sqrt(sum/float64(len(samples)))
}

// Value p percent of the way through the sorted values, interpolated linearly between the two nearest ranks with p clamped to 0..100; NaN for no values, a NaN p or a NaN among the values
func Percentile(values []float64, p float64) float64 {
	n := len(values)
	if n == 0 || math.IsNaN(p) {
		return math.NaN()
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	if math.IsNaN(sorted[0]) {
		return math.NaN()
	}
	rank := float64(n-1) * min(max(p, 0), 100) / 100
	lo := int(rank)
	if lo >= n-1 {
		return sorted[n-1]
	}
	a, b, frac := sorted[lo], sorted[lo+1], rank-float64(lo)
	if frac == 0 || a == b {
		return a
	}
	if d := b - a; !math.IsInf(d, 0) {
		return a + d*frac
	}
	return a*(1-frac) + b*frac
}
