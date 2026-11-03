package gfx

import (
	"bytes"
	"strconv"
)

// Levels each channel of the sixel palette takes
const sixelLevels = 6

// Colors of the sixel palette, a cube of the levels
const sixelColors = sixelLevels * sixelLevels * sixelLevels

// Sixel encoder over a fixed color cube with an ordered dither, so no frame waits on a palette
type sixel struct {
	idx     []uint8
	present [sixelColors]bool
	head    []byte
}

// Bayer matrix the dither reads its threshold from
var bayer = [4][4]int{{0, 8, 2, 10}, {12, 4, 14, 6}, {3, 11, 1, 9}, {15, 7, 13, 5}}

// Palette index of a channel level dithered by a threshold share
func level(v uint8, share float64) int {
	f := float64(v)/255*(sixelLevels-1) + share - 0.5
	return min(max(int(f+0.5), 0), sixelLevels-1)
}

// Writes the raster, the palette and the bands of the pixels, three bytes each
func (s *sixel) encode(out *bytes.Buffer, rgb []byte, w, h int) {
	if len(s.head) == 0 {
		var hb bytes.Buffer
		for i := range sixelColors {
			r, g, b := i/(sixelLevels*sixelLevels), i/sixelLevels%sixelLevels, i%sixelLevels
			hb.WriteByte('#')
			hb.WriteString(strconv.Itoa(i))
			hb.WriteString(";2;")
			hb.WriteString(strconv.Itoa(r * 100 / (sixelLevels - 1)))
			hb.WriteByte(';')
			hb.WriteString(strconv.Itoa(g * 100 / (sixelLevels - 1)))
			hb.WriteByte(';')
			hb.WriteString(strconv.Itoa(b * 100 / (sixelLevels - 1)))
		}
		s.head = hb.Bytes()
	}
	out.WriteString("\"1;1;")
	out.WriteString(strconv.Itoa(w))
	out.WriteByte(';')
	out.WriteString(strconv.Itoa(h))
	out.Write(s.head)
	if cap(s.idx) < w*h {
		s.idx = make([]uint8, w*h)
	}
	s.idx = s.idx[:w*h]
	for y := range h {
		for x := range w {
			i := y*w + x
			share := (float64(bayer[y&3][x&3]) + 0.5) / 16
			r, g, b := level(rgb[i*3], share), level(rgb[i*3+1], share), level(rgb[i*3+2], share)
			s.idx[i] = uint8((r*sixelLevels+g)*sixelLevels + b)
		}
	}
	var bits []byte
	if cap(bits) < w {
		bits = make([]byte, w)
	}
	for y0 := 0; y0 < h; y0 += 6 {
		s.present = [sixelColors]bool{}
		rows := min(6, h-y0)
		for dy := range rows {
			row := s.idx[(y0+dy)*w : (y0+dy+1)*w]
			for _, c := range row {
				s.present[c] = true
			}
		}
		first := true
		for c := range sixelColors {
			if !s.present[c] {
				continue
			}
			if !first {
				out.WriteByte('$')
			}
			first = false
			out.WriteByte('#')
			out.WriteString(strconv.Itoa(c))
			for x := range w {
				bits[x] = 0
			}
			for dy := range rows {
				row := s.idx[(y0+dy)*w : (y0+dy+1)*w]
				bit := byte(1 << dy)
				for x, v := range row {
					if int(v) == c {
						bits[x] |= bit
					}
				}
			}
			run, last := 0, byte(0)
			flush := func() {
				if run == 0 {
					return
				}
				if run > 3 {
					out.WriteByte('!')
					out.WriteString(strconv.Itoa(run))
					out.WriteByte(63 + last)
					return
				}
				for range run {
					out.WriteByte(63 + last)
				}
			}
			for _, b := range bits[:w] {
				if b == last {
					run++
					continue
				}
				flush()
				run, last = 1, b
			}
			flush()
		}
		out.WriteByte('-')
	}
}
