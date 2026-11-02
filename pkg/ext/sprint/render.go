package sprint

import (
	"image/color"
	"math"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Color with channels from 0 to 1, open to arithmetic
type RGB struct{ R, G, B float64 }

// Color from a 24 bit value
func hexRGB(v uint32) RGB {
	return RGB{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}
}

// Color from any color, black for nil
func fromColor(c color.Color) RGB {
	if c == nil {
		return RGB{}
	}
	r, g, b, _ := c.RGBA()
	return RGB{float64(r) / 65535, float64(g) / 65535, float64(b) / 65535}
}

// Opaque color with every channel clamped
func (c RGB) Color() color.RGBA {
	return color.RGBA{R: channel(c.R), G: channel(c.G), B: channel(c.B), A: 0xff}
}

func channel(v float64) uint8 { return uint8(math.Round(min(max(v, 0), 1) * 255)) }

// Color t of the way from c to o
func (c RGB) Mix(o RGB, t float64) RGB {
	t = min(max(t, 0), 1)
	return RGB{c.R + (o.R-c.R)*t, c.G + (o.G-c.G)*t, c.B + (o.B-c.B)*t}
}

// Color with every channel multiplied
func (c RGB) Scale(f float64) RGB { return RGB{c.R * f, c.G * f, c.B * f} }

// Sum of two colors
func (c RGB) Add(o RGB) RGB { return RGB{c.R + o.R, c.G + o.G, c.B + o.B} }

// Squared distance between two colors
func (c RGB) Dist(o RGB) float64 {
	dr, dg, db := c.R-o.R, c.G-o.G, c.B-o.B
	return dr*dr + dg*dg + db*db
}

// Perceived brightness from 0 to 1
func (c RGB) Luma() float64 { return 0.2126*c.R + 0.7152*c.G + 0.0722*c.B }

// Point or direction in the world, x east, y up, z south
type Vec3 struct{ X, Y, Z float64 }

func (a Vec3) Add(b Vec3) Vec3             { return Vec3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a Vec3) Sub(b Vec3) Vec3             { return Vec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a Vec3) Mul(f float64) Vec3          { return Vec3{a.X * f, a.Y * f, a.Z * f} }
func (a Vec3) Dot(b Vec3) float64          { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a Vec3) Len() float64                { return math.Sqrt(a.Dot(a)) }
func (a Vec3) Lerp(b Vec3, t float64) Vec3 { return a.Add(b.Sub(a).Mul(t)) }

func (a Vec3) Cross(b Vec3) Vec3 {
	return Vec3{a.Y*b.Z - a.Z*b.Y, a.Z*b.X - a.X*b.Z, a.X*b.Y - a.Y*b.X}
}

// Vector of length one, the zero vector staying zero
func (a Vec3) Norm() Vec3 {
	l := a.Len()
	if l == 0 {
		return a
	}
	return a.Mul(1 / l)
}

// Point turned by yaw radians around the y axis
func (a Vec3) RotateY(yaw float64) Vec3 {
	s, c := math.Sincos(yaw)
	return Vec3{a.X*c + a.Z*s, a.Y, -a.X*s + a.Z*c}
}

// Orbit camera looking at a target from a yaw, a pitch and a distance
type Camera struct {
	Target Vec3
	Yaw    float64
	Pitch  float64
	Dist   float64
	FOV    float64
}

// Position of the camera
func (c Camera) Eye() Vec3 {
	cp, sp := math.Cos(c.Pitch), math.Sin(c.Pitch)
	return c.Target.Add(Vec3{cp * math.Sin(c.Yaw), sp, cp * math.Cos(c.Yaw)}.Mul(c.Dist))
}

// Nearest view depth a face may have and still be drawn
const nearPlane = 0.25

// Pixel grid with a depth per pixel, each pixel twice as tall as wide so a cell holds four
type Frame struct {
	W, H  int
	Color []RGB
	// One over the view depth of the pixel, zero where nothing was drawn
	Depth []float32
}

// Frame of the size, the buffers reused when the size did not change
func (fr *Frame) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	if fr.W != w || fr.H != h || fr.Color == nil {
		fr.W, fr.H = w, h
		fr.Color = make([]RGB, w*h)
		fr.Depth = make([]float32, w*h)
	}
}

// Paints the background from a function of the pixel and clears the depth
func (fr *Frame) Clear(bg func(x, y int) RGB) {
	for y := range fr.H {
		for x := range fr.W {
			i := y*fr.W + x
			fr.Color[i] = bg(x, y)
			fr.Depth[i] = 0
		}
	}
}

// Flat face in the world with a color, a light per corner and how it blends
type Quad struct {
	P     [4]Vec3
	Color RGB
	Light [4]float64
	// Share of the face laid over what is behind it, one covering it
	Alpha float64
	// Light added instead of laid over, for glows
	Additive bool
	// Face drawn from either side, for decals and billboards
	TwoSided bool
}

// Corner of a face in view space with its light
type vertex struct {
	p     Vec3
	light float64
}

// Draws faces into a frame through a camera with a sun, fog and a ground glow
type Raster struct {
	fr      *Frame
	cam     Camera
	eye     Vec3
	right   Vec3
	up      Vec3
	forward Vec3
	fx, fy  float64
	// Fog toward a color between two view depths
	FogColor RGB
	FogNear  float64
	FogFar   float64
	scratch  []vertex
}

// Points the raster at the frame through the camera
func (r *Raster) Begin(fr *Frame, cam Camera) {
	r.fr, r.cam = fr, cam
	r.eye = cam.Eye()
	r.forward = cam.Target.Sub(r.eye).Norm()
	r.right = r.forward.Cross(Vec3{0, 1, 0}).Norm()
	if r.right.Len() == 0 {
		r.right = Vec3{1, 0, 0}
	}
	r.up = r.right.Cross(r.forward)
	f := 1 / math.Tan(cam.FOV/2)
	aspect := float64(max(fr.W, 1)) / float64(2*max(fr.H, 1))
	r.fx, r.fy = f/aspect, f
}

// Camera basis the raster draws through
func (r *Raster) Basis() (right, up, forward Vec3) { return r.right, r.up, r.forward }

// Point in view space, z growing away from the camera
func (r *Raster) view(p Vec3) Vec3 {
	d := p.Sub(r.eye)
	return Vec3{d.Dot(r.right), d.Dot(r.up), d.Dot(r.forward)}
}

// Pixel of a view space point and one over its depth
func (r *Raster) screen(v Vec3) (sx, sy, invz float64) {
	invz = 1 / v.Z
	sx = (v.X*invz*r.fx*0.5 + 0.5) * float64(r.fr.W)
	sy = (0.5 - v.Y*invz*r.fy*0.5) * float64(r.fr.H)
	return sx, sy, invz
}

// Pixel of a world point and its view depth, false behind the camera
func (r *Raster) Project(p Vec3) (sx, sy, depth float64, ok bool) {
	v := r.view(p)
	if v.Z < nearPlane {
		return 0, 0, 0, false
	}
	sx, sy, _ = r.screen(v)
	return sx, sy, v.Z, true
}

// Pixels one world unit spans at a view depth, sideways
func (r *Raster) Scale(depth float64) float64 {
	if depth <= 0 {
		return 0
	}
	return r.fx * 0.5 * float64(r.fr.W) / depth
}

// Draws a face, clipped to the near plane, back faces skipped unless it is two sided
func (r *Raster) Quad(q Quad) {
	if q.Alpha <= 0 {
		return
	}
	if !q.TwoSided {
		n := q.P[1].Sub(q.P[0]).Cross(q.P[3].Sub(q.P[0]))
		if n.Dot(r.eye.Sub(q.P[0])) <= 0 {
			return
		}
	}
	poly := r.scratch[:0]
	for i := range 4 {
		poly = append(poly, vertex{p: r.view(q.P[i]), light: q.Light[i]})
	}
	poly = clipNear(poly)
	r.scratch = poly
	if len(poly) < 3 {
		return
	}
	for i := 1; i+1 < len(poly); i++ {
		r.tri(poly[0], poly[i], poly[i+1], q)
	}
}

// Polygon cut to the part in front of the near plane, lights lerped along cut edges
func clipNear(in []vertex) []vertex {
	out := make([]vertex, 0, len(in)+1)
	for i := range in {
		a, b := in[i], in[(i+1)%len(in)]
		ain, bin := a.p.Z >= nearPlane, b.p.Z >= nearPlane
		if ain {
			out = append(out, a)
		}
		if ain != bin {
			t := (nearPlane - a.p.Z) / (b.p.Z - a.p.Z)
			out = append(out, vertex{p: a.p.Lerp(b.p, t), light: a.light + (b.light-a.light)*t})
		}
	}
	return out
}

// Fills one view space triangle with depth test, perspective correct light, fog and blending
func (r *Raster) tri(a, b, c vertex, q Quad) {
	fr := r.fr
	ax, ay, az := r.screen(a.p)
	bx, by, bz := r.screen(b.p)
	cx, cy, cz := r.screen(c.p)
	area := (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
	if math.Abs(area) < 1e-9 {
		return
	}
	x0 := max(int(math.Floor(min(ax, bx, cx))), 0)
	x1 := min(int(math.Ceil(max(ax, bx, cx))), fr.W-1)
	y0 := max(int(math.Floor(min(ay, by, cy))), 0)
	y1 := min(int(math.Ceil(max(ay, by, cy))), fr.H-1)
	if x0 > x1 || y0 > y1 {
		return
	}
	inv := 1 / area
	la, lb, lc := a.light*az, b.light*bz, c.light*cz
	write := q.Alpha >= 1 && !q.Additive
	for py := y0; py <= y1; py++ {
		yc := float64(py) + 0.5
		for px := x0; px <= x1; px++ {
			xc := float64(px) + 0.5
			w0 := ((bx-xc)*(cy-yc) - (by-yc)*(cx-xc)) * inv
			w1 := ((cx-xc)*(ay-yc) - (cy-yc)*(ax-xc)) * inv
			w2 := 1 - w0 - w1
			if w0 < 0 || w1 < 0 || w2 < 0 {
				continue
			}
			invz := w0*az + w1*bz + w2*cz
			i := py*fr.W + px
			if float32(invz) <= fr.Depth[i] {
				continue
			}
			light := (w0*la + w1*lb + w2*lc) / invz
			col := q.Color.Scale(light)
			if r.FogFar > r.FogNear {
				col = col.Mix(r.FogColor, (1/invz-r.FogNear)/(r.FogFar-r.FogNear))
			}
			switch {
			case q.Additive:
				fr.Color[i] = fr.Color[i].Add(col.Scale(q.Alpha))
			case q.Alpha < 1:
				fr.Color[i] = fr.Color[i].Mix(col, q.Alpha)
			default:
				fr.Color[i] = col
			}
			if write {
				fr.Depth[i] = float32(invz)
			}
		}
	}
}

// Draws a camera facing square of the size at a point, for particles and glows
func (r *Raster) Billboard(center Vec3, size float64, col RGB, alpha float64, additive bool) {
	h := size / 2
	rt, up := r.right.Mul(h), r.up.Mul(h)
	q := Quad{Color: col, Light: [4]float64{1, 1, 1, 1}, Alpha: alpha, Additive: additive, TwoSided: true}
	q.P = [4]Vec3{center.Sub(rt).Add(up), center.Add(rt).Add(up), center.Add(rt).Sub(up), center.Sub(rt).Sub(up)}
	r.Quad(q)
}

// Faces of an axis aligned box from its least corner and size, each lit flat by its side
func boxQuads(at, size Vec3, col RGB, light [6]float64, out []Quad) []Quad {
	x0, y0, z0 := at.X, at.Y, at.Z
	x1, y1, z1 := at.X+size.X, at.Y+size.Y, at.Z+size.Z
	face := func(a, b, c, d Vec3, l float64) {
		out = append(out, Quad{P: [4]Vec3{a, b, c, d}, Color: col, Light: [4]float64{l, l, l, l}, Alpha: 1})
	}
	face(Vec3{x0, y1, z0}, Vec3{x0, y1, z1}, Vec3{x1, y1, z1}, Vec3{x1, y1, z0}, light[0])
	face(Vec3{x0, y0, z1}, Vec3{x0, y0, z0}, Vec3{x1, y0, z0}, Vec3{x1, y0, z1}, light[1])
	face(Vec3{x0, y0, z0}, Vec3{x0, y1, z0}, Vec3{x1, y1, z0}, Vec3{x1, y0, z0}, light[2])
	face(Vec3{x1, y0, z1}, Vec3{x1, y1, z1}, Vec3{x0, y1, z1}, Vec3{x0, y0, z1}, light[3])
	face(Vec3{x0, y0, z1}, Vec3{x0, y1, z1}, Vec3{x0, y1, z0}, Vec3{x0, y0, z0}, light[4])
	face(Vec3{x1, y0, z0}, Vec3{x1, y1, z0}, Vec3{x1, y1, z1}, Vec3{x1, y0, z1}, light[5])
	return out
}

// Light of each box side under a sun, top, bottom, north, south, west, east
func sideLights(sun Vec3) [6]float64 {
	normals := [6]Vec3{{0, 1, 0}, {0, -1, 0}, {0, 0, -1}, {0, 0, 1}, {-1, 0, 0}, {1, 0, 0}}
	var out [6]float64
	for i, n := range normals {
		out[i] = 0.5 + 0.55*max(n.Dot(sun), 0) + 0.1*n.Y
	}
	return out
}

// Quadrant glyphs by lit pixel mask, top left one, top right two, bottom left four, bottom right eight
var quadrants = [16]string{" ", "▘", "▝", "▀", "▖", "▌", "▞", "▛", "▗", "▚", "▐", "▜", "▄", "▙", "▟", "█"}

// Writes the frame into cells, four pixels per cell, each cell the two colors and glyph nearest its pixels
func (fr *Frame) Encode(buf *uv.Buffer, x0, y0, cols, rows int, half bool, quantize func(RGB) color.Color) {
	if quantize == nil {
		quantize = func(c RGB) color.Color { return c.Color() }
	}
	var px [4]RGB
	for cy := range rows {
		for cx := range cols {
			if half {
				px[0] = fr.at(cx, 2*cy)
				px[1] = px[0]
				px[2] = fr.at(cx, 2*cy+1)
				px[3] = px[2]
			} else {
				px[0] = fr.at(2*cx, 2*cy)
				px[1] = fr.at(2*cx+1, 2*cy)
				px[2] = fr.at(2*cx, 2*cy+1)
				px[3] = fr.at(2*cx+1, 2*cy+1)
			}
			mask, fg, bg := partition(px)
			c := uv.Cell{Content: quadrants[mask], Width: 1, Style: uv.Style{Bg: quantize(bg)}}
			if mask != 0 {
				c.Style.Fg = quantize(fg)
			}
			buf.SetCell(x0+cx, y0+cy, &c)
		}
	}
}

// Pixel color, the last pixel of the frame past its edge
func (fr *Frame) at(x, y int) RGB {
	if fr.W == 0 || fr.H == 0 {
		return RGB{}
	}
	x, y = min(max(x, 0), fr.W-1), min(max(y, 0), fr.H-1)
	return fr.Color[y*fr.W+x]
}

// Error below which four pixels paint as one solid color rather than a pattern
const solidSlack = 0.0025

// Split of four pixels into lit and unlit groups with their mean colors that strays least from the pixels
func partition(px [4]RGB) (mask int, fg, bg RGB) {
	mean := px[0].Add(px[1]).Add(px[2]).Add(px[3]).Scale(0.25)
	best := 0.0
	for _, p := range px {
		best += p.Dist(mean)
	}
	bg = mean
	if best <= solidSlack {
		return 0, mean, mean
	}
	for m := 1; m < 15; m++ {
		var sumIn, sumOut RGB
		nIn := 0
		for i, p := range px {
			if m&(1<<i) != 0 {
				sumIn = sumIn.Add(p)
				nIn++
			} else {
				sumOut = sumOut.Add(p)
			}
		}
		cin, cout := sumIn.Scale(1/float64(nIn)), sumOut.Scale(1/float64(4-nIn))
		err := 0.0
		for i, p := range px {
			if m&(1<<i) != 0 {
				err += p.Dist(cin)
			} else {
				err += p.Dist(cout)
			}
		}
		if err < best {
			best, mask, fg, bg = err, m, cin, cout
		}
	}
	return mask, fg, bg
}

// Sixteen colors of a terminal with no palette of its own
var ansiPalette = [16]RGB{
	hexRGB(0x000000), hexRGB(0xaa0000), hexRGB(0x00aa00), hexRGB(0xaa5500), hexRGB(0x0000aa), hexRGB(0xaa00aa), hexRGB(0x00aaaa), hexRGB(0xaaaaaa),
	hexRGB(0x555555), hexRGB(0xff5555), hexRGB(0x55ff55), hexRGB(0xffff55), hexRGB(0x5555ff), hexRGB(0xff55ff), hexRGB(0x55ffff), hexRGB(0xffffff),
}

// Quantizer to the nearest of sixteen terminal colors, the given palette where it names them
func ansiQuantizer(palette [16]color.Color) func(RGB) color.Color {
	table := ansiPalette
	for i, c := range palette {
		if c != nil {
			table[i] = fromColor(c)
		}
	}
	return func(c RGB) color.Color {
		best, at := math.Inf(1), 0
		for i, p := range table {
			if d := c.Dist(p); d < best {
				best, at = d, i
			}
		}
		return ansi.IndexedColor(uint8(at))
	}
}
