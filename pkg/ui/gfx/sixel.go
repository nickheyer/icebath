package gfx

import "strconv"

// Encoder over a fixed color cube sized to the registers, ordered dither so no frame waits on a palette
type sixelEncoder struct {
	registers int
	levels    [3]int
	gray      bool
	colors    int
	lut       [3][64][256]uint8
	head      []byte
	out       []byte
	bits      []byte
	lo, hi    []int
	used      []int
	xs        []int
}

// Threshold order of the 8 by 8 Bayer dither
var bayer8 = [8][8]uint8{
	{0, 32, 8, 40, 2, 34, 10, 42},
	{48, 16, 56, 24, 50, 18, 58, 26},
	{12, 44, 4, 36, 14, 46, 6, 38},
	{60, 28, 52, 20, 62, 30, 54, 22},
	{3, 35, 11, 43, 1, 33, 9, 41},
	{51, 19, 59, 27, 49, 17, 57, 25},
	{15, 47, 7, 39, 13, 45, 5, 37},
	{63, 31, 55, 23, 61, 29, 53, 21},
}

// Levels per channel of the largest even cube the registers hold, green gaining levels in small palettes
func cube(registers int) (levels [3]int, gray bool) {
	if registers < 8 {
		return [3]int{max(registers, 2), 1, 1}, true
	}
	a := 2
	for (a+1)*(a+1)*(a+1) <= registers {
		a++
	}
	if a >= 4 {
		return [3]int{a, a, a}, false
	}
	g := a
	for a*(g+1)*a <= registers {
		g++
	}
	return [3]int{a, g, a}, false
}

// Level of a channel value among the levels, dithered by the Bayer threshold
func level(v, levels, threshold int) int {
	f := float64(v)*float64(levels-1)/255 + (float64(threshold)+0.5)/64
	return min(int(f), levels-1)
}

// Builds the lookup tables and palette for the registers, unknown registers taken as 256
func (e *sixelEncoder) configure(registers int) {
	if registers <= 0 {
		registers = 256
	}
	registers = min(registers, 256)
	if e.head != nil && e.registers == registers {
		return
	}
	e.registers = registers
	e.levels, e.gray = cube(registers)
	l := e.levels
	e.colors = l[0] * l[1] * l[2]
	weight := [3]int{l[1] * l[2], l[2], 1}
	for ch := range 3 {
		for t := range 64 {
			for v := range 256 {
				e.lut[ch][t][v] = uint8(level(v, l[ch], t) * weight[ch])
			}
		}
	}
	e.head = e.head[:0]
	for i := range e.colors {
		r, g, b := i/weight[0], i/weight[1]%l[1], i%l[2]
		pr, pg, pb := percent(r, l[0]), percent(g, l[1]), percent(b, l[2])
		if e.gray {
			pg, pb = pr, pr
		}
		e.head = append(e.head, '#')
		e.head = strconv.AppendInt(e.head, int64(i), 10)
		e.head = append(e.head, ";2;"...)
		e.head = strconv.AppendInt(e.head, int64(pr), 10)
		e.head = append(e.head, ';')
		e.head = strconv.AppendInt(e.head, int64(pg), 10)
		e.head = append(e.head, ';')
		e.head = strconv.AppendInt(e.head, int64(pb), 10)
	}
	e.bits, e.lo, e.hi = nil, nil, nil
}

// Share in percent of a level among the levels, rounded
func percent(i, levels int) int {
	if levels <= 1 {
		return 0
	}
	return (i*100 + (levels-1)/2) / (levels - 1)
}

// Sixel tiles covering cols by rows cells at the native cell size, each within the largest image the terminal takes
func (e *sixelEncoder) encode(raw []byte, w, h, cols, rows, cw, ch, registers, maxW, maxH int) []tile {
	e.configure(registers)
	W, H := cols*cw, rows*ch
	if cap(e.xs) < W {
		e.xs = make([]int, W)
	}
	e.xs = e.xs[:W]
	for X := range W {
		e.xs[X] = min(X*w/W, w-1) * 3
	}
	tc, tr := cols, rows
	if maxW > 0 {
		tc = max(maxW/cw, 1)
	}
	if maxH > 0 {
		tr = max(maxH/ch, 1)
	}
	var tiles []tile
	for r0 := 0; r0 < rows; r0 += tr {
		for c0 := 0; c0 < cols; c0 += tc {
			e.out = e.tile(e.out[:0], raw, w, h, H, c0*cw, r0*ch, min(c0+tc, cols)*cw, min(r0+tr, rows)*ch)
			tiles = append(tiles, tile{col: c0, row: r0, data: append([]byte(nil), e.out...)})
		}
	}
	return tiles
}

// Appends one sixel image of native pixels X0 to X1 across and Y0 to Y1 down, sampled from the frame
func (e *sixelEncoder) tile(out, raw []byte, w, h, H, X0, Y0, X1, Y1 int) []byte {
	tw, th := X1-X0, Y1-Y0
	if len(e.bits) < e.colors*tw {
		e.bits = make([]byte, e.colors*tw)
	}
	if len(e.hi) < e.colors {
		e.lo, e.hi = make([]int, e.colors), make([]int, e.colors)
		for c := range e.hi {
			e.hi[c] = -1
		}
	}
	out = append(out, "\x1bP0;1;q\"1;1;"...)
	out = strconv.AppendInt(out, int64(tw), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(th), 10)
	out = append(out, e.head...)
	lr, lg, lb := &e.lut[0], &e.lut[1], &e.lut[2]
	for y0 := 0; y0 < th; y0 += 6 {
		used := e.used[:0]
		for dy := range min(6, th-y0) {
			Y := Y0 + y0 + dy
			src := raw[(Y*h/H)*w*3 : (Y*h/H+1)*w*3]
			brow := &bayer8[Y&7]
			bit := byte(1) << dy
			for x := range tw {
				X := X0 + x
				si := e.xs[X]
				t := brow[X&7]
				var c int
				if e.gray {
					c = int(lr[t][(77*int(src[si])+150*int(src[si+1])+29*int(src[si+2]))>>8])
				} else {
					c = int(lr[t][src[si]]) + int(lg[t][src[si+1]]) + int(lb[t][src[si+2]])
				}
				switch {
				case e.hi[c] < 0:
					e.lo[c], e.hi[c] = x, x
					used = append(used, c)
				case x < e.lo[c]:
					e.lo[c] = x
				case x > e.hi[c]:
					e.hi[c] = x
				}
				e.bits[c*tw+x] |= bit
			}
		}
		for k, c := range used {
			if k > 0 {
				out = append(out, '$')
			}
			out = append(out, '#')
			out = strconv.AppendInt(out, int64(c), 10)
			out = appendRun(out, 0, e.lo[c])
			row := e.bits[c*tw+e.lo[c] : c*tw+e.hi[c]+1]
			run, last := 0, row[0]
			for _, v := range row {
				if v == last {
					run++
					continue
				}
				out = appendRun(out, last, run)
				run, last = 1, v
			}
			out = appendRun(out, last, run)
			clear(row)
			e.hi[c] = -1
		}
		e.used = used
		if y0+6 < th {
			out = append(out, '-')
		}
	}
	return append(out, "\x1b\\"...)
}

// Appends n sixels of the bits, run length packed past three
func appendRun(out []byte, bits byte, n int) []byte {
	if n <= 0 {
		return out
	}
	if n > 3 {
		out = append(out, '!')
		out = strconv.AppendInt(out, int64(n), 10)
		return append(out, 63+bits)
	}
	for range n {
		out = append(out, 63+bits)
	}
	return out
}
