package mathutil

import (
	"errors"
	"fmt"
)

// Error for a negative number of samples
var ErrNegativeCount = errors.New("negative sample count")

// n evenly spaced values from start towards stop, the last one exactly stop when endpoint is set and one step short of it otherwise
func Linspace(start, stop float64, n int, endpoint bool) ([]float64, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: %d", ErrNegativeCount, n)
	}
	out := make([]float64, n)
	div := n
	if endpoint {
		div = n - 1
	}
	if div <= 0 {
		if n > 0 {
			out[0] = start
		}
		return out, nil
	}
	delta := stop - start
	step := delta / float64(div)
	if step == 0 && delta != 0 {
		for i := range out {
			out[i] = start + float64(float64(i)/float64(div)*delta)
		}
	} else {
		for i := range out {
			out[i] = start + float64(float64(i)*step)
		}
	}
	if endpoint {
		out[n-1] = stop
	}
	return out, nil
}
