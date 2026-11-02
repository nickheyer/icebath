package sprint

import (
	"math"
	"strings"
	"time"
)

// What the harness says a member is doing right now
type liveState struct {
	running  bool
	tool     string
	toolAt   time.Time
	approval bool
	asking   bool
	failed   bool
}

// Most motes alive at once
const maxParticles = 420

// Seconds the team cheers after a sprint is done
const cheerTime = 25.0

// The floor with everyone and everything on it, advanced frame by frame from the state of a sprint
type scene struct {
	island   grid
	static   []Quad
	builders int
	actors   []*actor
	byName   map[string]*actor
	crates   map[string]*crate
	order    []string
	parts    []particle
	beams    []beam
	t        float64
	stage    Stage
	running  bool
	ended    bool
	endedAt  float64
	lastSeq  int
	seed     uint32
	sprintID string
	// Sky at the top and at the horizon
	zenith  RGB
	horizon RGB
	lobby   *actor
	dyn     []Quad
	dust    float64
	rackHue float64
}

// Floor built for a team of that many builders
func newScene(builders int) *scene {
	sc := &scene{byName: map[string]*actor{}, crates: map[string]*crate{}, seed: 0x1234567, zenith: hexRGB(0x05070a), horizon: hexRGB(0x101a2a)}
	sc.rebuild(builders)
	return sc
}

// Builds the floor again for a team of that many builders
func (sc *scene) rebuild(builders int) {
	sc.builders = max(builders, 1)
	sc.island = buildIsland(sc.builders)
	sc.static = sc.island.mesh()
}

// Pseudo random share from 0 to 1
func (sc *scene) rand() float64 {
	sc.seed = sc.seed*1664525 + 1013904223
	return float64(sc.seed>>8) / float64(1<<24)
}

// Share from -1 to 1
func (sc *scene) spread() float64 { return sc.rand()*2 - 1 }

// Color the screen and sparks of a member take from the tool it runs
func toolColor(name string) RGB {
	switch {
	case name == "":
		return hexRGB(0x9cc7e8)
	case strings.HasPrefix(name, "Sprint"):
		return colPaper
	case strings.HasPrefix(name, "mcp__"):
		return hexRGB(0xf07aa6)
	}
	switch name {
	case "Read", "Glob", "Grep", "Outline":
		return hexRGB(0x5ab4f0)
	case "Edit", "Write":
		return hexRGB(0xf0b429)
	case "Shell", "ShellOutput", "KillShell":
		return hexRGB(0x7fe0b8)
	case "WebFetch":
		return hexRGB(0x9b6bff)
	case "Agent":
		return hexRGB(0xff7f66)
	}
	return hexRGB(0x9cc7e8)
}

// Brings the floor in line with a sprint, nil for none, and what the harness says each member is doing
func (sc *scene) sync(snap *Snapshot, live map[string]liveState, cfg Config) {
	if snap == nil {
		sc.syncLobby()
		return
	}
	if sc.sprintID != snap.ID {
		sc.reset(snap.ID)
	}
	sc.lobby = nil
	if n := countRole(snap.Members, RoleBuilder); n > sc.builders {
		sc.rebuild(n)
	}
	for _, m := range snap.Members {
		a, ok := sc.byName[m.Name]
		if !ok {
			a = sc.spawn(m, cfg)
		}
		look := cfg.LookOf(m.Role, m.Index)
		a.col, a.hat = dye(look.Color), look.Hat
	}
	sc.syncCrates(snap)
	for _, m := range snap.Members {
		sc.direct(snap, m, live[m.Name])
	}
	sc.syncMessages(snap)
	sc.syncEnd(snap)
	sc.stage, sc.running = snap.Stage, snap.Running()
}

// Count of members of a role
func countRole(members []Member, role Role) int {
	n := 0
	for _, m := range members {
		if m.Role == role {
			n++
		}
	}
	return n
}

// Clears the floor for another sprint
func (sc *scene) reset(id string) {
	sc.sprintID = id
	sc.actors = sc.actors[:0]
	sc.byName = map[string]*actor{}
	sc.crates = map[string]*crate{}
	sc.order = nil
	sc.parts = sc.parts[:0]
	sc.beams = sc.beams[:0]
	sc.lastSeq = 0
	sc.ended, sc.endedAt = false, 0
}

// Drops a new member onto the floor at its station
func (sc *scene) spawn(m Member, cfg Config) *actor {
	look := cfg.LookOf(m.Role, m.Index)
	a := &actor{name: m.Name, role: m.Role, index: m.Index, col: dye(look.Color), hat: look.Hat, drop: 16, phase: sc.rand() * 6}
	a.home, a.face = sc.station(m.Role, m.Index)
	a.pos, a.yaw = a.home, a.face
	sc.actors = append(sc.actors, a)
	sc.byName[m.Name] = a
	return a
}

// Home spot of a role and the way it faces there
func (sc *scene) station(role Role, index int) (Vec3, float64) {
	switch role {
	case RoleDirector:
		return directorSpot, -math.Pi / 2
	case RoleArchitect:
		return architectAt, math.Pi
	case RoleReviewer:
		return reviewerAt, math.Pi
	}
	return builderSpot(index), math.Pi
}

// Spot a builder stands at while its handoff is under review
func boothSpot() Vec3 { return Vec3{reviewDesk.X - 2.6, 0, reviewerAt.Z + 0.2} }

// Keeps a crate per ticket, each resting where its status says, builders carrying theirs between desk and board
func (sc *scene) syncCrates(snap *Snapshot) {
	slots := map[int]int{}
	for _, t := range snap.Tickets {
		c, ok := sc.crates[t.ID]
		if !ok {
			c = &crate{id: t.ID, status: t.Status}
			sc.crates[t.ID] = c
			sc.order = append(sc.order, t.ID)
			c.at = holder{kind: holdColumn, column: todoColumn, slot: slots[todoColumn]}
			c.pos = c.at.spot().Add(Vec3{0, 7, 0})
			c.drop = 0
		}
		if c.status != t.Status {
			c.status = t.Status
			c.glow = 1
		}
		want := c.at
		switch t.Status {
		case TicketTodo:
			want = holder{kind: holdColumn, column: todoColumn, slot: slots[todoColumn]}
			slots[todoColumn]++
		case TicketActive:
			if a := sc.byName[t.Builder]; a != nil {
				want = holder{kind: holdDesk, desk: a.index}
			}
		case TicketReview:
			want = holder{kind: holdReview}
		case TicketDone:
			want = holder{kind: holdColumn, column: doneColumn, slot: slots[doneColumn]}
			slots[doneColumn]++
		default:
			want = holder{kind: holdColumn, column: failColumn, slot: slots[failColumn]}
			slots[failColumn]++
		}
		if want != c.at && c.drop >= 1 {
			sc.move(c, want, sc.byName[t.Builder])
		}
	}
}

// Gets a crate to a resting place, the builder carrying it when it is on their way, flying it otherwise
func (sc *scene) move(c *crate, want holder, b *actor) {
	if c.carrier != nil && c.carrier != b {
		c.carrier.carry = nil
		c.carrier = nil
	}
	carryable := b != nil && b.landed && (c.at.kind == holdColumn && want.kind == holdDesk && want.desk == b.index || c.at.kind == holdDesk && c.at.desk == b.index && want.kind == holdReview)
	switch {
	case c.carrier == b && b != nil:
		b.steps = b.steps[:0]
		b.addWalk(approach(want))
		b.steps = append(b.steps, step{kind: stepPlace, crate: c, holder: want})
		c.at = want
	case carryable:
		b.steps = b.steps[:0]
		b.addWalk(approach(c.at))
		b.steps = append(b.steps, step{kind: stepPick, crate: c})
		b.addWalk(approach(want))
		b.steps = append(b.steps, step{kind: stepPlace, crate: c, holder: want})
		c.at = want
	default:
		if b != nil {
			b.steps = dropCrateSteps(b.steps, c)
		}
		c.settle(want, sc)
	}
}

// Errands with every step about the crate taken out
func dropCrateSteps(steps []step, c *crate) []step {
	kept := steps[:0]
	for _, s := range steps {
		if s.crate != c {
			kept = append(kept, s)
		}
	}
	return kept
}

// Spot an actor stands at to reach a resting place
func approach(h holder) Vec3 {
	switch h.kind {
	case holdColumn:
		return Vec3{columnsX[h.column] + 0.5, 0, columnZ + 2.6}
	case holdDesk:
		return builderSpot(h.desk)
	case holdReview:
		return boothSpot()
	}
	return Vec3{scrapAt.X - 2, 0, scrapAt.Z}
}

// Sets what a member does and where it stands from the sprint and the harness
func (sc *scene) direct(snap *Snapshot, m Member, st liveState) {
	a := sc.byName[m.Name]
	if a == nil || sc.ended {
		return
	}
	spot, face := sc.station(m.Role, m.Index)
	rest := stateIdle
	working := func() actorState {
		switch {
		case !st.running:
			return stateIdle
		case st.tool != "" && time.Since(st.toolAt) < 4*time.Second:
			return stateWork
		}
		return stateThink
	}
	switch m.Role {
	case RoleDirector:
		if snap.Stage == StageBrief || snap.Stage == StageReport {
			rest = working()
			if st.asking {
				rest = stateWait
				a.emote, a.emoteT = "?", 0.5
			}
		} else {
			spot, face = loungeSpot, -math.Pi/2
			rest = working()
		}
	case RoleArchitect:
		if snap.Stage == StagePlan {
			rest = working()
		} else if st.running {
			rest = working()
		}
	case RoleReviewer:
		if strings.HasPrefix(m.Task, "reviewing") {
			rest = working()
			if rest == stateIdle {
				rest = stateThink
			}
		} else if st.running {
			rest = working()
		}
	case RoleBuilder:
		t, ok := snap.Ticket(m.Ticket)
		switch {
		case !ok:
			rest = working()
		case t.Status == TicketReview:
			spot, face = boothSpot(), math.Pi/4
			rest = stateWait
		case t.Status == TicketActive:
			rest = working()
			if rest == stateIdle && m.Task != "" {
				rest = stateThink
			}
		default:
			rest = working()
		}
	}
	if st.approval {
		rest = stateBlocked
		a.emote, a.emoteT = "!", 0.5
	}
	if st.failed && !st.running {
		rest = stateFail
	}
	if st.tool != "" {
		a.tool, a.toolAt = st.tool, sc.t
	}
	a.rest = rest
	a.home, a.face = spot, face
}

// Shows every exchange since the last sync as a beam between its actors, the one spoken to nodding
func (sc *scene) syncMessages(snap *Snapshot) {
	for _, m := range snap.MessagesAfter(sc.lastSeq) {
		sc.lastSeq = m.Seq
		from, to := sc.byName[m.From], sc.byName[m.To]
		col := hexRGB(0x9cc7e8)
		switch m.Kind {
		case MsgAssign:
			col = colCrateWork
		case MsgHandoff:
			col = colCrateRev
		case MsgVerdict:
			col = colCrateDone
			if strings.HasPrefix(m.Text, string(VerdictRework)) || strings.HasPrefix(m.Text, string(VerdictFailed)) {
				col = colCrateFail
			}
		case MsgQuestion:
			if from != nil {
				from.emote, from.emoteT = "?", 6
			}
			continue
		case MsgAnswer:
			if to != nil {
				to.nod = 1
				to.emote = ""
			}
			continue
		case MsgFailure:
			if from != nil {
				sc.burst(from.headPos(), 14, colCrateFail, 0.5)
				from.emote, from.emoteT = "✘", 4
			}
			continue
		case MsgStage, MsgNote:
			continue
		}
		if from != nil && to != nil {
			sc.beams = append(sc.beams, beam{from: from, to: to, col: col, life: 1.6})
			to.nod = 1
		}
		if m.Kind == MsgVerdict && to != nil {
			if c := sc.crates[m.Ticket]; c != nil && col == colCrateDone {
				sc.burst(c.pos, 20, colCrateDone, 1)
			}
		}
	}
}

// Gathers the team at the plaza to cheer a done sprint or slump over a failed one
func (sc *scene) syncEnd(snap *Snapshot) {
	if snap.Running() {
		if sc.ended {
			sc.ended = false
			for _, a := range sc.actors {
				a.rest = stateIdle
			}
		}
		return
	}
	if sc.ended {
		return
	}
	sc.ended, sc.endedAt = true, sc.t
	cheer := snap.Err == nil
	for i, a := range sc.actors {
		ang := 2 * math.Pi * float64(i) / float64(max(len(sc.actors), 1))
		spot := plazaAt.Add(Vec3{3.2 * math.Cos(ang), 0, 2.2 * math.Sin(ang)})
		a.walkTo(spot)
		a.face = math.Atan2(plazaAt.X-spot.X, plazaAt.Z-spot.Z)
		if cheer {
			a.rest = stateCheer
		} else {
			a.rest = stateSlump
		}
		a.carry = nil
	}
	for _, c := range sc.crates {
		if c.carrier != nil {
			c.carrier = nil
			c.settle(c.at, sc)
		}
	}
}

// Keeps a lone director wandering the floor while no sprint runs
func (sc *scene) syncLobby() {
	if sc.sprintID != "" {
		sc.reset("")
	}
	if sc.lobby == nil {
		sc.lobby = &actor{name: "Director", role: RoleDirector, col: dye("crimson"), hat: "fedora", pos: loungeSpot, home: loungeSpot, landed: true, yaw: -math.Pi / 2, face: -math.Pi / 2}
		sc.actors = append(sc.actors, sc.lobby)
		sc.byName[sc.lobby.name] = sc.lobby
	}
}

// Advances everything on the floor by dt seconds
func (sc *scene) update(dt float64) {
	sc.t += dt
	if sc.lobby != nil && !sc.lobby.busy() && sc.rand() < dt*0.12 {
		spots := []Vec3{loungeSpot, directorSpot, Vec3{columnsX[0], 0, columnZ + 2.3}, Vec3{columnsX[2], 0, columnZ + 2.3}, Vec3{3.5, 0, 20}, architectAt}
		sc.lobby.walkTo(spots[int(sc.rand()*float64(len(spots)))%len(spots)])
	}
	for _, a := range sc.actors {
		a.update(dt, sc)
		sc.emitFor(a, dt)
	}
	for _, c := range sc.crates {
		c.update(dt, sc)
	}
	kept := sc.beams[:0]
	for i := range sc.beams {
		b := &sc.beams[i]
		b.age += dt
		if b.age < b.life {
			kept = append(kept, *b)
		}
	}
	sc.beams = kept
	alive := sc.parts[:0]
	for i := range sc.parts {
		p := &sc.parts[i]
		p.age += dt
		if p.age >= p.life {
			continue
		}
		p.vel.Y -= p.weight * dt
		p.vel = p.vel.Mul(1 - min(dt*1.5, 1))
		p.pos = p.pos.Add(p.vel.Mul(dt))
		if p.pos.Y < 0.05 && p.weight > 0 {
			p.pos.Y = 0.05
			p.vel = Vec3{}
		}
		alive = append(alive, *p)
	}
	sc.parts = alive
	sc.dust += dt
	if sc.dust > 0.4 && len(sc.parts) < maxParticles-40 {
		sc.dust = 0
		sc.add(particle{pos: Vec3{sc.rand() * floorW, 0.5 + sc.rand()*6, sc.rand() * floorD}, vel: Vec3{sc.spread() * 0.2, 0.15, sc.spread() * 0.2}, life: 6, col: hexRGB(0x9cc7e8), size: 0.12, additive: true})
	}
	if sc.ended && sc.t-sc.endedAt < 12 && sc.rand() < dt*25 {
		cols := []RGB{colCrateDone, colCrateWork, colCrateRev, hexRGB(0xf07aa6), colPaper}
		sc.add(particle{pos: plazaAt.Add(Vec3{sc.spread() * 7, 11 + sc.rand()*3, sc.spread() * 5}), vel: Vec3{sc.spread() * 2, -1, sc.spread() * 2}, life: 5, col: cols[int(sc.rand()*5)%5], size: 0.3, weight: 2.5})
	}
	if sc.ended && sc.t-sc.endedAt > cheerTime {
		for _, a := range sc.actors {
			if a.rest == stateCheer {
				a.rest = stateIdle
			}
		}
	}
}

// Adds a mote while there is room for it
func (sc *scene) add(p particle) {
	if len(sc.parts) < maxParticles {
		sc.parts = append(sc.parts, p)
	}
}

// Sprays motes from a point in every direction, fading over about a second
func (sc *scene) burst(at Vec3, n int, col RGB, speed float64) {
	for range n {
		v := Vec3{sc.spread(), sc.rand() + 0.4, sc.spread()}.Norm().Mul(3 * speed * (0.6 + sc.rand()))
		sc.add(particle{pos: at, vel: v, life: 0.8 + sc.rand()*0.6, col: col, size: 0.22, weight: 4, additive: true})
	}
}

// Low puff of dust where something landed
func (sc *scene) puff(at Vec3, n int, col RGB) {
	for range n {
		v := Vec3{sc.spread(), 0.3, sc.spread()}.Mul(1.8)
		sc.add(particle{pos: at.Add(Vec3{0, 0.1, 0}), vel: v, life: 0.5 + sc.rand()*0.3, col: col, size: 0.25, weight: 1})
	}
}

// Motes an actor gives off in its state, thoughts while thinking and sparks while working
func (sc *scene) emitFor(a *actor, dt float64) {
	switch a.state {
	case stateThink:
		if sc.rand() < dt*2.5 {
			sc.add(particle{pos: a.headPos().Add(Vec3{0.3 + sc.rand()*0.3, 0.3, 0}), vel: Vec3{0.1, 0.9, 0}, life: 1.6, col: colPaper, size: 0.16 + sc.rand()*0.14, additive: true})
		}
	case stateWork:
		if sc.rand() < dt*9 {
			at := a.pos.Add(a.facing().Mul(1.6)).Add(Vec3{sc.spread() * 0.6, 2.4 + sc.rand(), 0})
			sc.add(particle{pos: at, vel: Vec3{0, 1.2 + sc.rand(), 0}, life: 0.7, col: toolColor(a.tool), size: 0.12, additive: true})
		}
	case stateCheer:
		if sc.rand() < dt*3 {
			sc.add(particle{pos: a.headPos().Add(Vec3{sc.spread(), 0.8, sc.spread()}), vel: Vec3{0, 2, 0}, life: 0.9, col: colCrateDone, size: 0.18, additive: true})
		}
	}
}

// Color of the sky at a share of the way down the frame
func (sc *scene) sky(share float64) RGB {
	return sc.zenith.Mix(sc.horizon, math.Pow(min(max(share, 0), 1), 1.6))
}

// Draws the floor, the team, the crates, the glows and the motes through the raster
func (sc *scene) emit(r *Raster) {
	for i := range sc.static {
		r.Quad(sc.static[i])
	}
	dyn := sc.dyn[:0]
	for _, a := range sc.actors {
		dyn = a.quads(sc.t, dyn)
	}
	for _, c := range sc.crates {
		dyn = c.quads(dyn)
	}
	dyn = sc.screens(dyn)
	for i := range dyn {
		r.Quad(dyn[i])
	}
	sc.dyn = dyn
	sc.decals(r)
	sc.glows(r)
	for i := range sc.parts {
		p := &sc.parts[i]
		fade := 1 - p.age/p.life
		r.Billboard(p.pos, p.size*(0.7+0.3*fade), p.col, fade*0.9, p.additive)
	}
}

// Monitor screens lit by the tool their builder runs, and the lights of the server rack
func (sc *scene) screens(out []Quad) []Quad {
	for i := range sc.builders {
		d := deskAt(i)
		col := colScreenOff
		light := 1.0
		for _, a := range sc.actors {
			if a.role == RoleBuilder && a.index == i && (a.state == stateWork || a.state == stateThink) && a.pos.Sub(builderSpot(i)).Len() < 0.5 {
				col = toolColor(a.tool).Scale(0.55 + 0.1*math.Sin(sc.t*9+float64(i)))
				if a.state == stateThink {
					col = col.Scale(0.6)
				}
				light = 1.6
			}
		}
		out = append(out, Quad{P: [4]Vec3{{d.X - 0.85, 3.8, d.Z + 0.02}, {d.X + 0.85, 3.8, d.Z + 0.02}, {d.X + 0.85, 2.2, d.Z + 0.02}, {d.X - 0.85, 2.2, d.Z + 0.02}}, Color: col, Light: [4]float64{light, light, light, light}, Alpha: 1})
	}
	rx := floorW - 2
	for k := range 5 {
		for j := range 4 {
			on := hash2(k*7+int(sc.t*3), j) > 0.45
			col := hexRGB(0x1f3a2a)
			if on {
				col = colCrateDone
				if (k+j)%3 == 0 {
					col = colCrateWork
				}
			}
			y := 0.6 + float64(j)*0.8
			z := 19.3 + float64(k)*1.0
			out = append(out, Quad{P: [4]Vec3{{rx - 0.02, y + 0.2, z}, {rx - 0.02, y + 0.2, z + 0.4}, {rx - 0.02, y, z + 0.4}, {rx - 0.02, y, z}}, Color: col, Light: [4]float64{1.5, 1.5, 1.5, 1.5}, Alpha: 1})
		}
	}
	return out
}

// Soft shadows under the team and the crates
func (sc *scene) decals(r *Raster) {
	shadow := func(at Vec3, w, d float64, alpha float64) {
		q := Quad{Color: RGB{}, Light: [4]float64{1, 1, 1, 1}, Alpha: alpha, TwoSided: true}
		q.P = [4]Vec3{{at.X - w, 0.02, at.Z - d}, {at.X - w, 0.02, at.Z + d}, {at.X + w, 0.02, at.Z + d}, {at.X + w, 0.02, at.Z - d}}
		r.Quad(q)
	}
	for _, a := range sc.actors {
		h := a.drop + max(a.bob, 0)
		shadow(a.pos, 0.95/(1+h*0.3), 0.6/(1+h*0.3), 0.38/(1+h*0.5))
	}
	for _, c := range sc.crates {
		if c.carrier != nil || c.flying || c.drop < 1 {
			shadow(Vec3{c.pos.X, 0, c.pos.Z}, 0.55, 0.55, 0.3/(1+c.pos.Y*0.2))
		}
	}
}

// Lamp light, emotes and beams as additive billboards
func (sc *scene) glows(r *Raster) {
	flicker := 0.85 + 0.15*math.Sin(sc.t*7)*math.Sin(sc.t*3.1)
	r.Billboard(lampAt.Add(Vec3{0.5, 5.6, 0.5}), 2.4, colLamp, 0.35*flicker, true)
	r.Billboard(lampAt.Add(Vec3{0.5, 5.6, 0.5}), 0.9, colLamp, 0.9, true)
	for _, a := range sc.actors {
		switch {
		case a.emote == "?":
			r.Billboard(a.headPos().Add(Vec3{0, 1.6 + 0.15*math.Sin(sc.t*4), 0}), 1.2, colCrateRev, 0.35+0.2*math.Sin(sc.t*5), true)
		case a.emote == "!":
			r.Billboard(a.headPos().Add(Vec3{0, 1.6, 0}), 1.3, colCrateWork, 0.4+0.3*math.Sin(sc.t*8), true)
		case a.emote == "✘":
			r.Billboard(a.headPos().Add(Vec3{0, 1.5, 0}), 1.2, colCrateFail, 0.5, true)
		}
	}
	for i := range sc.beams {
		b := &sc.beams[i]
		from, to := b.from.headPos().Sub(Vec3{0, 1.2, 0}), b.to.headPos().Sub(Vec3{0, 1.2, 0})
		fade := 1 - b.age/b.life
		for k := range 9 {
			s := math.Mod(b.age*1.3+float64(k)/9, 1)
			p := from.Lerp(to, s)
			p.Y += math.Sin(s*math.Pi) * 1.5
			r.Billboard(p, 0.3+0.2*math.Sin(s*math.Pi), b.col, fade*0.9, true)
		}
	}
}

// Ring on the floor around a selected or hovered actor
func (sc *scene) ring(r *Raster, a *actor, col RGB, alpha float64) {
	if a == nil {
		return
	}
	rad := 1.6 + 0.08*math.Sin(sc.t*4)
	for k := range 4 {
		ang := sc.t*1.2 + float64(k)*math.Pi/2
		s, c := math.Sincos(ang)
		center := a.pos.Add(Vec3{rad * c, 0.03, rad * s})
		q := Quad{Color: col, Light: [4]float64{1, 1, 1, 1}, Alpha: alpha, Additive: true, TwoSided: true}
		dx, dz := -s*0.9, c*0.9
		nx, nz := c*0.12, s*0.12
		q.P = [4]Vec3{center.Add(Vec3{-dx - nx, 0, -dz - nz}), center.Add(Vec3{-dx + nx, 0, -dz + nz}), center.Add(Vec3{dx + nx, 0, dz + nz}), center.Add(Vec3{dx - nx, 0, dz - nz})}
		r.Quad(q)
	}
}
