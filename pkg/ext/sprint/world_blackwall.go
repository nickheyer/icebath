package sprint

import (
	"math"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Colors of the blackwall world, red strands over a black glossy floor of red motes
var (
	bwFloor    = vxl.Hex(0x090c12)
	bwFort     = vxl.Hex(0x0c1017)
	bwSeam     = vxl.Hex(0x2a1420)
	bwRim      = vxl.Hex(0x0b0e14)
	bwCliff    = vxl.Hex(0x050608)
	bwDot      = vxl.Hex(0xff2a4a)
	bwDotPink  = vxl.Hex(0xff4fa0)
	bwStrand   = vxl.Hex(0xff1f45)
	bwStrandHi = vxl.Hex(0xff5fb8)
	bwSky      = vxl.Hex(0x020306)
	bwSkyGlow  = vxl.Hex(0x4a0a1c)
	bwFog      = vxl.Hex(0x0a0308)
)

// One glowing mote sunk into the floor
type floorDot struct {
	at  vxl.Vec3
	col vxl.RGB
	lvl float64
}

// Red strand of the wall, its cell, its height and its color
type strand struct {
	x, z, h int
	col     vxl.RGB
	emit    float64
}

// Cyberspace before the blackwall, a black glossy floor of red motes under a wall of red strands
type blackwall struct {
	dots    []floorDot
	strands []strand
	scans   [7]scanLine
	rnd     vxl.RNG
	t       float64
	w       int
}

func newBlackwall() *blackwall {
	b := &blackwall{rnd: vxl.RNG{Seed: 0x5eed}}
	for i := range b.scans {
		b.scans[i] = scanLine{y: b.rnd.Next() * 16, speed: 0.6 + b.rnd.Next()*1.6}
	}
	return b
}

func (b *blackwall) name() string { return "blackwall" }

// Picks a new span across the wall for a scan line
func (b *blackwall) rescan(i int) {
	w := float64(b.w + 20)
	x0 := -10 + b.rnd.Next()*w*0.7
	b.scans[i].x0, b.scans[i].x1 = x0, x0+3+b.rnd.Next()*w*0.3
}

// Glossy black tiles with a red mote in every other cell, a cliff under the edges and the strand wall
func (b *blackwall) terrain(lay *dive, g vxl.Grid) {
	b.w = lay.w
	b.dots = b.dots[:0]
	b.strands = b.strands[:0]
	lay.floor(g, func(x, z int, fort bool) vxl.Voxel {
		c := bwFloor
		if fort {
			c = bwFort
		}
		if x%6 == 0 || z%6 == 0 {
			c = c.Mix(bwSeam, 0.5)
		}
		return glossy(c, vxl.TexPanel, 0.78)
	})
	lay.cliff(g, glossy(bwRim, vxl.TexGrate, 0.4), vox(bwCliff, vxl.TexNoise), vox(bwCliff.Scale(0.6), vxl.TexNoise), 5)
	for cell := range lay.tiles {
		x, z := cell[0], cell[1]
		if x%2 == 0 || z%2 == 0 || lay.blocked[cell] || lay.edge(x, z) {
			continue
		}
		h := vxl.Hash2(x*13, z*7)
		col := bwDot.Mix(bwDotPink, vxl.Hash2(z, x*3))
		b.dots = append(b.dots, floorDot{at: vxl.V(float64(x)+0.5, 0, float64(z)+0.5), col: col, lvl: 1.4 + 1.2*h})
	}
	for x := -10; x < lay.w+10; x++ {
		for z := lay.wallZ - 1; z <= lay.wallZ; z++ {
			if (x+z)%2 != 0 {
				continue
			}
			h := vxl.Hash2(x, z*31)
			s := strand{x: x, z: z, h: 9 + int(h*9), col: bwStrand.Mix(bwStrandHi, vxl.Hash2(x*7, z)*0.6), emit: 0.55 + h*0.7}
			if vxl.Hash2(x*3, z*5) > 0.92 {
				s.h += 6
			}
			b.strands = append(b.strands, s)
			for y := -6; y < s.h; y++ {
				level := s.emit * (0.55 + 0.45*vxl.Hash3(x, y, z))
				g.Put(x, y, z, glow(s.col, level))
			}
		}
	}
}

func (b *blackwall) materials() materials { return steelMaterials(bwDot) }

// Black above, a red haze low toward the wall
func (b *blackwall) sky(dir vxl.Vec3, t float64) vxl.RGB {
	north := max(-dir.Z, 0)
	low := max(1-math.Abs(dir.Y)*5, 0)
	return bwSky.Add(bwSkyGlow.Scale(north * north * low * 0.9))
}

// No sun, a faint cold ambient, black fog and air that carries the red of the wall
func (b *blackwall) light(r *vxl.Raster, sc *scene, dist float64) {
	r.Sun, r.SunColor = vxl.V(0, 1, 0), vxl.RGB{}
	r.Ambient = vxl.Hex(0x0c0f18).Scale(0.9)
	r.FogColor, r.FogSun, r.FogNear, r.FogFar = bwFog, vxl.RGB{}, dist*1.8, dist*6
	r.AO = vxl.Occlusion{Radius: 1.3, Strength: 1.3}
	r.Reflect = vxl.Reflection{Strength: 1, Distance: 36}
	r.Haze = vxl.Haze{Density: 0.028, Reach: dist * 5, Color: bwFog.Scale(0.15), Ambient: 0.55, Lights: 0.45}
}

// The motes of the floor as emitters, grouped four by four
func (b *blackwall) emitters(lay *dive) []vxl.Light {
	type sum struct {
		at  vxl.Vec3
		col vxl.RGB
		n   float64
	}
	groups := map[[2]int]*sum{}
	var order [][2]int
	for _, d := range b.dots {
		k := [2]int{int(d.at.X) / 4, int(d.at.Z) / 4}
		s, ok := groups[k]
		if !ok {
			s = &sum{}
			groups[k] = s
			order = append(order, k)
		}
		s.at = s.at.Add(d.at)
		s.col = s.col.Add(d.col.Scale(d.lvl * 0.22))
		s.n++
	}
	out := make([]vxl.Light, 0, len(order))
	for _, k := range order {
		s := groups[k]
		out = append(out, vxl.Light{Pos: s.at.Mul(1 / s.n).Add(vxl.V(0, 0.3, 0)), Color: s.col, Radius: 9})
	}
	return out
}

func (b *blackwall) bake() bake {
	return bake{cluster: 4, radius: 12, smooth: 2, scale: 0.9, margin: 10, height: 18}
}

func (b *blackwall) finish() finish {
	return finish{grade: vxl.Grade{Exposure: 1.05, Tonemap: true, Saturation: 1.2, Aberration: 0.7}, bloomThreshold: 0.5, bloomStrength: 1.2, outline: 0.5, vignette: 0.3}
}

// Every mote of the floor as a small glowing face
func (b *blackwall) decor(r *vxl.Raster, sc *scene) {
	for _, d := range b.dots {
		q := vxl.FlatQuad(vxl.FloorQuad(d.at, 0.17, 0.17, 0.012), d.col, d.lvl)
		r.Quad(q)
	}
}

// Scan lines crawling over the strands and the surge glow of the wall
func (b *blackwall) dress(r *vxl.Raster, sc *scene) {
	z := float64(sc.lay.wallZ) + 1.003
	k := 0.5 + 0.5*sc.wallSurge
	for _, s := range b.scans {
		q := vxl.Quad{P: [4]vxl.Vec3{vxl.V(s.x0, s.y+0.4, z), vxl.V(s.x1, s.y+0.4, z), vxl.V(s.x1, s.y, z), vxl.V(s.x0, s.y, z)}, Color: bwStrandHi, Alpha: 0.5 + 0.5*sc.wallSurge, Additive: true, TwoSided: true}
		l := vxl.Grey(1.6 * k)
		q.Light = [4]vxl.RGB{l, l, l, l}
		r.Quad(q)
	}
	if sc.wallSurge > 0 {
		r.Billboard(vxl.V(float64(sc.lay.w)/2, 6, z+1), float64(sc.lay.w)*1.2, colRed, 0.1*sc.wallSurge, true)
		r.Billboard(vxl.V(float64(sc.lay.w)/2, 2, z-2), float64(sc.lay.w)*1.4, bwStrand, 0.12*sc.wallSurge, true)
	}
}

// Broad red lights along the wall washing the north of the fort, steady so the lit floor keeps them, the surge carried by the glow over the wall
func (b *blackwall) lights(out []vxl.Light, sc *scene) []vxl.Light {
	w := float64(sc.lay.w)
	z := float64(sc.lay.wallZ) + 2.5
	for i := range 3 {
		x := w * (float64(i) + 0.5) / 3
		out = append(out, vxl.Light{Pos: vxl.V(x, 5, z), Color: bwStrand.Scale(0.275), Radius: 15})
	}
	return out
}

// Moves the scan lines, faster while the wall surges
func (b *blackwall) update(dt float64, sc *scene) {
	b.t += dt
	for i := range b.scans {
		s := &b.scans[i]
		s.y += dt * s.speed * (1 + 3*sc.wallSurge)
		if s.y > 18 {
			s.y = -5
			b.rescan(i)
		}
	}
}

func (b *blackwall) drift() bool { return true }

func (b *blackwall) bounds(lay *dive) (lo, hi vxl.Vec3) {
	return vxl.V(-12, -7, float64(lay.wallZ)-2), vxl.V(float64(lay.w)+12, 26, float64(lay.d)+3)
}
