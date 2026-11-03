package sprint

import (
	"math"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Colors of the soulkiller world, a cyan sea of data under black air
var (
	skFloor   = vxl.Hex(0x070a10)
	skFort    = vxl.Hex(0x090d15)
	skSeam    = vxl.Hex(0x12303a)
	skRim     = vxl.Hex(0x0a1018)
	skCliff   = vxl.Hex(0x04070a)
	skDeep    = vxl.Hex(0x0a3844)
	skCyan    = vxl.Hex(0x34e4ff)
	skWhite   = vxl.Hex(0xd8fbff)
	skSky     = vxl.Hex(0x010204)
	skSkyGlow = vxl.Hex(0x0b2a33)
	skFog     = vxl.Hex(0x020a0e)
	skRed     = vxl.Hex(0xff2030)
)

// Cells the sea of data reaches past the platform
const seaReach = 34

// The soulkiller construct, a sea of cyan data blocks around the platform, every figure lit red
type soulkiller struct {
	t float64
}

func newSoulkiller() *soulkiller { return &soulkiller{} }

func (s *soulkiller) name() string { return "soulkiller" }

// Glossy black tiles flecked with cyan data, a cliff under the edges and the sea of data blocks thinning into the void
func (s *soulkiller) terrain(lay *dive, g vxl.Grid) {
	lay.floor(g, func(x, z int, fort bool) vxl.Voxel {
		c := skFloor
		if fort {
			c = skFort
		}
		if x%6 == 0 || z%6 == 0 {
			c = c.Mix(skSeam, 0.5)
		}
		if h := vxl.Hash2(x*5, z*11); h > 0.88 && !lay.blocked[[2]int{x, z}] && !lay.edge(x, z) {
			return glow(skCyan.Mix(skWhite, (h-0.88)*4), 0.5+(h-0.88)*6)
		}
		return glossy(c, vxl.TexPanel, 0.6)
	})
	lay.cliff(g, glossy(skRim, vxl.TexGrate, 0.4), vox(skCliff, vxl.TexNoise), vox(skCliff.Scale(0.6), vxl.TexNoise), 6)
	for x := -seaReach; x < lay.w+seaReach; x++ {
		for z := -seaReach; z < lay.d+seaReach; z++ {
			dx := max(-x, x-lay.w+1, 0)
			dz := max(-z, z-lay.d+1, 0)
			dd := math.Sqrt(float64(dx*dx + dz*dz))
			if dd < 2 {
				continue
			}
			density := 0.55 * math.Exp(-dd/16)
			h := vxl.Hash2(x*3+7, z*5+1)
			if h > density {
				continue
			}
			tall := 1 + int(vxl.Hash2(z*9, x*13)*3.2*(0.4+0.6*math.Exp(-dd/14)))
			bright := vxl.Hash2(x*17, z*19)
			for y := range tall {
				switch {
				case y == tall-1:
					g.Put(x, -3+y, z, glow(skCyan.Mix(skWhite, bright*bright*0.5), 0.18+bright*bright*0.9))
				default:
					g.Put(x, -3+y, z, glossy(skDeep.Mix(skCyan, 0.25*bright), vxl.TexNoise, 0.3))
				}
			}
		}
	}
}

func (s *soulkiller) materials() materials { return steelMaterials(skCyan) }

// Black above, the glow of the sea bleeding up below the horizon
func (s *soulkiller) sky(dir vxl.Vec3, t float64) vxl.RGB {
	below := max(-dir.Y, 0)
	return skSky.Add(skSkyGlow.Scale(math.Sqrt(below) * 0.7))
}

// No sun, the dark blue of the void, black fog and air that carries the cyan of the sea
func (s *soulkiller) light(r *vxl.Raster, sc *scene, dist float64) {
	r.Sun, r.SunColor = vxl.V(0, 1, 0), vxl.RGB{}
	r.Ambient = vxl.Hex(0x060c14)
	r.FogColor, r.FogSun, r.FogNear, r.FogFar = skFog, vxl.RGB{}, dist*1.5, dist*5
	r.AO = vxl.Occlusion{Radius: 1.2, Strength: 1.2}
	r.Reflect = vxl.Reflection{Strength: 0.9, Distance: 30}
	r.Haze = vxl.Haze{Density: 0.025, Reach: dist * 5, Color: skFog.Scale(0.4), Ambient: 0.35, Lights: 0.5}
}

func (s *soulkiller) emitters(lay *dive) []vxl.Light { return nil }

func (s *soulkiller) bake() bake {
	return bake{cluster: 3, radius: 10, smooth: 3, scale: 1.1, margin: 16, height: 16}
}

func (s *soulkiller) finish() finish {
	return finish{grade: vxl.Grade{Exposure: 1.05, Tonemap: true, Saturation: 1.25, Aberration: 2}, bloomThreshold: 0.5, bloomStrength: 1.1, outline: 0.4, vignette: 0.35}
}

func (s *soulkiller) decor(r *vxl.Raster, sc *scene) {}

// Motes of data rising off the sea now and then, drawn as glows
func (s *soulkiller) dress(r *vxl.Raster, sc *scene) {
	for i := range 40 {
		f := float64(i)
		ph := math.Mod(s.t*0.12+f*0.37, 1)
		x := -seaReach + math.Mod(f*17.3, float64(sc.lay.w+2*seaReach))
		z := -seaReach + math.Mod(f*29.7, float64(sc.lay.d+2*seaReach))
		if x > -2 && x < float64(sc.lay.w)+2 && z > -2 && z < float64(sc.lay.d)+2 {
			continue
		}
		r.Billboard(vxl.V(x, -2+ph*9, z), 0.5, skCyan, 0.5*(1-ph), true)
	}
}

// Red light over every figure, the mark of the soulkiller on the crew
func (s *soulkiller) lights(out []vxl.Light, sc *scene) []vxl.Light {
	for _, a := range sc.actors {
		if a.gone {
			continue
		}
		out = append(out, vxl.Light{Pos: a.headPos().Add(vxl.V(0, 1.6, 0)), Color: skRed.Scale(1.1), Radius: 5.5})
	}
	return out
}

func (s *soulkiller) update(dt float64, sc *scene) { s.t += dt }

func (s *soulkiller) drift() bool { return true }

func (s *soulkiller) bounds(lay *dive) (lo, hi vxl.Vec3) {
	return vxl.V(-seaReach, -7, -seaReach), vxl.V(float64(lay.w+seaReach), 18, float64(lay.d+seaReach))
}
