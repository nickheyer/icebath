package sprint

import (
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Look of the arena around the platform, its ground, its surroundings, its sky, its light and its finish
type world interface {
	name() string
	// Static voxels of the ground under the platform and everything around it
	terrain(lay *dive, g vxl.Grid)
	// Voxels the stations are built from
	materials() materials
	// Sky along a unit direction out of the camera at a time
	sky(dir vxl.Vec3, t float64) vxl.RGB
	// Sun, ambient, fog, air, crowding and mirroring of the raster for a camera at a distance
	light(r *vxl.Raster, sc *scene, dist float64)
	// Emitters beyond the glowing voxels for the radiance bake
	emitters(lay *dive) []vxl.Light
	// How the radiance is baked
	bake() bake
	// Finish of every frame
	finish() finish
	// Faces drawn with the static layer, kept while the camera rests
	decor(r *vxl.Raster, sc *scene)
	// Faces drawn every frame
	dress(r *vxl.Raster, sc *scene)
	// Point lights of the world this frame
	lights(out []vxl.Light, sc *scene) []vxl.Light
	// Advances whatever moves in the world by dt seconds
	update(dt float64, sc *scene)
	// Box around the platform and the surroundings the shadows and the radiance cover
	bounds(lay *dive) (lo, hi vxl.Vec3)
	// Reports whether shards of the broken net drift around the platform
	drift() bool
}

// Finish of a frame, its glow, its outline, its edges and its grade
type finish struct {
	grade          vxl.Grade
	bloomThreshold float64
	bloomStrength  float64
	outline        float64
	vignette       float64
}

// How the radiance of a world is baked, the grouping and reach of its emitters, the sun bounce and the smoothing
type bake struct {
	cluster int
	radius  float64
	// Share of the sunlight the ground bounces, none at zero, and the grouping of the bounce
	bounce        float64
	bounceCluster int
	smooth        int
	// Factor the baked light is scaled by
	scale float64
	// World units the volume reaches past the platform sideways and up
	margin, height float64
}

// World by the name of the setting, the blackwall for an unknown one
func worldFor(name string) world {
	switch name {
	case "soulkiller":
		return newSoulkiller()
	case "nightcity":
		return newNightCity()
	case "den":
		return newDen()
	}
	return newBlackwall()
}

// Rendering budget of a frame, which passes run and how finely
type quality struct {
	// Frame pixels per cell pixel across and down
	super int
	// Whether the crowding, the mirroring and the air are drawn
	occlude, mirror, haze bool
	// Samples and grid shrink of the crowding, steps and shrink of the mirroring, steps and shrink of the air
	aoSamples, aoShrink   int
	reflSteps, reflShrink int
	hazeSteps, hazeShrink int
}

// Quality by the name of the setting, high for an unknown one
func qualityOf(name string) quality {
	switch name {
	case "low":
		return quality{super: 1}
	case "medium":
		return quality{super: 1, occlude: true, haze: true, aoSamples: 8, aoShrink: 4, hazeSteps: 12, hazeShrink: 8}
	case "ultra":
		return quality{super: 2, occlude: true, mirror: true, haze: true, aoSamples: 16, aoShrink: 4, reflSteps: 32, reflShrink: 4, hazeSteps: 28, hazeShrink: 8}
	}
	return quality{super: 1, occlude: true, mirror: true, haze: true, aoSamples: 12, aoShrink: 2, reflSteps: 24, reflShrink: 2, hazeSteps: 20, hazeShrink: 4}
}

// Fits the passes a world set on the raster to the budget, switching off the ones it lacks
func (q quality) apply(r *vxl.Raster) {
	if q.occlude {
		r.AO.Samples, r.AO.Shrink = q.aoSamples, q.aoShrink
	} else {
		r.AO.Strength = 0
	}
	if q.mirror {
		r.Reflect.Steps, r.Reflect.Shrink = q.reflSteps, q.reflShrink
	} else {
		r.Reflect.Strength = 0
	}
	if q.haze {
		r.Haze.Steps, r.Haze.Shrink = q.hazeSteps, q.hazeShrink
	} else {
		r.Haze.Density = 0
	}
}

// Voxel of a color and pattern
func vox(col vxl.RGB, tex vxl.Texture) vxl.Voxel { return vxl.Voxel{Col: col, Tex: tex} }

// Voxel of a color and pattern mirroring at a gloss
func glossy(col vxl.RGB, tex vxl.Texture, gloss float64) vxl.Voxel {
	return vxl.Voxel{Col: col, Tex: tex, Gloss: gloss}
}

// Voxel glowing with a color at a strength
func glow(col vxl.RGB, emit float64) vxl.Voxel { return vxl.Voxel{Col: col, Emit: emit} }

// Stations of a dark steel look, shared by the worlds of the net
func steelMaterials(seam vxl.RGB) materials {
	return materials{
		conduit:    glossy(colConduit, vxl.TexGrate, 0.3),
		pedestal:   glossy(colPedestal, vxl.TexGrate, 0.3),
		pedestalHi: glossy(colPedestalH.Mix(seam, 0.2), vxl.TexGrate, 0.3),
		socket:     glow(seam, 0.8),
		pad:        glossy(colPad, vxl.TexPanel, 0.5),
		padHi:      glow(colPadHi.Mix(seam, 0.5), 1.1),
		pillar:     vox(colPillar, vxl.TexGrate),
		table:      vox(colTable, vxl.TexGrate),
		tableTop:   glossy(colTableTop, vxl.TexPanel, 0.5),
		tower:      glossy(colTower, vxl.TexPanel, 0.35),
		towerHi:    glow(colTowerHi.Mix(seam, 0.6), 0.9),
		cradle:     glow(colCradle.Mix(seam, 0.4), 0.7),
	}
}

// Sky fading from a zenith color to a horizon color by the height of a direction, the horizon widened by a power
func gradientSky(dir vxl.Vec3, zenith, horizon vxl.RGB, power float64) vxl.RGB {
	up := min(max(dir.Y, 0), 1)
	t := 1 - up
	k := t
	for range int(power) {
		k *= t
	}
	return zenith.Mix(horizon, k)
}

// Share of the direction within a cone around another, one along it, falling to nothing at the cone edge
func cone(dir, axis vxl.Vec3, width float64) float64 {
	d := dir.Dot(axis)
	if d <= 1-width {
		return 0
	}
	return vxl.Smoothstep((d - (1 - width)) / width)
}

// Narrow sun disc with a soft glow around it along a direction
func sunDisc(dir, sun vxl.Vec3, col vxl.RGB, disc, halo float64) vxl.RGB {
	d := max(dir.Dot(sun), 0)
	k := d
	for range 7 {
		k *= k
	}
	return col.Scale(cone(dir, sun, disc)*2 + k*halo)
}
