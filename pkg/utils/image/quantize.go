package imageutil

import (
	"cmp"
	"image"
	"image/color"
	"slices"
)

// Palette image of img with at most colors opaque entries, colors held to 1 through 256, found by cutting its colour space again and again where the cut leaves the least squared error, each pixel weighted by its opacity; every pixel, clear ones included, takes the entry nearest its colour
func Quantize(img *image.NRGBA, colors int) *image.Paletted {
	colors = min(max(colors, 1), 256)
	w, h := img.Rect.Dx(), img.Rect.Dy()
	swatches := histogram(img, true)
	if len(swatches) == 0 {
		swatches = histogram(img, false)
	}
	palette := medianCut(swatches, colors)
	out := image.NewPaletted(image.Rect(0, 0, w, h), palette)
	entries := make([][3]int32, len(palette))
	for i, c := range palette {
		n := c.(color.NRGBA)
		entries[i] = [3]int32{int32(n.R), int32(n.G), int32(n.B)}
	}
	cache := make(map[uint32]uint8)
	lastKey, lastIdx := uint32(1<<24), uint8(0)
	for y := range h {
		sp := img.Pix[y*img.Stride : y*img.Stride+4*w]
		dp := out.Pix[y*out.Stride : y*out.Stride+w]
		for x := range dp {
			r, g, b := sp[4*x], sp[4*x+1], sp[4*x+2]
			key := uint32(r)<<16 | uint32(g)<<8 | uint32(b)
			if key != lastKey {
				idx, ok := cache[key]
				if !ok {
					idx = nearestEntry(entries, int32(r), int32(g), int32(b))
					cache[key] = idx
				}
				lastKey, lastIdx = key, idx
			}
			dp[x] = lastIdx
		}
	}
	return out
}

// Most distinct colour keys the histogram holds before it coarsens every channel by one bit
const maxSwatches = 1 << 16

// Colours sharing one histogram key: their total weight, weighted channel sums and weighted sums of squared channels
type swatch struct {
	key        uint32
	w          float64
	r, g, b    float64
	rr, gg, bb float64
}

func (s *swatch) add(r, g, b uint8, w float64) {
	fr, fg, fb := float64(r), float64(g), float64(b)
	s.w += w
	s.r, s.g, s.b = s.r+w*fr, s.g+w*fg, s.b+w*fb
	s.rr, s.gg, s.bb = s.rr+w*fr*fr, s.gg+w*fg*fg, s.bb+w*fb*fb
}

func (s *swatch) merge(o *swatch) {
	s.w += o.w
	s.r, s.g, s.b = s.r+o.r, s.g+o.g, s.b+o.b
	s.rr, s.gg, s.bb = s.rr+o.rr, s.gg+o.gg, s.bb+o.bb
}

func (s *swatch) remove(o *swatch) {
	s.w -= o.w
	s.r, s.g, s.b = s.r-o.r, s.g-o.g, s.b-o.b
	s.rr, s.gg, s.bb = s.rr-o.rr, s.gg-o.gg, s.bb-o.bb
}

// Weighted squared distance of the colours from their mean
func (s *swatch) spread() float64 {
	if s.w <= 0 {
		return 0
	}
	return s.rr + s.gg + s.bb - (s.r*s.r+s.g*s.g+s.b*s.b)/s.w
}

// Weighted mean of one channel, 0 red, 1 green, 2 blue
func (s *swatch) mean(channel int) float64 {
	if s.w <= 0 {
		return 0
	}
	return [3]float64{s.r, s.g, s.b}[channel] / s.w
}

// Weighted spread of one channel alone
func (s *swatch) channelSpread(channel int) float64 {
	if s.w <= 0 {
		return 0
	}
	sum, sq := [3]float64{s.r, s.g, s.b}[channel], [3]float64{s.rr, s.gg, s.bb}[channel]
	return sq - sum*sum/s.w
}

func (s *swatch) color() color.NRGBA {
	return color.NRGBA{R: round8(s.mean(0)), G: round8(s.mean(1)), B: round8(s.mean(2)), A: 0xff}
}

func round8(v float64) uint8 { return uint8(min(max(v+0.5, 0), 255)) }

// Colour swatches of the image, weighted by opacity with clear pixels left out or every pixel counted once, keys losing a bit per channel whenever there are too many
func histogram(img *image.NRGBA, byAlpha bool) []swatch {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	index := make(map[uint32]int)
	var list []swatch
	shift := uint(0)
	for y := range h {
		row := img.Pix[y*img.Stride : y*img.Stride+4*w]
		for i := 0; i < len(row); i += 4 {
			weight := 1.0
			if byAlpha {
				if row[i+3] == 0 {
					continue
				}
				weight = float64(row[i+3]) / 0xff
			}
			r, g, b := row[i], row[i+1], row[i+2]
			key := uint32(r>>shift)<<16 | uint32(g>>shift)<<8 | uint32(b>>shift)
			k, ok := index[key]
			if !ok {
				k = len(list)
				index[key] = k
				list = append(list, swatch{key: key})
			}
			list[k].add(r, g, b, weight)
			for len(list) > maxSwatches {
				shift++
				list, index = coarsen(list)
			}
		}
	}
	return list
}

// Swatches merged under keys one bit coarser per channel, reusing the slice
func coarsen(list []swatch) ([]swatch, map[uint32]int) {
	index := make(map[uint32]int, len(list)/2)
	out := list[:0]
	for _, s := range list {
		s.key = s.key >> 1 & 0x7f7f7f
		if k, ok := index[s.key]; ok {
			out[k].merge(&s)
			continue
		}
		index[s.key] = len(out)
		out = append(out, s)
	}
	return out, index
}

// Run of swatches one palette entry stands for, with their totals
type box struct {
	lo, hi int
	total  swatch
}

// Palette of at most colors entries, the mean of each box: the box of swatches with the largest spread split again and again where the two halves spread least
func medianCut(list []swatch, colors int) color.Palette {
	if len(list) == 0 {
		return color.Palette{color.NRGBA{A: 0xff}}
	}
	boxes := []box{newBox(list, 0, len(list))}
	for len(boxes) < colors {
		pick, widest := -1, 0.0
		for i := range boxes {
			if s := boxes[i].total.spread(); boxes[i].hi-boxes[i].lo > 1 && s > widest {
				pick, widest = i, s
			}
		}
		if pick < 0 {
			break
		}
		left, right := splitBox(list, boxes[pick])
		boxes[pick] = left
		boxes = append(boxes, right)
	}
	palette := make(color.Palette, len(boxes))
	for i := range boxes {
		palette[i] = boxes[i].total.color()
	}
	return palette
}

func newBox(list []swatch, lo, hi int) box {
	b := box{lo: lo, hi: hi}
	for i := lo; i < hi; i++ {
		b.total.merge(&list[i])
	}
	return b
}

// Two boxes from one: swatches sorted along the channel that spreads most, cut where the halves spread least in total
func splitBox(list []swatch, b box) (box, box) {
	channel := 0
	for c := 1; c < 3; c++ {
		if b.total.channelSpread(c) > b.total.channelSpread(channel) {
			channel = c
		}
	}
	run := list[b.lo:b.hi]
	slices.SortFunc(run, func(x, y swatch) int { return cmp.Compare(x.mean(channel), y.mean(channel)) })
	var left swatch
	right := b.total
	cut, best := b.lo+1, 0.0
	for i := range len(run) - 1 {
		left.merge(&run[i])
		right.remove(&run[i])
		if cost := left.spread() + right.spread(); i == 0 || cost < best {
			cut, best = b.lo+i+1, cost
		}
	}
	return newBox(list, b.lo, cut), newBox(list, cut, b.hi)
}

// Index of the palette entry nearest a colour by squared distance, the first on ties
func nearestEntry(entries [][3]int32, r, g, b int32) uint8 {
	best, bestDist := 0, int32(1<<30)
	for i, e := range entries {
		dr := e[0] - r
		d := dr * dr
		if d >= bestDist {
			continue
		}
		dg := e[1] - g
		if d += dg * dg; d >= bestDist {
			continue
		}
		db := e[2] - b
		if d += db * db; d < bestDist {
			best, bestDist = i, d
		}
	}
	return uint8(best)
}
