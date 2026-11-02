package vxl

import "math"

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
	p := &Part{Name: name, At: at, Pivot: pivot}
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

// Queues every face of the model in its stance, lit by the raster, glowing keys unlit
func (m *Model) Draw(r *Raster, pal Palette, st Stance) {
	alpha := 1 - st.Fade
	if alpha <= 0 {
		return
	}
	for _, part := range m.Parts {
		if part.Hidden(st.Poses) {
			continue
		}
		for i := range part.faces {
			f := &part.faces[i]
			var q Quad
			for c := range 4 {
				q.P[c] = part.Place(f.p[c], st.Poses).Mul(m.Scale).RotateY(st.Yaw).Add(st.Pos)
			}
			q.Color, q.Alpha = pal.color(f.key), alpha
			if pal.Glow[f.key] {
				l := Grey(1.5)
				q.Light = [4]RGB{l, l, l, l}
				r.Quad(q)
				continue
			}
			if st.Tint != nil {
				q.Color = st.Tint(q.Color)
			}
			n := part.Orient(f.n, st.Poses).RotateY(st.Yaw)
			r.Shade(&q, n)
			for c := range 4 {
				q.Light[c] = q.Light[c].Scale(f.ao[c])
			}
			r.Quad(q)
		}
	}
}

// Point of the model in the world for a point in its voxel units, before any pose
func (m *Model) World(v Vec3, st Stance) Vec3 { return v.Mul(m.Scale).RotateY(st.Yaw).Add(st.Pos) }

// Height of the model in world units
func (m *Model) Height() float64 {
	top := 0.0
	for _, p := range m.Parts {
		top = max(top, (p.hi.Y + p.At.Y))
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
