package vxl

import (
	"math"
	"runtime"
	"sync"
)

// Runs fn over bands of rows side by side, one goroutine per band
func parallelRows(h int, fn func(y0, y1 int)) {
	workers := min(runtime.NumCPU(), max(h/8, 1))
	if workers <= 1 {
		fn(0, h)
		return
	}
	band := (h + workers - 1) / workers
	var wg sync.WaitGroup
	for y0 := 0; y0 < h; y0 += band {
		y1 := min(y0+band, h)
		wg.Go(func() { fn(y0, y1) })
	}
	wg.Wait()
}

// Pixel grid with a depth per pixel and the shape of its pixels on screen
type Frame struct {
	W, H  int
	Color []RGB
	// One over the view depth of the pixel, zero where nothing was drawn
	Depth []float32
	// Width of one pixel over its height as the screen shows it
	PixelAspect float64
}

// Frame of the size, the buffers reused when the size did not change
func (fr *Frame) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	if fr.PixelAspect == 0 {
		fr.PixelAspect = 1
	}
	if fr.W != w || fr.H != h || fr.Color == nil {
		fr.W, fr.H = w, h
		fr.Color = make([]RGB, w*h)
		fr.Depth = make([]float32, w*h)
	}
}

// Paints the background from a function of the pixel and clears the depth
func (fr *Frame) Clear(bg func(x, y int) RGB) {
	for y := range fr.H {
		row := fr.Color[y*fr.W : (y+1)*fr.W]
		for x := range row {
			row[x] = bg(x, y)
		}
	}
	clear(fr.Depth)
}

// Copies the pixels and depths of a frame of the same shape
func (fr *Frame) CopyFrom(o *Frame) {
	fr.PixelAspect = o.PixelAspect
	fr.Resize(o.W, o.H)
	copy(fr.Color, o.Color)
	copy(fr.Depth, o.Depth)
}

// Pixel color, the nearest pixel of the frame past its edge
func (fr *Frame) At(x, y int) RGB {
	if fr.W == 0 || fr.H == 0 {
		return RGB{}
	}
	x, y = min(max(x, 0), fr.W-1), min(max(y, 0), fr.H-1)
	return fr.Color[y*fr.W+x]
}

// Scratch buffers of the bloom pass, reused across frames
type Post struct {
	small []RGB
	blur  []RGB
	sw    int
	sh    int
}

// Factor the bloom pass shrinks the frame by before blurring
const bloomShrink = 4

// Spreads the light of bright pixels into their surroundings, the glow added at the strength
func (fr *Frame) Bloom(p *Post, threshold, strength float64) {
	if fr.W == 0 || fr.H == 0 || strength <= 0 {
		return
	}
	sw, sh := (fr.W+bloomShrink-1)/bloomShrink, (fr.H+bloomShrink-1)/bloomShrink
	if p.sw != sw || p.sh != sh {
		p.sw, p.sh = sw, sh
		p.small = make([]RGB, sw*sh)
		p.blur = make([]RGB, sw*sh)
	}
	clear(p.small)
	inv := 1 / float64(bloomShrink*bloomShrink)
	parallelRows(sh, func(s0, s1 int) {
		for y := s0 * bloomShrink; y < min(s1*bloomShrink, fr.H); y++ {
			sy := y / bloomShrink
			row := fr.Color[y*fr.W : (y+1)*fr.W]
			for x, c := range row {
				l := c.Luma()
				if l <= threshold {
					continue
				}
				k := (l - threshold) / l * inv
				i := sy*sw + x/bloomShrink
				p.small[i] = p.small[i].Add(c.Scale(k))
			}
		}
	})
	weights := [5]float64{0.0625, 0.25, 0.375, 0.25, 0.0625}
	for range 2 {
		for y := range sh {
			for x := range sw {
				var acc RGB
				for k, w := range weights {
					sx := min(max(x+k-2, 0), sw-1)
					acc = acc.Add(p.small[y*sw+sx].Scale(w))
				}
				p.blur[y*sw+x] = acc
			}
		}
		for y := range sh {
			for x := range sw {
				var acc RGB
				for k, w := range weights {
					sy := min(max(y+k-2, 0), sh-1)
					acc = acc.Add(p.blur[sy*sw+x].Scale(w))
				}
				p.small[y*sw+x] = acc
			}
		}
	}
	lit := make([]bool, sh)
	for y := range sh {
		for x := range sw {
			if c := p.small[y*sw+x]; c.R > 0 || c.G > 0 || c.B > 0 {
				lit[y] = true
				break
			}
		}
	}
	parallelRows(fr.H, func(r0, r1 int) {
		mixed := make([]RGB, sw)
		for y := r0; y < r1; y++ {
			fy := (float64(y)+0.5)/bloomShrink - 0.5
			y0 := min(max(int(math.Floor(fy)), 0), sh-1)
			y1 := min(y0+1, sh-1)
			if !lit[y0] && !lit[y1] {
				continue
			}
			ty := min(max(fy-float64(y0), 0), 1)
			for x := range sw {
				mixed[x] = p.small[y0*sw+x].Mix(p.small[y1*sw+x], ty).Scale(strength)
			}
			row := fr.Color[y*fr.W : (y+1)*fr.W]
			for x := range row {
				fx := (float64(x)+0.5)/bloomShrink - 0.5
				x0 := min(max(int(math.Floor(fx)), 0), sw-1)
				x1 := min(x0+1, sw-1)
				a, b := mixed[x0], mixed[x1]
				if a == (RGB{}) && b == (RGB{}) {
					continue
				}
				tx := min(max(fx-float64(x0), 0), 1)
				row[x] = row[x].Add(a.Mix(b, tx))
			}
		}
	})
}

// Darkens the pixels toward the edges of the frame by the strength
func (fr *Frame) Vignette(strength float64) {
	if fr.W == 0 || fr.H == 0 || strength <= 0 {
		return
	}
	parallelRows(fr.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			dy := (float64(y)+0.5)/float64(fr.H)*2 - 1
			row := fr.Color[y*fr.W : (y+1)*fr.W]
			for x := range row {
				dx := (float64(x)+0.5)/float64(fr.W)*2 - 1
				d := dx*dx + dy*dy*0.7
				k := 1 - strength*Smoothstep((d-0.35)/1.3)
				row[x] = row[x].Scale(k)
			}
		}
	})
}
