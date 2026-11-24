package imageutil

import (
	"image"
	"math"
	"runtime"
	"sync"
)

// Resampling kernel with the radius it reaches at unit scale
type kernel struct {
	support float64
	at      func(float64) float64
}

func (f Filter) kernel() kernel {
	if f == CatmullRom {
		return kernel{2, catmullRom}
	}
	return kernel{3, lanczos3}
}

func lanczos3(x float64) float64 {
	x = math.Abs(x)
	if x >= 3 {
		return 0
	}
	if x < 1e-12 {
		return 1
	}
	px := math.Pi * x
	return 3 * math.Sin(px) * math.Sin(px/3) / (px * px)
}

func catmullRom(x float64) float64 {
	x = math.Abs(x)
	switch {
	case x < 1:
		return (1.5*x-2.5)*x*x + 1
	case x < 2:
		return ((-0.5*x+2.5)*x-4)*x + 2
	}
	return 0
}

// Source window and normalised weights of every output position along one axis, taps weights per position
type axis struct {
	start, count []int
	taps         int
	weights      []float32
	identity     bool
}

// Weights of a kernel widened by the scale factor when shrinking, sampled at source pixel centres; no window is wider than the source, so taps never exceeds in
func newAxis(in, out int, k kernel) axis {
	a := axis{start: make([]int, out), count: make([]int, out)}
	if in == out {
		a.taps, a.identity = 1, true
		a.weights = make([]float32, out)
		for i := range out {
			a.start[i], a.count[i], a.weights[i] = i, 1, 1
		}
		return a
	}
	scale := float64(in) / float64(out)
	stretch := max(scale, 1)
	support := k.support * stretch
	a.taps = min(2*int(math.Ceil(support))+1, in)
	a.weights = make([]float32, out*a.taps)
	raw := make([]float64, a.taps)
	for i := range out {
		center := (float64(i) + 0.5) * scale
		lo := max(int(math.Floor(center-support+0.5)), 0)
		hi := min(int(math.Floor(center+support+0.5)), in)
		sum := 0.0
		for t := range hi - lo {
			raw[t] = k.at((float64(lo+t) + 0.5 - center) / stretch)
			sum += raw[t]
		}
		w := a.weights[i*a.taps : (i+1)*a.taps]
		if hi <= lo || sum == 0 {
			lo = min(max(int(center), 0), in-1)
			hi = lo + 1
			w[0] = 1
		} else {
			for t := range hi - lo {
				w[t] = float32(raw[t] / sum)
			}
		}
		a.start[i], a.count[i] = lo, hi-lo
	}
	return a
}

// Separable resample of src into dst, rows filtered across on demand into a ring the down pass reads, colours premultiplied by alpha in between
func resample(dst, src *image.NRGBA, k kernel) {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := dst.Rect.Dx(), dst.Rect.Dy()
	across, down := newAxis(sw, dw, k), newAxis(sh, dh, k)
	parallelRows(dh, down.taps, dw*dh*(across.taps+down.taps), func(lo, hi int) {
		ring := newRowRing(src, &across, down.taps)
		acc := make([]float32, 4*dw)
		for y := lo; y < hi; y++ {
			clear(acc)
			first, n := down.start[y], down.count[y]
			for t, wt := range down.weights[y*down.taps : y*down.taps+n] {
				row := ring.row(first + t)
				for i, v := range row {
					acc[i] += wt * v
				}
			}
			storeRow(dst.Pix[y*dst.Stride:y*dst.Stride+4*dw], acc)
		}
	})
}

// Source rows filtered across and premultiplied, kept in slots by row number modulo the ring length
type rowRing struct {
	src    *image.NRGBA
	across *axis
	line   []float32
	slots  [][]float32
	held   []int
}

func newRowRing(src *image.NRGBA, across *axis, size int) *rowRing {
	dw := len(across.start)
	r := &rowRing{src: src, across: across, line: make([]float32, 4*src.Rect.Dx()), slots: make([][]float32, size), held: make([]int, size)}
	cells := make([]float32, size*4*dw)
	for i := range r.slots {
		r.slots[i] = cells[i*4*dw : (i+1)*4*dw : (i+1)*4*dw]
		r.held[i] = -1
	}
	return r
}

// Filtered source row sy, computed once and reused while it stays in the ring
func (r *rowRing) row(sy int) []float32 {
	slot := sy % len(r.slots)
	out := r.slots[slot]
	if r.held[slot] == sy {
		return out
	}
	r.held[slot] = sy
	sw := r.src.Rect.Dx()
	sp := r.src.Pix[sy*r.src.Stride : sy*r.src.Stride+4*sw]
	line := r.line
	if r.across.identity {
		line = out
	}
	for i := 0; i < len(sp); i += 4 {
		a := float32(sp[i+3])
		line[i], line[i+1], line[i+2], line[i+3] = float32(sp[i])*a, float32(sp[i+1])*a, float32(sp[i+2])*a, a
	}
	if r.across.identity {
		return out
	}
	taps := r.across.taps
	for x := range len(out) / 4 {
		first, n := r.across.start[x], r.across.count[x]
		px := line[4*first : 4*(first+n)]
		var cr, cg, cb, ca float32
		for t, wt := range r.across.weights[x*taps : x*taps+n] {
			cr += wt * px[4*t]
			cg += wt * px[4*t+1]
			cb += wt * px[4*t+2]
			ca += wt * px[4*t+3]
		}
		out[4*x], out[4*x+1], out[4*x+2], out[4*x+3] = cr, cg, cb, ca
	}
	return out
}

// Writes premultiplied accumulators back as straight alpha bytes, a pixel below half an alpha step becoming clear black
func storeRow(dp []uint8, acc []float32) {
	for i := 0; i < len(acc); i += 4 {
		a := acc[i+3]
		if a < 0.5 {
			dp[i], dp[i+1], dp[i+2], dp[i+3] = 0, 0, 0, 0
			continue
		}
		dp[i], dp[i+1], dp[i+2], dp[i+3] = clamp8(acc[i]/a), clamp8(acc[i+1]/a), clamp8(acc[i+2]/a), clamp8(a)
	}
}

func clamp8(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
}

// Multiply adds below which splitting the rows across goroutines costs more than it saves
const parallelWork = 1 << 18

// Runs fn over consecutive bands of rows 0 to n on up to GOMAXPROCS goroutines when the work pays for them, each band long against the overlap it recomputes
func parallelRows(n, overlap, work int, fn func(lo, hi int)) {
	workers := min(runtime.GOMAXPROCS(0), work/parallelWork, n/max(4*overlap, 16))
	if workers <= 1 {
		fn(0, n)
		return
	}
	band := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += band {
		wg.Go(func() { fn(lo, min(lo+band, n)) })
	}
	wg.Wait()
}
