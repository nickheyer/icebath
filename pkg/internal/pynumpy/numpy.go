package pynumpy

import (
	"fmt"
	"math"
	"slices"
)

// ValueError is the ValueError numpy raises for a negative sample count
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Evenly spaced float64 samples computed step by step like numpy.linspace, the endpoint pinned to stop
func Linspace(start, stop float64, num int, endpoint bool) ([]float64, error) {
	if num < 0 {
		return nil, &ValueError{Msg: fmt.Sprintf("Number of samples, %d, must be non-negative.", num)}
	}
	div := num
	if endpoint {
		div = num - 1
	}
	y := make([]float64, num)
	delta := stop - start
	switch step := delta / float64(div); {
	case div > 0 && step == 0:
		for i := range y {
			y[i] = float64(float64(i)/float64(div)) * delta
		}
	case div > 0:
		for i := range y {
			y[i] = float64(float64(i) * step)
		}
	default:
		for i := range y {
			y[i] = float64(float64(i) * delta)
		}
	}
	for i := range y {
		y[i] += start
	}
	if endpoint && num > 1 {
		y[num-1] = stop
	}
	return y, nil
}

// Root mean square of int16 samples as numpy computes sqrt(mean(x.astype(float64) ** 2)) with pairwise summation
func RMSInt16(data []int16) float64 {
	squares := make([]float64, len(data))
	for i, v := range data {
		f := float64(v)
		squares[i] = float64(f * f)
	}
	return math.Sqrt(pairwiseSum(squares) / float64(len(data)))
}

// Sum in numpy's pairwise order, eight accumulators per block of at most 128 and halving above that
func pairwiseSum(a []float64) float64 {
	n := len(a)
	if n < 8 {
		res := 0.0
		for _, v := range a {
			res += v
		}
		return res
	}
	if n <= 128 {
		var r [8]float64
		copy(r[:], a[:8])
		i := 8
		for ; i < n-n%8; i += 8 {
			for j := range r {
				r[j] += a[i+j]
			}
		}
		res := (float64(r[0]+r[1]) + float64(r[2]+r[3])) + (float64(r[4]+r[5]) + float64(r[6]+r[7]))
		for ; i < n; i++ {
			res += a[i]
		}
		return res
	}
	n2 := n / 2
	n2 -= n2 % 8
	return pairwiseSum(a[:n2]) + pairwiseSum(a[n2:])
}

// Percentile with numpy's default linear method, interpolating from the upper neighbour past the midpoint
func Percentile(values []float64, q float64) float64 {
	a := append([]float64(nil), values...)
	slices.Sort(a)
	n := len(a)
	virtual := float64(n-1) * (q / 100)
	prev := int(math.Floor(virtual))
	next := prev + 1
	if virtual >= float64(n-1) {
		prev, next = n-1, n-1
	}
	gamma := virtual - math.Floor(virtual)
	if prev == n-1 {
		gamma = virtual + 1
	}
	diff := a[next] - a[prev]
	if gamma >= 0.5 {
		return a[next] - float64(diff*(1-gamma))
	}
	return a[prev] + float64(diff*gamma)
}
