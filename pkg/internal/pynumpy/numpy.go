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

// Result of np.convolve(a, np.ones(64) / 64.0, mode="same"), the longer input setting the length
func ConvolveSameBox64(a []float64) ([]float64, error) {
	if len(a) == 0 {
		return nil, &ValueError{Msg: "a cannot be empty"}
	}
	v := make([]float64, 64)
	for i := range v {
		v[i] = 1.0 / 64.0
	}
	if len(v) > len(a) {
		a, v = v, a
	}
	k := slices.Clone(v)
	slices.Reverse(k)
	return correlateSame(a, k), nil
}

// Correlation in mode same walked as numpy's _pyarray_correlate, growing then full then shrinking overlaps
func correlateSame(a, k []float64) []float64 {
	n1, n2 := len(a), len(k)
	nLeft := n2 / 2
	nRight := n2 - nLeft - 1
	out := make([]float64, 0, n1)
	n := n2 - nLeft
	for i := range nLeft {
		out = append(out, doubleDot(a[:n], k[nLeft-i:nLeft-i+n]))
		n++
	}
	p := 0
	for ; p < n1-n2+1; p++ {
		out = append(out, doubleDot(a[p:p+n], k[:n]))
	}
	for range nRight {
		n--
		out = append(out, doubleDot(a[p:p+n], k[:n]))
		p++
	}
	return out
}

// Float64 dot as numpy's DOUBLE_dot adds one cblas_ddot chunk onto zero
func doubleDot(x, y []float64) float64 {
	sum := 0.0
	sum += haswellDdot(x, y)
	return sum
}

// Single thread OpenBLAS Haswell ddot, sixteen fused lanes per block reduced pairwise then a plain tail
func haswellDdot(x, y []float64) float64 {
	n := len(x)
	dot := 0.0
	n1 := n &^ 15
	if n1 > 0 {
		var acc [16]float64
		for i := 0; i < n1; i += 16 {
			for j := range acc {
				acc[j] = math.FMA(x[i+j], y[i+j], acc[j])
			}
		}
		var lane [4][2]float64
		for r := range lane {
			lane[r][0] = acc[4*r] + acc[4*r+2]
			lane[r][1] = acc[4*r+1] + acc[4*r+3]
		}
		lo := float64(lane[0][0]+lane[1][0]) + float64(lane[2][0]+lane[3][0])
		hi := float64(lane[0][1]+lane[1][1]) + float64(lane[2][1]+lane[3][1])
		dot = lo + hi
	}
	for i := n1; i < n; i++ {
		dot += float64(y[i] * x[i])
	}
	return dot
}
