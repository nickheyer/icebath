package vxl

import (
	"cmp"
	"math"
	"slices"
)

// Static voxels by integer cell
type Grid map[[3]int]Voxel

// Color, pattern, gloss and glow of one voxel
type Voxel struct {
	Col RGB
	Tex Texture
	// Share of the light its faces mirror
	Gloss float64
	// Light its faces give off on their own, scaling the color, none at zero
	Emit float64
}

// Sets a plain voxel at a cell
func (g Grid) Set(x, y, z int, col RGB) { g[[3]int{x, y, z}] = Voxel{Col: col} }

// Sets a voxel painted with a pattern at a cell
func (g Grid) SetTex(x, y, z int, col RGB, tex Texture) {
	g[[3]int{x, y, z}] = Voxel{Col: col, Tex: tex}
}

// Sets a voxel of any make at a cell
func (g Grid) Put(x, y, z int, v Voxel) { g[[3]int{x, y, z}] = v }

// Fills a box of voxels of one make from a least corner and a size in cells
func (g Grid) Fill(x, y, z, w, h, d int, v Voxel) {
	for i := range w {
		for j := range h {
			for k := range d {
				g[[3]int{x + i, y + j, z + k}] = v
			}
		}
	}
}

// Removes the voxel at a cell
func (g Grid) Del(x, y, z int) { delete(g, [3]int{x, y, z}) }

// Fills a box of plain voxels from a least corner and a size in cells
func (g Grid) Box(x, y, z, w, h, d int, col RGB) { g.BoxTex(x, y, z, w, h, d, col, TexNone) }

// Fills a box of patterned voxels from a least corner and a size in cells
func (g Grid) BoxTex(x, y, z, w, h, d int, col RGB, tex Texture) {
	for i := range w {
		for j := range h {
			for k := range d {
				g.SetTex(x+i, y+j, z+k, col, tex)
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

// Open face of one voxel in a layer of a chunk, with the crowding of each of its corners
type cellFace struct {
	vox Voxel
	ao  [4]int8
	ok  bool
}

// Faces of every layer of one side of a chunk, by layer then the two axes across it
type sideMask [chunkSize][chunkSize][chunkSize]cellFace

// Open voxel faces merged into rectangles of one voxel and crowding, patterns tiling in world units
func (g Grid) Mesh(r *Raster) []Chunk {
	byChunk := map[[3]int][][3]int{}
	for cell := range g {
		key := [3]int{floorDiv(cell[0], chunkSize), floorDiv(cell[1], chunkSize), floorDiv(cell[2], chunkSize)}
		byChunk[key] = append(byChunk[key], cell)
	}
	keys := make([][3]int, 0, len(byChunk))
	for k := range byChunk {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [3]int) int {
		return cmp.Or(cmp.Compare(a[1], b[1]), cmp.Compare(a[2], b[2]), cmp.Compare(a[0], b[0]))
	})
	mask := new(sideMask)
	out := make([]Chunk, 0, len(keys))
	for _, key := range keys {
		base := [3]int{key[0] * chunkSize, key[1] * chunkSize, key[2] * chunkSize}
		lo := Vec3{float64(base[0]), float64(base[1]), float64(base[2])}
		ch := Chunk{Lo: lo, Hi: lo.Add(Vec3{chunkSize, chunkSize, chunkSize})}
		for side := range 6 {
			g.maskSide(mask, byChunk[key], base, side)
			ch.Quads = mergeSide(mask, base, side, r, ch.Quads)
		}
		out = append(out, ch)
	}
	return out
}

// Fills the mask of one side of a chunk with the open faces of its voxels on that side
func (g Grid) maskSide(mask *sideMask, cells [][3]int, base [3]int, side int) {
	*mask = sideMask{}
	n := sideNormals[side]
	axis, u, w := sideAxes(side)
	for _, c := range cells {
		if g.Solid(c[0]+n[0], c[1]+n[1], c[2]+n[2]) {
			continue
		}
		f := cellFace{vox: g[c], ok: true}
		for k := range 4 {
			f.ao[k] = int8(g.occlusion(c[0], c[1], c[2], side, k))
		}
		mask[c[axis]-base[axis]][c[u]-base[u]][c[w]-base[w]] = f
	}
}

// Axis a side faces along and the two axes across it, in the order its corners run
func sideAxes(side int) (axis, u, w int) {
	n := sideNormals[side]
	for n[axis] == 0 {
		axis++
	}
	return axis, (axis + 1) % 3, (axis + 2) % 3
}

// Reports whether the crowding of a face holds steady along an axis across it, letting neighbors merge that way
func flatAlong(side, axis int, ao [4]int8) bool {
	normal, _, _ := sideAxes(side)
	other := 3 - normal - axis
	for a := range 4 {
		for b := range 4 {
			oa, ob := sideCorners[side][a], sideCorners[side][b]
			if oa[axis] != ob[axis] && oa[other] == ob[other] && ao[a] != ao[b] {
				return false
			}
		}
	}
	return true
}

// Appends the merged faces of one side of a chunk, shaded at their corners with world unit pattern places
func mergeSide(mask *sideMask, base [3]int, side int, r *Raster, quads []Quad) []Quad {
	axis, u, w := sideAxes(side)
	for layer := range chunkSize {
		m := &mask[layer]
		for a := range chunkSize {
			for b := range chunkSize {
				f := m[a][b]
				if !f.ok {
					continue
				}
				same := func(a2, b2 int) bool {
					o := m[a2][b2]
					return o.ok && o.vox == f.vox && o.ao == f.ao
				}
				wa := 1
				if flatAlong(side, u, f.ao) {
					for a+wa < chunkSize && same(a+wa, b) {
						wa++
					}
				}
				wb := 1
				if flatAlong(side, w, f.ao) {
					for b+wb < chunkSize {
						fits := true
						for i := range wa {
							if !same(a+i, b+wb) {
								fits = false
								break
							}
						}
						if !fits {
							break
						}
						wb++
					}
				}
				for i := range wa {
					for j := range wb {
						m[a+i][b+j].ok = false
					}
				}
				var c, size [3]int
				c[axis], c[u], c[w] = base[axis]+layer, base[u]+a, base[w]+b
				size[axis], size[u], size[w] = 1, wa, wb
				q := Quad{Color: f.vox.Col, Alpha: 1, Tex: f.vox.Tex, Gloss: f.vox.Gloss}
				for k := range 4 {
					o := sideCorners[side][k]
					p := [3]float64{float64(c[0] + o[0]*size[0]), float64(c[1] + o[1]*size[1]), float64(c[2] + o[2]*size[2])}
					q.P[k] = Vec3{p[0], p[1], p[2]}
					q.UV[k] = [2]float64{p[u], p[w]}
				}
				if f.vox.Emit > 0 {
					l := Grey(f.vox.Emit)
					q.Light = [4]RGB{l, l, l, l}
					quads = append(quads, q)
					continue
				}
				r.Shade(&q, boxNormals[side])
				for k := range 4 {
					q.Light[k] = q.Light[k].Scale(1 - 0.2*float64(f.ao[k]))
				}
				quads = append(quads, q)
			}
		}
	}
	return quads
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
