package sprint

import (
	"math"
)

// What a member is doing, driving its pose and motion
type actorState int

const (
	stateIdle actorState = iota
	stateWalk
	stateWork
	stateThink
	stateWait
	stateBlocked
	stateFail
	stateCheer
	stateSlump
)

// One thing an actor does next, walking somewhere, picking a crate up, putting it down or standing a while
type step struct {
	kind   stepKind
	to     Vec3
	crate  *crate
	holder holder
	dur    float64
}

type stepKind int

const (
	stepWalk stepKind = iota
	stepPick
	stepPlace
	stepWait
)

// Member of the team as a voxel character on the floor
type actor struct {
	name   string
	role   Role
	index  int
	col    RGB
	hat    string
	pos    Vec3
	yaw    float64
	home   Vec3
	face   float64
	steps  []step
	state  actorState
	rest   actorState
	phase  float64
	bob    float64
	carry  *crate
	tool   string
	toolAt float64
	emote  string
	emoteT float64
	nod    float64
	// Height above the floor while dropping in
	drop    float64
	dropVel float64
	landed  bool
	// Share of a tint toward failure red
	gloom float64
}

// Speed of a walking actor in floor units per second
const walkSpeed = 7.0

// Where the head of the actor is, for labels, beams and emotes
func (a *actor) headPos() Vec3 { return a.pos.Add(Vec3{0, 4.9 + a.bob + a.drop, 0}) }

// Where the actor holds a crate while carrying it
func (a *actor) handsPos() Vec3 {
	f := a.facing()
	return a.pos.Add(f.Mul(0.95)).Add(Vec3{0, 2.1 + a.bob + a.drop, 0})
}

// Unit vector the actor faces along
func (a *actor) facing() Vec3 { return Vec3{math.Sin(a.yaw), 0, math.Cos(a.yaw)} }

// Points the actor at a spot on the floor
func (a *actor) lookAt(p Vec3) {
	d := p.Sub(a.pos)
	if d.X*d.X+d.Z*d.Z > 1e-6 {
		a.face = math.Atan2(d.X, d.Z)
	}
}

// Reports whether the actor has an errand to run
func (a *actor) busy() bool { return len(a.steps) > 0 }

// Clears the errands and sends the actor walking to a spot, through the corridor when it crosses the floor
func (a *actor) walkTo(to Vec3) {
	a.steps = a.steps[:0]
	a.addWalk(to)
}

// Adds a walk to a spot to the errands, through the corridor when it crosses the floor
func (a *actor) addWalk(to Vec3) {
	from := a.pos
	if len(a.steps) > 0 {
		for i := len(a.steps) - 1; i >= 0; i-- {
			if a.steps[i].kind == stepWalk {
				from = a.steps[i].to
				break
			}
		}
	}
	for _, p := range route(from, to) {
		a.steps = append(a.steps, step{kind: stepWalk, to: p})
	}
}

// Waypoints from one spot to another, straight when near and on one side of the corridor, else along the corridor
func route(from, to Vec3) []Vec3 {
	sameSide := (from.Z < corridorZ) == (to.Z < corridorZ)
	if sameSide && from.Sub(to).Len() < 9 {
		return []Vec3{to}
	}
	return []Vec3{{from.X, 0, corridorZ}, {to.X, 0, corridorZ}, to}
}

// Advances the errands and the pose by dt seconds
func (a *actor) update(dt float64, sc *scene) {
	if !a.landed {
		a.dropVel -= 40 * dt
		a.drop += a.dropVel * dt
		if a.drop <= 0 {
			a.drop, a.dropVel, a.landed = 0, 0, true
			sc.puff(a.pos, 10, hexRGB(0x9aa6b5))
		}
		a.phase += dt
		return
	}
	if len(a.steps) == 0 && !sc.ended {
		if d := a.home.Sub(a.pos); d.X*d.X+d.Z*d.Z > 0.09 {
			a.walkTo(a.home)
		}
	}
	if a.nod > 0 {
		a.nod = max(a.nod-dt, 0)
	}
	if a.emoteT > 0 {
		a.emoteT -= dt
		if a.emoteT <= 0 {
			a.emote = ""
		}
	}
	a.gloom += (flagF(a.rest == stateFail || a.rest == stateSlump) - a.gloom) * min(dt*2, 1)
	if len(a.steps) == 0 {
		a.state = a.rest
		a.turnToward(a.face, dt)
		a.pose(dt)
		return
	}
	s := &a.steps[0]
	switch s.kind {
	case stepWalk:
		d := s.to.Sub(a.pos)
		d.Y = 0
		dist := d.Len()
		if dist < 0.05 {
			a.pos.X, a.pos.Z = s.to.X, s.to.Z
			a.steps = a.steps[1:]
			if len(a.steps) == 0 {
				a.state = a.rest
			}
			return
		}
		a.state = stateWalk
		a.lookAt(s.to)
		a.turnToward(a.face, dt)
		stepLen := min(walkSpeed*dt, dist)
		a.pos = a.pos.Add(d.Mul(stepLen / dist))
		a.phase += dt * 11
		a.bob = 0.09 * math.Abs(math.Sin(a.phase))
	case stepPick:
		if s.crate != nil {
			s.crate.carrier = a
			s.crate.flying = false
			a.carry = s.crate
		}
		a.steps = a.steps[1:]
	case stepPlace:
		if s.crate != nil && s.crate.carrier == a {
			s.crate.carrier = nil
			a.carry = nil
			s.crate.settle(s.holder, sc)
		}
		a.steps = a.steps[1:]
	case stepWait:
		s.dur -= dt
		a.state = stateWait
		a.pose(dt)
		if s.dur <= 0 {
			a.steps = a.steps[1:]
		}
	}
}

// Turns the actor toward a heading at a steady rate
func (a *actor) turnToward(target, dt float64) {
	d := math.Mod(target-a.yaw+3*math.Pi, 2*math.Pi) - math.Pi
	turn := 9 * dt
	if math.Abs(d) <= turn {
		a.yaw = target
		return
	}
	a.yaw += math.Copysign(turn, d)
}

// Advances the standing pose of the actor by its state
func (a *actor) pose(dt float64) {
	switch a.state {
	case stateWork:
		a.phase += dt * 20
		a.bob = 0.02 * math.Sin(a.phase*0.3)
	case stateThink:
		a.phase += dt * 2
		a.bob = 0.04 * math.Sin(a.phase)
	case stateCheer:
		a.phase += dt * 7
		a.bob = 1.1 * math.Max(0, math.Sin(a.phase))
	case stateWait, stateBlocked:
		a.phase += dt * 3
		a.bob = 0.03 * math.Sin(a.phase)
	case stateFail, stateSlump:
		a.phase += dt
		a.bob = -0.08 + 0.02*math.Sin(a.phase)
	default:
		a.phase += dt * 2
		a.bob = 0.04 * math.Sin(a.phase)
	}
}

func flagF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Box turned by a yaw around its own center, lit by the sun on its turned sides
func orientedBox(center, size Vec3, yaw float64, col RGB, out []Quad) []Quad {
	hx, hy, hz := size.X/2, size.Y/2, size.Z/2
	corner := func(sx, sy, sz float64) Vec3 {
		return Vec3{sx * hx, sy * hy, sz * hz}.RotateY(yaw).Add(center)
	}
	faces := [6][4][3]float64{
		{{-1, 1, -1}, {-1, 1, 1}, {1, 1, 1}, {1, 1, -1}},
		{{-1, -1, 1}, {-1, -1, -1}, {1, -1, -1}, {1, -1, 1}},
		{{-1, -1, -1}, {-1, 1, -1}, {1, 1, -1}, {1, -1, -1}},
		{{1, -1, 1}, {1, 1, 1}, {-1, 1, 1}, {-1, -1, 1}},
		{{-1, -1, 1}, {-1, 1, 1}, {-1, 1, -1}, {-1, -1, -1}},
		{{1, -1, -1}, {1, 1, -1}, {1, 1, 1}, {1, -1, 1}},
	}
	normals := [6]Vec3{{0, 1, 0}, {0, -1, 0}, {0, 0, -1}, {0, 0, 1}, {-1, 0, 0}, {1, 0, 0}}
	for i, f := range faces {
		n := normals[i].RotateY(yaw)
		l := 0.5 + 0.55*max(n.Dot(sunDir), 0) + 0.1*n.Y
		var q Quad
		for c := range 4 {
			q.P[c] = corner(f[c][0], f[c][1], f[c][2])
			q.Light[c] = l
		}
		q.Color, q.Alpha = col, 1
		out = append(out, q)
	}
	return out
}

// Faces of the character in its pose, built from a few turned boxes
func (a *actor) quads(t float64, out []Quad) []Quad {
	base := a.pos.Add(Vec3{0, a.bob + a.drop, 0})
	tint := func(c RGB) RGB { return c.Mix(hexRGB(0x5a2a33), a.gloom*0.5) }
	yaw := a.yaw
	part := func(lx, y0, lz, w, h, d float64, col RGB) {
		center := Vec3{lx, 0, lz}.RotateY(yaw).Add(base).Add(Vec3{0, y0 + h/2, 0})
		out = orientedBox(center, Vec3{w, h, d}, yaw, tint(col), out)
	}
	swing := 0.0
	if a.state == stateWalk {
		swing = math.Sin(a.phase)
	}
	// Legs and shoes
	for _, side := range []float64{-1, 1} {
		lz := 0.35 * swing * side
		part(side*0.32, 0, lz, 0.5, 0.3, 0.75, colShoes)
		part(side*0.32, 0.3, lz*0.6, 0.48, 1.2, 0.6, colPants)
	}
	// Torso
	part(0, 1.5, 0, 1.45, 2, 0.85, a.col)
	part(0, 3.3, 0.1, 0.5, 0.2, 0.5, colSkin)
	// Arms with hands, posed by state
	for _, side := range []float64{-1, 1} {
		ax, ay, az := side*0.98, 1.7, 0.0
		h := 1.8
		switch a.state {
		case stateWalk:
			az = -0.3 * swing * side
		case stateWork:
			ay, az = 1.6, 0.55
			h = 1.5
			az += 0.06 * math.Sin(a.phase+side*1.7)
		case stateThink:
			if side > 0 {
				ay, az = 2.6, 0.5
				h = 1.5
			}
		case stateWait:
			ax = side * 1.08
		case stateBlocked:
			if side > 0 {
				ay, h = 3.2, 1.9
			}
		case stateCheer:
			ay, h = 3.3, 1.8
			az = 0.1
		case stateFail, stateSlump:
			az = 0.15
		}
		part(ax, ay, az, 0.42, h, 0.5, a.col)
		hy := ay - 0.35
		if a.state == stateBlocked && side > 0 || a.state == stateCheer {
			hy = ay + h
		}
		if a.state == stateWork || a.state == stateThink && side > 0 {
			hy = ay - 0.3
		}
		part(ax, hy, az, 0.4, 0.35, 0.5, colSkin)
	}
	// Head, eyes and hair
	hz, hy := 0.0, 3.5
	switch a.state {
	case stateWork:
		hz, hy = 0.12, 3.42
	case stateThink:
		hz = -0.05
	case stateFail, stateSlump:
		hz, hy = 0.2, 3.25
	}
	if a.nod > 0 {
		hy -= 0.12 * math.Abs(math.Sin(a.nod*18))
	}
	part(0, hy, hz, 1.3, 1.3, 1.3, colSkin)
	for _, side := range []float64{-1, 1} {
		part(side*0.3, hy+0.65, hz+0.62, 0.22, 0.22, 0.12, colEye)
	}
	if a.hat != "fedora" && a.hat != "hardhat" && a.hat != "beanie" {
		part(0, hy+1.3, hz, 1.36, 0.28, 1.36, colHair)
		part(0, hy+0.75, hz-0.6, 1.36, 0.6, 0.18, colHair)
	}
	top := hy + 1.3
	hatCol := a.col
	switch a.hat {
	case "cap":
		part(0, top, hz, 1.42, 0.36, 1.42, hatCol)
		part(0, top-0.05, hz+1.0, 1.0, 0.12, 0.7, hatCol.Scale(0.8))
	case "beanie":
		part(0, top, hz, 1.42, 0.5, 1.42, hatCol)
		part(0, top+0.5, hz, 0.45, 0.4, 0.45, hatCol.Mix(colPaper, 0.5))
	case "fedora":
		part(0, top, hz, 2.2, 0.12, 2.2, hatCol.Scale(0.7))
		part(0, top+0.12, hz, 1.32, 0.7, 1.32, hatCol.Scale(0.85))
		part(0, top+0.2, hz, 1.36, 0.16, 1.36, hatCol.Scale(0.45))
	case "hardhat":
		part(0, top-0.05, hz, 1.75, 0.1, 1.75, colHardhat.Scale(0.85))
		part(0, top, hz, 1.5, 0.45, 1.5, colHardhat)
		part(0, top+0.45, hz, 0.3, 0.15, 1.5, colHardhat.Scale(0.9))
	case "visor":
		part(0, hy+0.5, hz+0.68, 1.45, 0.38, 0.22, colVisor)
		part(0, hy+0.56, hz+0.78, 1.25, 0.24, 0.08, colVisorGlass)
	case "headphones":
		part(0, top+0.1, hz, 1.6, 0.16, 0.34, hatCol.Scale(0.6))
		for _, side := range []float64{-1, 1} {
			part(side*0.8, hy+0.35, hz, 0.3, 0.55, 0.55, hatCol)
		}
	case "bandana":
		part(0, top-0.1, hz, 1.42, 0.34, 1.42, hatCol)
		part(0, top-0.4, hz-0.9, 0.34, 0.34, 0.6, hatCol.Scale(0.85))
	case "crown":
		part(0, top, hz, 1.42, 0.3, 1.42, colHardhat)
		for _, c := range [][2]float64{{-0.55, -0.55}, {0.55, -0.55}, {-0.55, 0.55}, {0.55, 0.55}} {
			part(c[0], top+0.3, hz+c[1], 0.22, 0.32, 0.22, colHardhat)
		}
	}
	// Role props
	switch {
	case a.role == RoleArchitect && a.state != stateWalk && a.carry == nil:
		part(-0.98, 1.3, 0.35, 0.26, 0.26, 1.3, colMarker)
		part(-0.98, 1.3, 0.35, 0.3, 0.3, 0.2, colPaper)
	case a.role == RoleReviewer && a.state == stateWork:
		part(-0.95, 2.0, 0.55, 0.65, 0.85, 0.08, colPaper)
	}
	return out
}

// Where a crate rests
type holder struct {
	kind   holderKind
	column int
	slot   int
	desk   int
}

type holderKind int

const (
	holdColumn holderKind = iota
	holdDesk
	holdReview
	holdCarried
	holdScrap
)

// Ticket as a crate on the floor, carried, resting or flying between resting places
type crate struct {
	id      string
	status  TicketStatus
	pos     Vec3
	at      holder
	carrier *actor
	flying  bool
	from    Vec3
	to      Vec3
	flyT    float64
	flyDur  float64
	spin    float64
	glow    float64
	// Share of the way a fresh crate has dropped in from above
	drop float64
}

// Size of a crate in floor units
const crateSize = 1.4

// Spot a holder keeps a crate at, its center
func (h holder) spot() Vec3 {
	switch h.kind {
	case holdColumn:
		top := columnTop(h.column)
		stack, level := h.slot/3, h.slot%3
		dx := []float64{-0.6, 0.6, -0.6, 0.6}[stack%4]
		dz := []float64{0.4, 0.4, -0.75, -0.75}[stack%4]
		return top.Add(Vec3{dx, crateSize/2 + crateSize*float64(level) + 0.02*float64(stack/4), dz})
	case holdDesk:
		d := deskAt(h.desk)
		return Vec3{d.X + 1.4, 2 + crateSize/2, d.Z - 0.3}
	case holdReview:
		return Vec3{reviewDesk.X - 0.6, 2 + crateSize/2, reviewDesk.Z - 0.3}
	case holdScrap:
		return Vec3{scrapAt.X, 0.8 + crateSize/2, scrapAt.Z}
	}
	return Vec3{}
}

// Sends the crate on an arc to a resting place
func (c *crate) settle(h holder, sc *scene) {
	c.at = h
	to := h.spot()
	if c.pos.Sub(to).Len() < 0.05 {
		c.pos = to
		c.flying = false
		return
	}
	c.from, c.to, c.flyT, c.flying = c.pos, to, 0, true
	c.flyDur = min(max(c.from.Sub(to).Len()/18, 0.35), 1.2)
}

// Advances the crate, riding its carrier, flying its arc or resting
func (c *crate) update(dt float64, sc *scene) {
	c.glow = max(c.glow-dt, 0)
	if c.carrier != nil {
		c.pos = c.carrier.handsPos()
		c.spin = c.carrier.yaw
		return
	}
	if c.drop < 1 {
		c.drop = min(c.drop+dt*2.2, 1)
		rest := c.at.spot()
		h := (1 - c.drop) * (1 - c.drop) * 7
		c.pos = rest.Add(Vec3{0, h, 0})
		if c.drop >= 1 {
			sc.puff(rest.Sub(Vec3{0, crateSize / 2, 0}), 6, crateColor(c.status).Mix(colPaper, 0.4))
		}
		return
	}
	if !c.flying {
		c.pos = c.at.spot()
		return
	}
	c.flyT += dt
	t := min(c.flyT/c.flyDur, 1)
	e := t * t * (3 - 2*t)
	p := c.from.Lerp(c.to, e)
	p.Y += math.Sin(t*math.Pi) * (1.5 + c.from.Sub(c.to).Len()*0.12)
	c.pos = p
	c.spin += dt * 4
	if t >= 1 {
		c.flying = false
		c.pos = c.to
		c.spin = 0
		sc.puff(c.to.Sub(Vec3{0, crateSize / 2, 0}), 8, crateColor(c.status).Mix(colPaper, 0.5))
	}
}

// Faces of the crate, a box with a lighter lid and darker edge bands
func (c *crate) quads(out []Quad) []Quad {
	col := crateColor(c.status)
	if c.glow > 0 {
		col = col.Mix(colPaper, c.glow*0.6)
	}
	s := crateSize
	out = orientedBox(c.pos, Vec3{s, s, s}, c.spin, col, out)
	band := col.Scale(0.55)
	out = orientedBox(c.pos.Add(Vec3{0, s*0.5 - 0.06, 0}), Vec3{s * 1.02, 0.1, s * 1.02}, c.spin, band, out)
	out = orientedBox(c.pos.Add(Vec3{0, -s*0.5 + 0.06, 0}), Vec3{s * 1.02, 0.1, s * 1.02}, c.spin, band, out)
	out = orientedBox(c.pos.Add(Vec3{0, 0, 0}), Vec3{0.14, s * 1.02, s * 1.02}, c.spin, band, out)
	return out
}

// Mote of light with a life, falling under gravity when it has weight
type particle struct {
	pos      Vec3
	vel      Vec3
	age      float64
	life     float64
	col      RGB
	size     float64
	weight   float64
	additive bool
}

// Stream of light between two actors while one tells the other something
type beam struct {
	from *actor
	to   *actor
	col  RGB
	age  float64
	life float64
}
