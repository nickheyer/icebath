//go:build ignore

package arena

import (
	"math"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Colors of the den world, a dark room walled with screens
var (
	denGrate    = vxl.Hex(0x171a20)
	denPanel    = vxl.Hex(0x1c2028)
	denSeam     = vxl.Hex(0x2a3240)
	denWall     = vxl.Hex(0x10141b)
	denBezel    = vxl.Hex(0x080a0e)
	denScreen   = vxl.Hex(0x3f9fff)
	denScreenHi = vxl.Hex(0x9fe0ff)
	denAmber    = vxl.Hex(0xffb347)
	denAlert    = vxl.Hex(0xff3b3b)
	denRack     = vxl.Hex(0x0c0f14)
	denLed      = vxl.Hex(0x7fff9e)
	denLamp     = vxl.Hex(0xdfe9f2)
	denSky      = vxl.Hex(0x04060a)
	denSkyUp    = vxl.Hex(0x0a1420)
	denFog      = vxl.Hex(0x05070b)
	denCliff    = vxl.Hex(0x05070a)
)

// Height of the walls of the den and the ceiling over it
const denHeight = 13

// One monitor on the north wall, its cell, its width and its color
type monitor struct {
	x, y, w int
	col     vxl.RGB
}

// A netrunner den, grated floor, walls of screens, racks of servers and lamps hanging from the cables
type den struct {
	screens []monitor
	lamps   []vxl.Vec3
	t       float64
	wallZ   int
}

func newDen() *den { return &den{} }

func (d *den) name() string { return "den" }

// Grated tiles, the north and west walls with their screens and racks, and lamps hanging over the floor
func (d *den) terrain(lay *dive, g vxl.Grid) {
	d.screens = d.screens[:0]
	d.lamps = d.lamps[:0]
	d.wallZ = -4
	lay.floor(g, func(x, z int, fort bool) vxl.Voxel {
		c, tex := denGrate, vxl.TexGrate
		if fort {
			c, tex = denPanel, vxl.TexPanel
		}
		if x%6 == 0 || z%6 == 0 {
			c = c.Mix(denSeam, 0.4)
		}
		return glossy(c, tex, 0.42)
	})
	lay.cliff(g, glossy(denGrate.Scale(0.7), vxl.TexGrate, 0.3), vox(denCliff, vxl.TexNoise), vox(denCliff, vxl.TexNoise), 3)
	x0, x1 := -5, lay.w+4
	z0, z1 := d.wallZ, lay.d+3
	g.Fill(x0, -2, z0, x1-x0+1, denHeight+2, 1, vox(denWall, vxl.TexPanel))
	g.Fill(x0, -2, z0, 1, denHeight+2, z1-z0+1, vox(denWall, vxl.TexPanel))
	g.Fill(x1, -2, z0, 1, denHeight+2, 10, vox(denWall, vxl.TexPanel))
	for x := x0 + 2; x < x1-4; x += 6 {
		for y := 1; y <= 9; y += 4 {
			h := vxl.Hash2(x, y)
			col := denScreen.Mix(denScreenHi, vxl.Hash2(y, x*3))
			switch {
			case h > 0.93:
				col = denAlert
			case h > 0.8:
				col = denAmber
			}
			off := h < 0.1
			g.Fill(x-1, y-1, z0+1, 6, 5, 1, vox(denBezel, vxl.TexPanel))
			if off {
				g.Fill(x, y, z0+1, 4, 3, 1, glossy(denBezel.Mix(denScreen, 0.1), vxl.TexPanel, 0.6))
				continue
			}
			g.Fill(x, y, z0+1, 4, 3, 1, vxl.Voxel{Col: col, Emit: 1.4, Tex: vxl.TexNoise, Gloss: 0.2})
			d.screens = append(d.screens, monitor{x: x, y: y, w: 4, col: col})
		}
	}
	for z := z0 + 3; z < z1-3; z += 3 {
		g.Fill(x0+1, 0, z, 1, 7, 2, vox(denRack, vxl.TexGrate))
		for y := range 7 {
			if h := vxl.Hash2(z*7, y); h > 0.55 {
				col := denLed
				if h > 0.85 {
					col = denAmber
				}
				g.Put(x0+2, y, z+int(h*2)%2, glow(col, 2))
			}
		}
	}
	for _, at := range [][2]float64{{lay.uplink.X, lay.uplink.Z - 3}, {lay.chart.X, lay.chart.Z - 2}, {float64(lay.w) * 0.7, float64(lay.fortD) * 0.5}} {
		x, z := int(at[0]), int(at[1])
		g.Fill(x, denHeight-3, z, 1, 3, 1, vox(denBezel, vxl.TexGrate))
		g.Put(x, denHeight-4, z, glow(denLamp, 1.2))
		d.lamps = append(d.lamps, vxl.V(float64(x)+0.5, denHeight-4.5, float64(z)+0.5))
	}
}

func (d *den) materials() materials {
	m := steelMaterials(denScreen)
	m.socket = glow(denAmber, 0.8)
	m.cradle = glow(denAmber, 0.35)
	return m
}

// Near black, a little bluer straight up
func (d *den) sky(dir vxl.Vec3, t float64) vxl.RGB {
	return denSky.Mix(denSkyUp, max(dir.Y, 0))
}

// No sun, a low blue ambient from the screens, black fog and still air glowing with the screens
func (d *den) light(r *vxl.Raster, sc *scene, dist float64) {
	r.Sun, r.SunColor = vxl.V(0, 1, 0), vxl.RGB{}
	r.Ambient = vxl.Hex(0x1a2436).Scale(0.8)
	r.FogColor, r.FogSun, r.FogNear, r.FogFar = denFog, vxl.RGB{}, dist*1.2, dist*4
	r.AO = vxl.Occlusion{Radius: 1.2, Strength: 1.4}
	r.Reflect = vxl.Reflection{Strength: 0.6, Distance: 24}
	r.Haze = vxl.Haze{Density: 0.025, Reach: dist * 4, Color: denFog.Scale(0.3), Ambient: 0.8, Lights: 0.5}
}

func (d *den) emitters(lay *dive) []vxl.Light { return nil }

func (d *den) bake() bake {
	return bake{cluster: 2, radius: 11, smooth: 2, scale: 1, margin: 8, height: 16}
}

func (d *den) finish() finish {
	return finish{grade: vxl.Grade{Exposure: 1.1, Tonemap: true, Saturation: 1.1, Aberration: 0.4}, bloomThreshold: 0.6, bloomStrength: 1, outline: 0.5, vignette: 0.3}
}

func (d *den) decor(r *vxl.Raster, sc *scene) {}

// Lines of data scrolling down the screens
func (d *den) dress(r *vxl.Raster, sc *scene) {
	z := float64(d.wallZ) + 2.003
	for i, s := range d.screens {
		for k := range 2 {
			ph := math.Mod(d.t*(0.6+0.3*float64(i%3))+float64(k)*0.5+float64(i)*0.173, 1)
			y := float64(s.y) + 3 - ph*3
			x0, x1 := float64(s.x)+0.2, float64(s.x+s.w)-0.2
			q := vxl.Quad{P: [4]vxl.Vec3{vxl.V(x0, y+0.12, z), vxl.V(x1, y+0.12, z), vxl.V(x1, y, z), vxl.V(x0, y, z)}, Color: s.col.Mix(colPaper, 0.5), Alpha: 0.6 * (1 - ph), Additive: true, TwoSided: true}
			l := vxl.Grey(1.4)
			q.Light = [4]vxl.RGB{l, l, l, l}
			r.Quad(q)
		}
	}
}

// Cold lamps hanging over the floor
func (d *den) lights(out []vxl.Light, sc *scene) []vxl.Light {
	for _, l := range d.lamps {
		out = append(out, vxl.Light{Pos: l, Color: denLamp.Scale(0.9), Radius: 14})
	}
	return out
}

func (d *den) update(dt float64, sc *scene) { d.t += dt }

func (d *den) drift() bool { return false }

func (d *den) bounds(lay *dive) (lo, hi vxl.Vec3) {
	return vxl.V(-6, -4, float64(d.wallZ)-1), vxl.V(float64(lay.w)+6, denHeight+2, float64(lay.d)+4)
}
