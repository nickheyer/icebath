package vxl

import "math"

// Procedural pattern a face is painted with, scaling its color by a factor near one
type Texture uint8

const (
	TexNone Texture = iota
	// Panels one unit square with beveled seams and a shade each
	TexPanel
	// Bars a quarter unit apart under a frame line every unit
	TexGrate
	// Traces and pads routed over a half unit grid
	TexCircuit
	// Blocks of one unit in uneven shades with sparks and a fine grain
	TexNoise
	// Pulses travelling along u, read as a phase in radians
	TexFlow
)

// Factor the pattern scales color by at a place, detail finer than the pixel footprint along each axis filtered out
func (t Texture) shade(u, v, du, dv float64) float64 {
	switch t {
	case TexPanel:
		return panel(u, v, du, dv)
	case TexGrate:
		return grate(u, v, du, dv)
	case TexCircuit:
		return circuit(u, v, du, dv)
	case TexNoise:
		return grain(u, v, du, dv)
	case TexFlow:
		return pulses(u, du)
	}
	return 1
}

// Integral from zero to x of a unit period train lit over the first width of each period
func trainInt(x, w float64) float64 {
	f := math.Floor(x)
	return f*w + min(x-f, w)
}

// Box filtered share of a footprint around x that unit period pulses lit over a width cover
func train(x, w, fw float64) float64 {
	if fw < 1e-6 {
		if x-math.Floor(x) < w {
			return 1
		}
		return 0
	}
	return (trainInt(x+fw*0.5, w) - trainInt(x-fw*0.5, w)) / fw
}

// Weight left on detail of a frequency, full below a quarter period per footprint and gone past three quarters
func keep(fw, freq float64) float64 { return 1 - Smoothstep((fw*freq-0.25)*2) }

// Hash of the unit cell holding a place, salted
func cellHash(u, v float64, salt int) float64 {
	return Hash2(int(math.Floor(u))+salt*7919, int(math.Floor(v))-salt*104729)
}

// Unit panels with dark grooves at their seams, a lit bevel after each groove and a shaded one before it
func panel(u, v, du, dv float64) float64 {
	const groove, bevel = 0.07, 0.1
	gu, gv := train(u+groove/2, groove, du), train(v+groove/2, groove, dv)
	g := gu + gv - gu*gv
	lit := train(u-groove/2, bevel, du) + train(v-groove/2, bevel, dv)
	dark := train(u-(1-groove/2-bevel), bevel, du) + train(v-(1-groove/2-bevel), bevel, dv)
	f := (1 + 0.1*lit - 0.12*dark) * (1 - 0.42*g)
	return f * (1 + 0.14*(cellHash(u, v, 0)-0.5)*keep(max(du, dv), 1))
}

// Dark bars four to a unit along u crossed by a frame line once a unit along v
func grate(u, v, du, dv float64) float64 {
	bars := train(u*4, 0.35, du*4)
	frame := train(v+0.05, 0.1, dv)
	return (1 - 0.45*bars) * (1 - 0.3*frame)
}

// Traces across, down, crossing or ending in a pad per half unit cell, picked by a hash of the cell
func circuit(u, v, du, dv float64) float64 {
	const mean = 0.995
	tile := 1 + 0.1*(cellHash(u, v, 1)-0.5)*keep(max(du, dv), 1)
	k := keep(max(du, dv), 2)
	if k <= 0 {
		return tile * mean
	}
	su, sv, sdu, sdv := u*2, v*2, du*2, dv*2
	h := Hash2(int(math.Floor(su)), int(math.Floor(sv)))
	trace := 0.0
	switch {
	case h < 0.3:
		trace = train(sv-0.4, 0.2, sdv)
	case h < 0.6:
		trace = train(su-0.4, 0.2, sdu)
	case h < 0.7:
		trace = max(train(sv-0.4, 0.2, sdv), train(su-0.4, 0.2, sdu))
	case h > 0.9:
		trace = train(su-0.3, 0.4, sdu) * train(sv-0.3, 0.4, sdv)
	}
	return tile * (mean + (0.9+0.55*trace-mean)*k)
}

// Smooth value noise of the hashes at the corners of the unit cell around a place
func valueNoise(u, v float64) float64 {
	fu, fv := math.Floor(u), math.Floor(v)
	i, j := int(fu), int(fv)
	tu, tv := Smoothstep(u-fu), Smoothstep(v-fv)
	a := Hash2(i, j) + (Hash2(i+1, j)-Hash2(i, j))*tu
	b := Hash2(i, j+1) + (Hash2(i+1, j+1)-Hash2(i, j+1))*tu
	return a + (b-a)*tv
}

// Unit blocks in shades from dim to full with rare bright sparks over a fine five per unit speckle
func grain(u, v, du, dv float64) float64 {
	const blockMean = 0.9036
	fw := max(du, dv)
	n := cellHash(u, v, 2)
	block := 0.6 + 0.5*n
	if n > 0.92 {
		block = 1.75
	}
	f := blockMean + (block-blockMean)*keep(fw, 1)
	return f * (1 + 0.16*(valueNoise(u*5, v*5)-0.5)*keep(fw, 5))
}

// Bright sine crests along u over a dim floor, fading to their mean once a pixel spans a wave
func pulses(u, du float64) float64 {
	const mean = 0.4 + 0.6/math.Pi
	return mean + (0.4+0.6*max(0, math.Sin(u))-mean)*keep(du, 1/(2*math.Pi))
}
