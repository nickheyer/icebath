package vxl

import (
	"math"
	"slices"
)

// Paint keys of a model resolved to colors when it is drawn
type Palette struct {
	Colors map[rune]RGB
	// Keys that glow on their own, drawn unlit and bright
	Glow map[rune]bool
}

// Color of a key, magenta for one the palette lacks so the gap shows
func (p Palette) color(key rune) RGB {
	if c, ok := p.Colors[key]; ok {
		return c
	}
	return RGB{1, 0, 1}
}

// Rectangle of voxel faces of one key, in the voxel units of its part
type face struct {
	p   [4]Vec3
	n   Vec3
	ao  [4]float64
	key rune
	// Place of the key among the keys of the model
	ki int
}

// Rigid piece of a model, turned around its pivot and carried by its parent
type Part struct {
	Name string
	// Where the voxel origin of the part sits in the model, in voxel units
	At Vec3
	// Point the part turns around, in its own voxel units
	Pivot  Vec3
	parent *Part
	faces  []face
	lo, hi Vec3
	// Place of the part in the parts of its model
	idx int
}

// Turn and shift of a part for one frame
type Pose struct {
	Pitch, Yaw, Roll float64
	Shift            Vec3
	Hidden           bool
}

// Figure built from posable parts, every voxel one scale wide in the world
type Model struct {
	Parts []*Part
	Scale float64
	// Every paint key the faces use, each once
	keys []rune
}

// Part of a model by name, nil for an unknown one
func (m *Model) Part(name string) *Part {
	for _, p := range m.Parts {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// Adds a part built from layers of text, front layer last, each row read from the top down
func (m *Model) Add(name string, at, pivot Vec3, layers []string, rows int) *Part {
	p := &Part{Name: name, At: at, Pivot: pivot, idx: len(m.Parts)}
	vox := map[[3]int]rune{}
	for i, text := range layers {
		z := i / rows
		y := rows - 1 - i%rows
		for x, key := range []rune(text) {
			if key != '.' && key != ' ' {
				vox[[3]int{x, y, z}] = key
			}
		}
	}
	p.bake(vox)
	for i := range p.faces {
		f := &p.faces[i]
		f.ki = slices.Index(m.keys, f.key)
		if f.ki < 0 {
			f.ki = len(m.keys)
			m.keys = append(m.keys, f.key)
		}
	}
	m.Parts = append(m.Parts, p)
	return p
}

// Hangs a part on another so it follows the turns of its parent
func (m *Model) Link(child, parent string) {
	c, p := m.Part(child), m.Part(parent)
	if c != nil && p != nil {
		c.parent = p
	}
}

// Builds the faces of the voxels, neighbors joined into rectangles where their key and shading agree
func (p *Part) bake(vox map[[3]int]rune) {
	first := true
	for c := range vox {
		v := Vec3{float64(c[0]), float64(c[1]), float64(c[2])}
		if first {
			p.lo, p.hi, first = v, v.Add(Vec3{1, 1, 1}), false
			continue
		}
		p.lo = Vec3{min(p.lo.X, v.X), min(p.lo.Y, v.Y), min(p.lo.Z, v.Z)}
		p.hi = Vec3{max(p.hi.X, v.X+1), max(p.hi.Y, v.Y+1), max(p.hi.Z, v.Z+1)}
	}
	if first {
		return
	}
	solid := func(x, y, z int) bool { _, ok := vox[[3]int{x, y, z}]; return ok }
	lo := [3]int{int(p.lo.X), int(p.lo.Y), int(p.lo.Z)}
	hi := [3]int{int(p.hi.X), int(p.hi.Y), int(p.hi.Z)}
	type cellFace struct {
		key rune
		ao  [4]int
		ok  bool
	}
	for side := range 6 {
		n := sideNormals[side]
		axis := 0
		for n[axis] == 0 {
			axis++
		}
		u, w := (axis+1)%3, (axis+2)%3
		for layer := lo[axis]; layer < hi[axis]; layer++ {
			mask := map[[2]int]cellFace{}
			for a := lo[u]; a < hi[u]; a++ {
				for b := lo[w]; b < hi[w]; b++ {
					var c [3]int
					c[axis], c[u], c[w] = layer, a, b
					key, ok := vox[c]
					if !ok || solid(c[0]+n[0], c[1]+n[1], c[2]+n[2]) {
						continue
					}
					var ao [4]int
					for k := range 4 {
						ao[k] = occlusionIn(solid, c[0], c[1], c[2], side, k)
					}
					mask[[2]int{a, b}] = cellFace{key: key, ao: ao, ok: true}
				}
			}
			for a := lo[u]; a < hi[u]; a++ {
				for b := lo[w]; b < hi[w]; b++ {
					f, ok := mask[[2]int{a, b}]
					if !ok {
						continue
					}
					same := func(a2, b2 int) bool {
						g, ok := mask[[2]int{a2, b2}]
						return ok && g.key == f.key && g.ao == f.ao
					}
					wa := 1
					for same(a+wa, b) {
						wa++
					}
					wb := 1
					for {
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
					for i := range wa {
						for j := range wb {
							delete(mask, [2]int{a + i, b + j})
						}
					}
					var c [3]int
					c[axis], c[u], c[w] = layer, a, b
					var size [3]int
					size[axis], size[u], size[w] = 1, wa, wb
					var out face
					out.n = boxNormals[side]
					out.key = f.key
					for k := range 4 {
						o := sideCorners[side][k]
						out.p[k] = Vec3{float64(c[0] + o[0]*size[0]), float64(c[1] + o[1]*size[1]), float64(c[2] + o[2]*size[2])}
						out.ao[k] = 1 - 0.22*float64(f.ao[k])
					}
					p.faces = append(p.faces, out)
				}
			}
		}
	}
}

// Voxels crowding a face corner inside one part, as a grid counts them
func occlusionIn(solid func(x, y, z int) bool, x, y, z, side, corner int) int {
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
	d := [3]int{dir(0), dir(1), dir(2)}
	bx, by, bz := x+n[0], y+n[1], z+n[2]
	var axes [2][3]int
	k := 0
	for i := range 3 {
		if d[i] != 0 {
			axes[k][i] = d[i]
			k++
		}
	}
	s1 := solid(bx+axes[0][0], by+axes[0][1], bz+axes[0][2])
	s2 := solid(bx+axes[1][0], by+axes[1][1], bz+axes[1][2])
	if s1 && s2 {
		return 3
	}
	c := solid(bx+axes[0][0]+axes[1][0], by+axes[0][1]+axes[1][1], bz+axes[0][2]+axes[1][2])
	count := 0
	for _, b := range []bool{s1, s2, c} {
		if b {
			count++
		}
	}
	return count
}

// Where a model stands and turns, with the pose of every part
type Stance struct {
	Pos   Vec3
	Yaw   float64
	Poses map[string]Pose
	// Recolors every lit face, nil for none
	Tint func(RGB) RGB
	// Share of the way the model has faded out, none when zero
	Fade float64
}

// Point of a part in the model, after its own turn and those of every part carrying it
func (p *Part) Place(v Vec3, poses map[string]Pose) Vec3 {
	for part := p; part != nil; part = part.parent {
		pose := poses[part.Name]
		v = turn(v.Sub(part.Pivot), pose).Add(part.Pivot).Add(part.At).Add(pose.Shift)
	}
	return v
}

// Direction of a part in the model, after the turns of the chain
func (p *Part) Orient(v Vec3, poses map[string]Pose) Vec3 {
	for part := p; part != nil; part = part.parent {
		v = turn(v, poses[part.Name])
	}
	return v
}

// Vector turned by a pose, roll first, then pitch, then yaw
func turn(v Vec3, pose Pose) Vec3 {
	if pose.Roll != 0 {
		v = v.RotateZ(pose.Roll)
	}
	if pose.Pitch != 0 {
		v = v.RotateX(pose.Pitch)
	}
	if pose.Yaw != 0 {
		v = v.RotateY(pose.Yaw)
	}
	return v
}

// Reports whether a part or any part carrying it is hidden
func (p *Part) Hidden(poses map[string]Pose) bool {
	for part := p; part != nil; part = part.parent {
		if poses[part.Name].Hidden {
			return true
		}
	}
	return false
}

// Linear map as the images of the three unit axes, then a shift
type xform struct{ x, y, z, t Vec3 }

// Vector under the linear part alone
func (m xform) turn(v Vec3) Vec3 {
	return Vec3{m.x.X*v.X + m.y.X*v.Y + m.z.X*v.Z, m.x.Y*v.X + m.y.Y*v.Y + m.z.Y*v.Z, m.x.Z*v.X + m.y.Z*v.Y + m.z.Z*v.Z}
}

// Point under the whole map
func (m xform) apply(v Vec3) Vec3 { return m.turn(v).Add(m.t) }

// Map doing o first and m after
func (m xform) then(o xform) xform { return xform{m.turn(o.x), m.turn(o.y), m.turn(o.z), m.apply(o.t)} }

// Turn of a pose as a map, roll first, then pitch, then yaw
func poseTurn(p Pose) xform {
	m := xform{x: Vec3{1, 0, 0}, y: Vec3{0, 1, 0}, z: Vec3{0, 0, 1}}
	if p.Roll != 0 {
		s, c := math.Sincos(p.Roll)
		m = xform{x: Vec3{c, s, 0}, y: Vec3{-s, c, 0}, z: Vec3{0, 0, 1}}
	}
	if p.Pitch != 0 {
		s, c := math.Sincos(p.Pitch)
		m = xform{x: Vec3{1, 0, 0}, y: Vec3{0, c, s}, z: Vec3{0, -s, c}}.then(m)
	}
	if p.Yaw != 0 {
		s, c := math.Sincos(p.Yaw)
		m = xform{x: Vec3{c, 0, -s}, y: Vec3{0, 1, 0}, z: Vec3{s, 0, c}}.then(m)
	}
	return m
}

// Map of one part from voxel units into the world, and whether it or a carrier is hidden
type partXF struct {
	m      xform
	hidden bool
	done   bool
}

// Fills the map of a part and of every part carrying it, the world map at the root
func (m *Model) place(p *Part, xs []partXF, poses map[string]Pose, world xform) *partXF {
	x := &xs[p.idx]
	if x.done {
		return x
	}
	pose := poses[p.Name]
	r := poseTurn(pose)
	r.t = p.Pivot.Add(p.At).Add(pose.Shift).Sub(r.turn(p.Pivot))
	parent, hidden := world, pose.Hidden
	if p.parent != nil {
		px := m.place(p.parent, xs, poses, world)
		parent, hidden = px.m, hidden || px.hidden
	}
	*x = partXF{m: parent.then(r), hidden: hidden, done: true}
	return x
}

// Queues every front face of the model in its stance, lit by the lights reaching each part, glowing keys unlit
func (m *Model) Draw(r *Raster, pal Palette, st Stance) {
	alpha := 1 - st.Fade
	if alpha <= 0 {
		return
	}
	var buf [24]partXF
	xs := buf[:0]
	if len(m.Parts) > len(buf) {
		xs = make([]partXF, 0, len(m.Parts))
	}
	xs = xs[:len(m.Parts)]
	var colBuf [16]RGB
	var glowBuf [16]bool
	cols, glows := colBuf[:0], glowBuf[:0]
	if len(m.keys) > len(colBuf) {
		cols, glows = make([]RGB, 0, len(m.keys)), make([]bool, 0, len(m.keys))
	}
	for _, k := range m.keys {
		c, g := pal.color(k), pal.Glow[k]
		if !g && st.Tint != nil {
			c = st.Tint(c)
		}
		cols, glows = append(cols, c), append(glows, g)
	}
	s, c := math.Sincos(st.Yaw)
	k := m.Scale
	world := xform{x: Vec3{c * k, 0, -s * k}, y: Vec3{0, k, 0}, z: Vec3{s * k, 0, c * k}, t: st.Pos}
	unscale := 1 / k
	glow := Grey(1.5)
	for _, part := range m.Parts {
		x := m.place(part, xs, st.Poses, world)
		if x.hidden || len(part.faces) == 0 {
			continue
		}
		lo, hi := x.m.box(part.lo, part.hi)
		if !r.Visible(lo, hi) {
			continue
		}
		near := r.lightsInBox(lo, hi)
		for i := range part.faces {
			f := &part.faces[i]
			var q Quad
			for c := range 4 {
				q.P[c] = x.m.apply(f.p[c])
			}
			if q.P[1].Sub(q.P[0]).Cross(q.P[3].Sub(q.P[0])).Dot(r.eye.Sub(q.P[0])) <= 0 {
				continue
			}
			q.Color, q.Alpha = cols[f.ki], alpha
			if glows[f.ki] {
				q.Light = [4]RGB{glow, glow, glow, glow}
				r.quad(&q)
				continue
			}
			r.shadeFrom(&q, x.m.turn(f.n).Mul(unscale), near)
			for c := range 4 {
				q.Light[c] = q.Light[c].Scale(f.ao[c])
			}
			r.quad(&q)
		}
	}
}

// World box around the image of a box under the map, padded a hair against rounding
func (m xform) box(lo, hi Vec3) (Vec3, Vec3) {
	inf := math.Inf(1)
	a, b := Vec3{inf, inf, inf}, Vec3{-inf, -inf, -inf}
	for i := range 8 {
		p := lo
		if i&1 != 0 {
			p.X = hi.X
		}
		if i&2 != 0 {
			p.Y = hi.Y
		}
		if i&4 != 0 {
			p.Z = hi.Z
		}
		w := m.apply(p)
		a = Vec3{min(a.X, w.X), min(a.Y, w.Y), min(a.Z, w.Z)}
		b = Vec3{max(b.X, w.X), max(b.Y, w.Y), max(b.Z, w.Z)}
	}
	pad := Vec3{1e-9, 1e-9, 1e-9}
	return a.Sub(pad), b.Add(pad)
}

// Point of the model in the world for a point in its voxel units, before any pose
func (m *Model) World(v Vec3, st Stance) Vec3 { return v.Mul(m.Scale).RotateY(st.Yaw).Add(st.Pos) }

// Rest stature in world units, every part raised by the parts carrying it
func (m *Model) Height() float64 {
	top := 0.0
	for _, p := range m.Parts {
		y := p.hi.Y
		for part := p; part != nil; part = part.parent {
			y += part.At.Y
		}
		top = max(top, y)
	}
	return top * m.Scale
}

// Yaw turned toward a target yaw by at most a step
func ApproachYaw(yaw, target, step float64) float64 {
	d := TurnBetween(yaw, target)
	if math.Abs(d) <= step {
		return target
	}
	return yaw + math.Copysign(step, d)
}
