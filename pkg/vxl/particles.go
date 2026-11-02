package vxl

// Mote of light with a life, falling under gravity when it has weight
type Particle struct {
	Pos      Vec3
	Vel      Vec3
	Age      float64
	Life     float64
	Col      RGB
	Size     float64
	Weight   float64
	Drag     float64
	Additive bool
	// Height the mote comes to rest at, none when zero
	Floor float64
}

// Pool of motes capped at a count, the oldest dropped first past it
type Particles struct {
	Max  int
	list []Particle
}

// Adds a mote while there is room for it
func (ps *Particles) Add(p Particle) {
	if ps.Max > 0 && len(ps.list) >= ps.Max {
		return
	}
	ps.list = append(ps.list, p)
}

// Count of motes alive
func (ps *Particles) Len() int { return len(ps.list) }

// Reports whether a count of motes still fits
func (ps *Particles) Room(n int) bool { return ps.Max <= 0 || len(ps.list)+n <= ps.Max }

// Advances every mote by dt seconds, dropping the ones past their life
func (ps *Particles) Update(dt float64) {
	alive := ps.list[:0]
	for i := range ps.list {
		p := &ps.list[i]
		p.Age += dt
		if p.Age >= p.Life {
			continue
		}
		p.Vel.Y -= p.Weight * dt
		if p.Drag > 0 {
			p.Vel = p.Vel.Mul(1 - min(dt*p.Drag, 1))
		}
		p.Pos = p.Pos.Add(p.Vel.Mul(dt))
		if p.Floor != 0 && p.Pos.Y < p.Floor && p.Weight > 0 {
			p.Pos.Y = p.Floor
			p.Vel = Vec3{}
		}
		alive = append(alive, *p)
	}
	ps.list = alive
}

// Draws every mote as a billboard fading with its age
func (ps *Particles) Draw(r *Raster) {
	for i := range ps.list {
		p := &ps.list[i]
		fade := 1 - p.Age/p.Life
		r.Billboard(p.Pos, p.Size*(0.7+0.3*fade), p.Col, fade*0.9, p.Additive)
	}
}

// Drops every mote
func (ps *Particles) Clear() { ps.list = ps.list[:0] }
