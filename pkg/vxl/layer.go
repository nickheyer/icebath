package vxl

import "slices"

// Lit frame later flushes draw over, lit once and then only where something changed in front of it
type Layer struct {
	// Surface, depth, facing and lit color of every pixel before mirroring and haze
	fr Frame
	// Point lights the layer was lit by, every other light added over it per frame
	lights []Light
	// Crowding of every cell of the occlusion grid before its blur, and the depth it was read at
	aoW, aoH, aoShrink int
	aoZ, aoOcc         []float32
	// Inscattered light along the ray of every haze cell, cumulative at every step
	haze hazeField
	// Whether a flush has filled the layer
	ok bool
}

// Reports whether the layer holds a lit frame of the size
func (l *Layer) Fits(w, h int) bool { return l.ok && l.fr.W == w && l.fr.H == h }

// Writes the lit surfaces of the layer into a frame, which a flush over the layer then draws on
func (l *Layer) Restore(fr *Frame) {
	fr.CopyFrom(&l.fr)
}

// Keeps the frame as lit so far with the lights, the crowding grid and the haze of the flush
func (l *Layer) keep(r *Raster) {
	l.fr.CopyFrom(r.fr)
	l.lights = append(l.lights[:0], r.Lights...)
	lg := &r.aoGrid
	if len(r.ao) == r.fr.W*r.fr.H {
		l.aoW, l.aoH, l.aoShrink = lg.w, lg.h, lg.shrink
		l.aoZ = append(l.aoZ[:0], lg.z...)
		l.aoOcc = append(l.aoOcc[:0], r.aoRaw...)
	} else {
		l.aoW, l.aoH, l.aoShrink = 0, 0, 0
		l.aoZ, l.aoOcc = l.aoZ[:0], l.aoOcc[:0]
	}
	l.ok = true
}

// Keeps the haze the flush marched, once the march is done
func (l *Layer) keepHaze(r *Raster) { l.haze.copyFrom(&r.hz) }

// Reports whether a light of the flush was among the lights the layer was lit by
func (l *Layer) has(s *lightSpan) bool {
	if l == nil {
		return false
	}
	return slices.ContainsFunc(l.lights, func(k Light) bool { return k.Pos == s.pos && k.Color == s.col && k.Radius == s.radius })
}
