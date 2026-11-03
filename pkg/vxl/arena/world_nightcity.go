//go:build ignore

package arena

import (
	"math"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Colors of the night city world, a rooftop at dawn over a hazy skyline
var (
	ncConcrete = vxl.Hex(0x6a655e)
	ncAsphalt  = vxl.Hex(0x3b3a3d)
	ncSeam     = vxl.Hex(0x8a847a)
	ncPaint    = vxl.Hex(0xb9962a)
	ncParapet  = vxl.Hex(0x4a4541)
	ncFacade   = vxl.Hex(0x23242a)
	ncTower    = vxl.Hex(0x1a1b22)
	ncTowerB   = vxl.Hex(0x2a2530)
	ncWarm     = vxl.Hex(0xffc27a)
	ncCool     = vxl.Hex(0x78d9ff)
	ncBeacon   = vxl.Hex(0xff3030)
	ncMagenta  = vxl.Hex(0xff3fb5)
	ncZenith   = vxl.Hex(0x14203c)
	ncHorizon  = vxl.Hex(0xd87a3c)
	ncBand     = vxl.Hex(0xf4b070)
	ncGround   = vxl.Hex(0x2e2220)
	ncSunDisc  = vxl.Hex(0xfff0c0)
	ncFog      = vxl.Hex(0xb07650)
	ncFogSun   = vxl.Hex(0xffb070)
	ncSunLight = vxl.Hex(0xffd3a0)
	ncAmbient  = vxl.Hex(0x4a5878)
)

// Floors under the rooftop the building body is drawn down to
const facadeDepth = 14

// Tower of the skyline, its footprint, height and whether a beacon tops it
type tower struct {
	x, z, w, d, h int
	beacon        bool
	billboard     int
}

// A rooftop over night city at dawn, towers in silhouette through the haze and the sun low on the horizon
type nightCity struct {
	towers  []tower
	beacons []vxl.Vec3
	sun     vxl.Vec3
	t       float64
}

func newNightCity() *nightCity {
	return &nightCity{sun: vxl.V(-0.45, 0.14, -0.85).Norm()}
}

func (n *nightCity) name() string { return "nightcity" }

// Concrete tiles with a painted ring, a parapet, the facade of the building below and a ring of towers around it
func (n *nightCity) terrain(lay *dive, g vxl.Grid) {
	n.towers = n.towers[:0]
	n.beacons = n.beacons[:0]
	ux, uz := lay.uplink.X, lay.uplink.Z
	lay.floor(g, func(x, z int, fort bool) vxl.Voxel {
		c, tex := ncConcrete, vxl.TexPanel
		if fort {
			c, tex = ncAsphalt, vxl.TexNoise
		}
		if x%6 == 0 || z%6 == 0 {
			c = c.Mix(ncSeam, 0.3)
		}
		if ring := math.Hypot(float64(x)+0.5-ux, float64(z)+0.5-uz); ring > 4 && ring < 5.2 {
			c = ncPaint
		}
		return glossy(c, tex, 0.3)
	})
	lay.cliff(g, glossy(ncConcrete.Scale(0.8), vxl.TexPanel, 0.2), vox(ncFacade, vxl.TexPanel), vox(ncFacade, vxl.TexPanel), 2)
	for cell := range lay.tiles {
		x, z := cell[0], cell[1]
		if !lay.edge(x, z) {
			continue
		}
		if (x+z)%7 != 3 {
			g.Put(x, 0, z, vox(ncParapet, vxl.TexPanel))
		}
		if lay.ring(x, z) == 0 && (x == 0 || x == lay.w-1) && (z == 0 || z == lay.d-1) {
			g.Fill(x, 0, z, 1, 3, 1, vox(ncParapet, vxl.TexPanel))
		}
		for y := 2; y <= facadeDepth; y++ {
			v := vox(ncFacade, vxl.TexPanel)
			if h := vxl.Hash3(x, y, z); h > 0.7 && y%3 != 0 {
				col := ncWarm
				if h > 0.92 {
					col = ncCool
				}
				v = glow(col, 0.3+h*0.3)
			}
			g.Put(x, -y, z, v)
		}
	}
	cx, cz := float64(lay.w)/2, float64(lay.d)/2
	rnd := vxl.RNG{Seed: 0xc17}
	for i := range 40 {
		ang := float64(i)/40*2*math.Pi + rnd.Spread()*0.08
		rad := 46 + rnd.Next()*44
		tw := tower{w: 3 + rnd.Intn(5), d: 3 + rnd.Intn(5)}
		tw.x, tw.z = int(cx+math.Cos(ang)*rad)-tw.w/2, int(cz+math.Sin(ang)*rad)-tw.d/2
		if tw.x+tw.w > -5 && tw.x < lay.w+5 && tw.z+tw.d > lay.wallZ-5 && tw.z < lay.d+5 {
			continue
		}
		tw.h = 3 + rnd.Intn(9) + int(rad/90*12)
		if rnd.Next() < 0.2 {
			tw.h += 10 + rnd.Intn(8)
		}
		tw.beacon = rnd.Next() < 0.35
		if rnd.Next() < 0.2 {
			tw.billboard = 1 + rnd.Intn(3)
		}
		n.towers = append(n.towers, tw)
		n.raise(g, tw, cx, cz)
	}
}

// Raises one tower as dark voxels with lit windows, a beacon mast and a billboard facing the platform
func (n *nightCity) raise(g vxl.Grid, tw tower, cx, cz float64) {
	body := ncTower.Mix(ncTowerB, vxl.Hash2(tw.x, tw.z))
	for x := tw.x; x < tw.x+tw.w; x++ {
		for z := tw.z; z < tw.z+tw.d; z++ {
			edge := x == tw.x || x == tw.x+tw.w-1 || z == tw.z || z == tw.z+tw.d-1
			for y := -facadeDepth; y < tw.h; y++ {
				v := vox(body, vxl.TexPanel)
				if edge && y%3 != 0 {
					if h := vxl.Hash3(x*3, y, z*5); h > 0.78 {
						col := ncWarm
						if h > 0.9 {
							col = ncCool
						}
						v = glow(col.Scale(0.8+0.4*h), 0.35+h*0.35)
					}
				}
				g.Put(x, y, z, v)
			}
		}
	}
	if tw.beacon {
		mx, mz := tw.x+tw.w/2, tw.z+tw.d/2
		mast := 3 + int(vxl.Hash2(mx, mz)*4)
		g.Fill(mx, tw.h, mz, 1, mast, 1, vox(ncTower.Scale(0.8), vxl.TexGrate))
		g.Put(mx, tw.h+mast, mz, glow(ncBeacon, 1.8))
		n.beacons = append(n.beacons, vxl.V(float64(mx)+0.5, float64(tw.h+mast)+0.5, float64(mz)+0.5))
	}
	if tw.billboard == 0 {
		return
	}
	col := [3]vxl.RGB{ncMagenta, ncCool, ncWarm}[tw.billboard-1]
	y0 := tw.h/2 + 2
	toward := vxl.V(cx-float64(tw.x), 0, cz-float64(tw.z))
	switch {
	case math.Abs(toward.X) > math.Abs(toward.Z) && toward.X > 0:
		g.Fill(tw.x+tw.w, y0, tw.z, 1, 3, tw.d, vxl.Voxel{Col: col, Emit: 1.3, Tex: vxl.TexNoise})
	case math.Abs(toward.X) > math.Abs(toward.Z):
		g.Fill(tw.x-1, y0, tw.z, 1, 3, tw.d, vxl.Voxel{Col: col, Emit: 1.3, Tex: vxl.TexNoise})
	case toward.Z > 0:
		g.Fill(tw.x, y0, tw.z+tw.d, tw.w, 3, 1, vxl.Voxel{Col: col, Emit: 1.3, Tex: vxl.TexNoise})
	default:
		g.Fill(tw.x, y0, tw.z-1, tw.w, 3, 1, vxl.Voxel{Col: col, Emit: 1.3, Tex: vxl.TexNoise})
	}
}

func (n *nightCity) materials() materials {
	return materials{
		conduit:    glossy(vxl.Hex(0x2e3a44), vxl.TexGrate, 0.3),
		pedestal:   glossy(vxl.Hex(0x5a5752), vxl.TexGrate, 0.3),
		pedestalHi: glossy(vxl.Hex(0x6e6a63), vxl.TexGrate, 0.3),
		socket:     glow(ncWarm, 0.8),
		pad:        glossy(vxl.Hex(0x6c6760), vxl.TexPanel, 0.35),
		padHi:      glow(vxl.Hex(0xffe0a0), 1),
		pillar:     vox(vxl.Hex(0x3a3a3f), vxl.TexGrate),
		table:      vox(vxl.Hex(0x403d3a), vxl.TexGrate),
		tableTop:   glossy(vxl.Hex(0x5a5651), vxl.TexPanel, 0.4),
		tower:      glossy(vxl.Hex(0x46423e), vxl.TexPanel, 0.3),
		towerHi:    glow(ncWarm, 0.8),
		cradle:     glow(vxl.Hex(0xffb86a), 0.6),
	}
}

// Dawn gradient with the sun low on the horizon, a warm band along it and dark haze below
func (n *nightCity) sky(dir vxl.Vec3, t float64) vxl.RGB {
	if dir.Y < 0 {
		return ncGround.Mix(ncHorizon, max(1+dir.Y*6, 0))
	}
	c := gradientSky(dir, ncZenith, ncHorizon, 1)
	band := 1 - dir.Y
	for range 15 {
		band *= 1 - dir.Y
	}
	c = c.Add(ncBand.Scale(band * 0.25))
	return c.Add(sunDisc(dir, n.sun, ncSunDisc, 0.004, 0.6))
}

// A low warm sun with long shadows, blue ambient, warm haze thick with the sun and wet concrete mirroring the sky
func (n *nightCity) light(r *vxl.Raster, sc *scene, dist float64) {
	r.Sun, r.SunColor = n.sun, ncSunLight.Scale(0.9)
	r.Ambient = ncAmbient.Scale(0.5)
	r.FogColor, r.FogSun, r.FogNear, r.FogFar = ncFog.Scale(0.45), ncFogSun.Scale(0.3), dist*1.5, dist*5.5
	r.AO = vxl.Occlusion{Radius: 1.2, Strength: 1}
	r.Reflect = vxl.Reflection{Strength: 0.5, Distance: 30, Sky: ncHorizon.Scale(0.5)}
	r.Haze = vxl.Haze{Density: 0.012, Reach: dist * 4, Color: ncFog.Scale(0.02), Ambient: 0.25, Lights: 0.3, Sun: 0.3, Forward: 0.7}
}

func (n *nightCity) emitters(lay *dive) []vxl.Light { return nil }

func (n *nightCity) bake() bake {
	return bake{cluster: 2, radius: 10, bounce: 0.4, bounceCluster: 4, smooth: 2, scale: 1, margin: 10, height: 20}
}

func (n *nightCity) finish() finish {
	return finish{grade: vxl.Grade{Exposure: 0.95, Tonemap: true, Saturation: 1.1, Aberration: 0.35}, bloomThreshold: 0.75, bloomStrength: 0.8, outline: 0.45, vignette: 0.25}
}

func (n *nightCity) decor(r *vxl.Raster, sc *scene) {}

// Beacons blinking on the masts of the towers
func (n *nightCity) dress(r *vxl.Raster, sc *scene) {
	for i, b := range n.beacons {
		k := 0.5 + 0.5*math.Sin(n.t*2.4+float64(i)*1.7)
		if k > 0.6 {
			r.Billboard(b, 1.2+k, ncBeacon, 0.5*k, true)
		}
	}
}

func (n *nightCity) lights(out []vxl.Light, sc *scene) []vxl.Light { return out }

func (n *nightCity) update(dt float64, sc *scene) { n.t += dt }

func (n *nightCity) drift() bool { return false }

func (n *nightCity) bounds(lay *dive) (lo, hi vxl.Vec3) {
	return vxl.V(-80, -facadeDepth-1, -80), vxl.V(float64(lay.w)+80, 60, float64(lay.d)+80)
}
