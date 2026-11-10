package pet

import (
	"fmt"
	"image"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypil"
)

// Encodes a frame as DEC sixel with up to 255 adaptive colours, leaving transparent pixels unpainted
func encodeSixel(frame *image.NRGBA) string {
	pal, err := pypil.QuantizeMedianCut(frame, 255)
	if err != nil {
		panic(err)
	}
	w, h := pal.W, pal.H
	var out strings.Builder
	out.WriteString("\x1bP0;1;0q")
	fmt.Fprintf(&out, "\"1;1;%d;%d", w, h)
	var seen [256]bool
	for _, v := range pal.Pix {
		seen[v] = true
	}
	used := make([]int, 0, 256)
	for idx, ok := range seen {
		if ok {
			used = append(used, idx)
		}
	}
	for _, idx := range used {
		var rgb [3]int
		for c := 0; c < 3; c++ {
			if idx*3+c < len(pal.Palette) {
				rgb[c] = int(pal.Palette[idx*3+c])
			}
		}
		fmt.Fprintf(&out, "#%d;2;%d;%d;%d", idx, rgb[0]*100/255, rgb[1]*100/255, rgb[2]*100/255)
	}
	for band := 0; band < h; band += 6 {
		top := min(band+6, h)
		for _, colorIdx := range used {
			out.WriteByte('#')
			out.WriteString(strconv.Itoa(colorIdx))
			var prev byte
			run := 0
			for x := 0; x <= w; x++ {
				var ch byte
				if x < w {
					bits := 0
					for y := band; y < top; y++ {
						if frame.Pix[y*frame.Stride+x*4+3] > 32 && int(pal.Pix[y*w+x]) == colorIdx {
							bits |= 1 << (y - band)
						}
					}
					ch = byte(63 + bits)
					if run > 0 && ch == prev {
						run++
						continue
					}
				}
				if run > 3 {
					out.WriteByte('!')
					out.WriteString(strconv.Itoa(run))
					out.WriteByte(prev)
				} else {
					for k := 0; k < run; k++ {
						out.WriteByte(prev)
					}
				}
				prev, run = ch, 1
			}
			out.WriteByte('$')
		}
		out.WriteByte('-')
	}
	out.WriteString("\x1b\\")
	return out.String()
}
