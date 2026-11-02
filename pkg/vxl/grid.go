package vxl

import "math"

// Static voxels by integer cell, each a color
type Grid map[[3]int]RGB

// Sets a voxel at a cell
func (g Grid) Set(x, y, z int, col RGB) { g[[3]int{x, y, z}] = col }

// Removes the voxel at a cell
func (g Grid) Del(x, y, z int) { delete(g, [3]int{x, y, z}) }

// Fills a box of voxels from a least corner and a size in cells
func (g Grid) Box(x, y, z, w, h, d int, col RGB) {
	for i := range w {
		for j := range h {
			for k := range d {
				g.Set(x+i, y+j, z+k, col)
			}
		}
	}
}

// Reports whether a cell holds a voxel
func (g Grid) Solid(x, y, z int) bool {
	_, ok := g[[3]int{x, y, z}]
	return ok
}

// Least and greatest corner of the voxels, false for an empty grid
func (g Grid) Bounds() (lo, hi Vec3, ok bool) {
	first := true
	for c := range g {
		p := Vec3{float64(c[0]), float64(c[1]), float64(c[2])}
		if first {
			lo, hi, first = p, p.Add(Vec3{1, 1, 1}), false
			continue
		}
		lo = Vec3{min(lo.X, p.X), min(lo.Y, p.Y), min(lo.Z, p.Z)}
		hi = Vec3{max(hi.X, p.X+1), max(hi.Y, p.Y+1), max(hi.Z, p.Z+1)}
	}
	return lo, hi, !first
}

// Faces of one block of cells with the box around them, so a block out of view is skipped whole
type Chunk struct {
	Lo, Hi Vec3
	Quads  []Quad
}

// Side of the cubes of cells a mesh is cut into
const chunkSize = 8

// Faces of every voxel with no neighbor over them, lit at every corner and darkened where voxels crowd it
func (g Grid) Mesh(r *Raster) []Chunk {
	chunks := map[[3]int]*Chunk{}
	for cell, col := range g {
		x, y, z := cell[0], cell[1], cell[2]
		key := [3]int{floorDiv(x, chunkSize), floorDiv(y, chunkSize), floorDiv(z, chunkSize)}
		ch, ok := chunks[key]
		if !ok {
			lo := Vec3{float64(key[0] * chunkSize), float64(key[1] * chunkSize), float64(key[2] * chunkSize)}
			ch = &Chunk{Lo: lo, Hi: lo.Add(Vec3{chunkSize, chunkSize, chunkSize})}
			chunks[key] = ch
		}
		for side := range 6 {
			n := sideNormals[side]
			if g.Solid(x+n[0], y+n[1], z+n[2]) {
				continue
			}
			q := faceQuad(x, y, z, side, col)
			r.Shade(&q, boxNormals[side])
			for c := range 4 {
				q.Light[c] = q.Light[c].Scale(1 - 0.2*float64(g.occlusion(x, y, z, side, c)))
			}
			ch.Quads = append(ch.Quads, q)
		}
	}
	out := make([]Chunk, 0, len(chunks))
	for _, ch := range chunks {
		out = append(out, *ch)
	}
	return out
}

// Queues the faces of every chunk the camera may see
func (r *Raster) Chunks(chunks []Chunk) {
	for i := range chunks {
		ch := &chunks[i]
		if !r.Visible(ch.Lo, ch.Hi) {
			continue
		}
		for j := range ch.Quads {
			r.Quad(ch.Quads[j])
		}
	}
}

// Integer division rounding toward negative infinity
func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// Outward normal of each box side, top, bottom, north, south, west, east
var sideNormals = [6][3]int{{0, 1, 0}, {0, -1, 0}, {0, 0, -1}, {0, 0, 1}, {-1, 0, 0}, {1, 0, 0}}

// Corner offsets of each side in the order the faces are wound
var sideCorners = [6][4][3]int{
	{{0, 1, 0}, {0, 1, 1}, {1, 1, 1}, {1, 1, 0}},
	{{0, 0, 1}, {0, 0, 0}, {1, 0, 0}, {1, 0, 1}},
	{{0, 0, 0}, {0, 1, 0}, {1, 1, 0}, {1, 0, 0}},
	{{1, 0, 1}, {1, 1, 1}, {0, 1, 1}, {0, 0, 1}},
	{{0, 0, 1}, {0, 1, 1}, {0, 1, 0}, {0, 0, 0}},
	{{1, 0, 0}, {1, 1, 0}, {1, 1, 1}, {1, 0, 1}},
}

// One face of a unit voxel at a cell, unlit
func faceQuad(x, y, z, side int, col RGB) Quad {
	var q Quad
	for c := range 4 {
		o := sideCorners[side][c]
		q.P[c] = Vec3{float64(x + o[0]), float64(y + o[1]), float64(z + o[2])}
	}
	q.Color, q.Alpha = col, 1
	return q
}

// Voxels crowding a face corner, two along its edges and one diagonal, three when both edges are solid
func (g Grid) occlusion(x, y, z, side, corner int) int {
	n := sideNormals[side]
	o := sideCorners[side][corner]
	dir := func(axis int) int {
		if n[axis] != 0 {
			return 0
		}
		if o[axis] == 1 {
			return 1
		}
		return -1
	}
	dx, dy, dz := dir(0), dir(1), dir(2)
	bx, by, bz := x+n[0], y+n[1], z+n[2]
	var axes [2][3]int
	k := 0
	for _, d := range [][3]int{{dx, 0, 0}, {0, dy, 0}, {0, 0, dz}} {
		if d != [3]int{} && k < 2 {
			axes[k] = d
			k++
		}
	}
	if k != 2 {
		return 0
	}
	s1 := g.Solid(bx+axes[0][0], by+axes[0][1], bz+axes[0][2])
	s2 := g.Solid(bx+axes[1][0], by+axes[1][1], bz+axes[1][2])
	if s1 && s2 {
		return 3
	}
	c := g.Solid(bx+axes[0][0]+axes[1][0], by+axes[0][1]+axes[1][1], bz+axes[0][2]+axes[1][2])
	n2 := 0
	for _, b := range []bool{s1, s2, c} {
		if b {
			n2++
		}
	}
	return n2
}

// Cells on a straight line between two cells on the floor, ends included
func LineCells(x0, z0, x1, z1 int) [][2]int {
	dx, dz := x1-x0, z1-z0
	steps := max(int(math.Abs(float64(dx))), int(math.Abs(float64(dz))), 1)
	out := make([][2]int, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		out = append(out, [2]int{x0 + int(math.Round(float64(dx)*t)), z0 + int(math.Round(float64(dz)*t))})
	}
	return out
}
