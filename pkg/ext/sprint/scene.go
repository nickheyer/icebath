package sprint

import (
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Class of tool a member runs, each shown its own way on the platform
type toolKind int

const (
	kindNone toolKind = iota
	kindRead
	kindEdit
	kindShell
	kindFetch
	kindAgent
	kindCall
	kindOther
)

// Class of a tool by name
func classify(name string) toolKind {
	switch {
	case name == "":
		return kindNone
	case strings.HasPrefix(name, "Sprint"):
		return kindCall
	case strings.HasPrefix(name, "mcp__"):
		return kindOther
	}
	switch name {
	case "Read", "Glob", "Grep", "Outline":
		return kindRead
	case "Edit", "Write":
		return kindEdit
	case "Shell", "ShellOutput", "KillShell":
		return kindShell
	case "WebFetch":
		return kindFetch
	case "Agent":
		return kindAgent
	}
	return kindOther
}

// Color a class of tool paints its sparks with
func (k toolKind) color() vxl.RGB {
	switch k {
	case kindRead:
		return vxl.Hex(0x5ab4f0)
	case kindEdit:
		return vxl.Hex(0xf0b429)
	case kindShell:
		return vxl.Hex(0x7fe0b8)
	case kindFetch:
		return vxl.Hex(0x9b6bff)
	case kindAgent:
		return vxl.Hex(0xff7f66)
	case kindCall:
		return colPaper
	}
	return vxl.Hex(0x9cc7e8)
}

// Tool calls a member has started, by class
type toolTally struct {
	reads, edits, shells, fetches, agents, calls, others int
}

// Records one tool start
func (t *toolTally) add(name string) {
	switch classify(name) {
	case kindRead:
		t.reads++
	case kindEdit:
		t.edits++
	case kindShell:
		t.shells++
	case kindFetch:
		t.fetches++
	case kindAgent:
		t.agents++
	case kindCall:
		t.calls++
	case kindOther:
		t.others++
	}
}

// Count of every tool start
func (t toolTally) total() int {
	return t.reads + t.edits + t.shells + t.fetches + t.agents + t.calls + t.others
}

// What the harness says a member is doing right now, with what it has spent
type liveState struct {
	running  bool
	tool     string
	toolAt   time.Time
	approval bool
	asking   bool
	failed   bool
	tally    toolTally
	// Share of the token budget spent, of a nominal budget when none is set
	heat float64
	// Moment the current exchange began and how long it may take
	since time.Time
	limit time.Duration
}

// Most motes alive at once
const maxParticles = 900

// How the sprint ended, shaping the finale
type endMode int

const (
	endCheer endMode = iota
	endStop
	endFail
)

// Piece of the broken net drifting around the platform
type shard struct {
	ang, rad, h, size, spin, speed float64
}

// Scan line running across the face of the blackwall
type scanLine struct {
	y, x0, x1, speed float64
}

// The platform with everyone and everything on it, advanced frame by frame from the state of a sprint
type scene struct {
	lay      dive
	grid     vxl.Grid
	gridLo   vxl.Vec3
	gridHi   vxl.Vec3
	chunks   []vxl.Chunk
	dirty    bool
	builders int
	planKey  string
	nodes    map[string]*node
	order    []string
	actors   []*actor
	byName   map[string]*actor
	packets  []packet
	parts    vxl.Particles
	debris   []shard
	lights   []vxl.Light
	t        float64
	stage    Stage
	ended    bool
	endedAt  float64
	endMode  endMode
	queue    []*actor
	lastSeq  int
	rnd      vxl.RNG
	sprintID string
	// Sky at the top and at the horizon
	zenith  vxl.RGB
	horizon vxl.RGB
	lobby   *actor
	// Where the search beam of the spire points, eased, and the node it locks on
	beamAt, beamWant vxl.Vec3
	beamLock         string
	beamT            float64
	wallSurge        float64
	waiting          bool
	paused           bool
	scans            [7]scanLine
	stageFlash       float64
	stageText        string
	dust             float64
	lastStep         float64
	// Sky ramp for the zenith and horizon it was sampled from
	skyRamp atomic.Pointer[skyRamp]
}

// Steps the sky ramp samples between the zenith and the horizon
const skySteps = 512

// Sky colors from the zenith down to the horizon at even steps, for one pair of end colors
type skyRamp struct {
	zenith, horizon vxl.RGB
	at              [skySteps + 1]vxl.RGB
}

// Platform built for a team of that many builders, without a plan
func newScene(builders int) *scene {
	sc := &scene{byName: map[string]*actor{}, nodes: map[string]*node{}, rnd: vxl.RNG{Seed: 0x1234567}, zenith: vxl.Hex(0x05070a), horizon: vxl.Hex(0x101a2a)}
	sc.parts.Max = maxParticles
	sc.rebuild(nil, builders)
	for i := range 26 {
		sc.debris = append(sc.debris, shard{ang: sc.rnd.Next() * 2 * math.Pi, rad: 0.55 + sc.rnd.Next()*0.5, h: -8 + sc.rnd.Next()*18, size: 0.4 + sc.rnd.Next()*1.1, spin: sc.rnd.Next() * 6, speed: (0.02 + sc.rnd.Next()*0.05) * float64(1-2*(i%2))})
	}
	for i := range sc.scans {
		sc.scans[i] = scanLine{y: sc.rnd.Next() * 16, speed: 0.6 + sc.rnd.Next()*1.6}
		sc.rescan(i)
	}
	return sc
}

// Picks a new span across the wall for a scan line
func (sc *scene) rescan(i int) {
	w := float64(sc.lay.w + 16)
	x0 := -8 + sc.rnd.Next()*w*0.7
	sc.scans[i].x0, sc.scans[i].x1 = x0, x0+3+sc.rnd.Next()*w*0.3
}

// Builds the platform again for a plan and a count of builders, nodes kept where their ids match
func (sc *scene) rebuild(plan []planNode, builders int) {
	sc.builders = max(builders, 1)
	sc.lay = buildDive(plan, sc.builders)
	sc.grid = sc.lay.build()
	sc.gridLo, sc.gridHi, _ = sc.grid.Bounds()
	sc.dirty = true
	seen := map[string]bool{}
	for i, p := range plan {
		seen[p.id] = true
		n, ok := sc.nodes[p.id]
		if !ok {
			n = &node{id: p.id, status: TicketTodo, shell: 1, seed: sc.rnd.Next(), riseDelay: 0.3 + float64(sc.lay.levels[p.id])*0.5 + float64(i)*0.12}
			sc.nodes[p.id] = n
			sc.order = append(sc.order, p.id)
		}
		n.pos, n.level, n.deps = sc.lay.nodes[p.id], sc.lay.levels[p.id], sc.lay.deps[p.id]
	}
	for id := range sc.nodes {
		if !seen[id] {
			delete(sc.nodes, id)
		}
	}
	kept := sc.order[:0]
	for _, id := range sc.order {
		if seen[id] {
			kept = append(kept, id)
		}
	}
	sc.order = kept
	for _, a := range sc.actors {
		if a.pos.Y < 0.5 && !sc.lay.onPlatform(a.pos) {
			a.pos = sc.lay.nearest(a.pos)
		}
		if a.landed && !a.gone {
			sc.assign(a, a.duty)
		}
	}
}

// Signature of a plan, so the platform is only rebuilt when the tickets or their order change
func planSignature(tickets []Ticket) (string, []planNode) {
	var sb strings.Builder
	plan := make([]planNode, 0, len(tickets))
	for _, t := range tickets {
		sb.WriteString(t.ID)
		sb.WriteByte('<')
		sb.WriteString(strings.Join(t.DependsOn, ","))
		sb.WriteByte(';')
		plan = append(plan, planNode{id: t.ID, deps: t.DependsOn})
	}
	return sb.String(), plan
}

// Brings the platform in line with a sprint, nil for none, and what the harness says each member is doing
func (sc *scene) sync(snap *Snapshot, live map[string]liveState, cfg Config) {
	if snap == nil {
		sc.syncLobby(cfg)
		return
	}
	if sc.sprintID != snap.ID {
		sc.reset(snap.ID)
	}
	sc.lobby = nil
	key, plan := planSignature(snap.Tickets)
	builders := max(countRole(snap.Members, RoleBuilder), cfg.Team.Builders)
	if key != sc.planKey || builders > sc.builders {
		sc.planKey = key
		sc.rebuild(plan, builders)
	}
	for _, m := range snap.Members {
		a, ok := sc.byName[m.Name]
		if !ok {
			a = sc.spawn(m, cfg)
		}
		look := cfg.LookOf(m.Role, m.Index)
		if dye(look.Color) != a.col || look.Hat != a.rig {
			a.dress(look)
		}
	}
	sc.syncNodes(snap, live)
	sc.waiting, sc.paused = snap.Waiting != "", snap.Paused != ""
	sc.syncEnd(snap)
	for _, m := range snap.Members {
		sc.direct(snap, m, live[m.Name])
	}
	sc.syncMessages(snap)
	if sc.stage != snap.Stage {
		sc.stage = snap.Stage
		sc.stageFlash, sc.stageText = 3, strings.ToUpper(string(snap.Stage))
	}
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

// Clears the platform for another sprint
func (sc *scene) reset(id string) {
	sc.sprintID = id
	sc.actors = sc.actors[:0]
	sc.byName = map[string]*actor{}
	sc.nodes = map[string]*node{}
	sc.order = nil
	sc.planKey = ""
	sc.packets = sc.packets[:0]
	sc.parts.Clear()
	sc.lastSeq = 0
	sc.ended, sc.endedAt = false, 0
	sc.queue = nil
	sc.beamLock = ""
	sc.stage = ""
	sc.rebuild(nil, sc.builders)
}

// Drops a new member onto the platform at its station
func (sc *scene) spawn(m Member, cfg Config) *actor {
	a := newActor(m, cfg.LookOf(m.Role, m.Index))
	a.pos, a.face = sc.station(m.Role, m.Index)
	a.yaw = a.face
	a.phase = sc.rnd.Next() * 6
	a.duty = duty{kind: dutyIdle, at: a.pos, face: a.face}
	sc.actors = append(sc.actors, a)
	sc.byName[m.Name] = a
	return a
}

// Home spot of a role and the way it faces there
func (sc *scene) station(role Role, index int) (vxl.Vec3, float64) {
	switch role {
	case RoleDirector:
		return sc.lay.uplink, 0
	case RoleArchitect:
		return sc.lay.nearest(sc.lay.chart.Add(vxl.V(0, 0, -2.5))), 0
	case RoleReviewer:
		return sc.lay.spireTop(), 0
	}
	return sc.lay.cradle(index), 0
}

// Keeps every node at the status of its ticket, each edit cracking its shell and each read sweeping it
func (sc *scene) syncNodes(snap *Snapshot, live map[string]liveState) {
	for _, t := range snap.Tickets {
		n := sc.nodes[t.ID]
		if n == nil {
			continue
		}
		if n.status != t.Status {
			if t.Status == TicketDone {
				sc.shards(n.core(sc.t), 26)
				sc.burst(n.core(sc.t), 30, colNodeDone, 1.2)
			}
			if t.Status == TicketFailed {
				sc.glitch(n.core(sc.t), 24)
				sc.wallSurge = max(sc.wallSurge, 0.6)
			}
			n.status, n.glow = t.Status, 1
			if t.Status == TicketTodo {
				n.cracks = 0
			}
		}
		n.round = t.Round
		if t.Status == TicketActive && t.Builder != "" {
			st := live[t.Builder]
			if st.tally.edits > n.edits {
				n.cracks = min(n.cracks+0.08*float64(st.tally.edits-n.edits), 1)
				sc.shards(n.core(sc.t), 2)
			}
			if st.tally.reads > n.reads {
				n.scan = 1.2
			}
			n.edits, n.reads = st.tally.edits, st.tally.reads
		}
	}
}

// Sets what a member does and where it stands from the sprint and the harness
func (sc *scene) direct(snap *Snapshot, m Member, st liveState) {
	a := sc.byName[m.Name]
	if a == nil || sc.ended {
		return
	}
	recent := st.tool != "" && time.Since(st.toolAt) < 4*time.Second
	d := duty{kind: dutyIdle, at: a.pos, face: a.face}
	switch m.Role {
	case RoleDirector:
		if snap.Stage == StageBrief || snap.Stage == StageReport || st.running {
			d = duty{kind: dutySpot, at: sc.lay.uplink, face: 0, rest: stateThink}
			if st.running {
				d.rest = stateBrief
			}
			if st.asking {
				d.rest = stateWait
			}
		}
	case RoleArchitect:
		if snap.Stage == StagePlan || st.running {
			stand, face := sc.station(RoleArchitect, 0)
			d = duty{kind: dutySpot, at: stand, face: face, rest: stateThink}
			if st.running {
				d.rest = stateChart
			}
		}
	case RoleReviewer:
		d = duty{kind: dutySpire, at: sc.lay.spireTop(), face: 0, rest: stateIdle}
		if strings.HasPrefix(m.Task, "reviewing") || st.running {
			d.rest = stateReview
		}
	case RoleBuilder:
		t, ok := snap.Ticket(m.Ticket)
		switch {
		case ok && t.Status == TicketReview:
			at, face := sc.lay.jackSpot(m.Ticket)
			d = duty{kind: dutyWaitNode, node: m.Ticket, at: at, face: face, rest: stateWait}
		case ok && t.Status == TicketActive:
			at, face := sc.lay.jackSpot(m.Ticket)
			d = duty{kind: dutyNode, node: m.Ticket, at: at, face: face, rest: stateThink}
			if st.running && recent {
				d.rest = stateJack
			}
		case st.running:
			d = duty{kind: dutySpot, at: sc.lay.cradle(m.Index), face: 0, rest: stateThink}
		}
	}
	if st.failed && !st.running {
		d.rest = stateSlump
	}
	if st.tool != "" {
		a.toolKind = classify(st.tool)
		if recent {
			a.drone = 1
		}
	}
	a.heat = st.heat
	if st.limit > 0 && !st.since.IsZero() && st.running {
		a.clock = time.Since(st.since).Seconds() / st.limit.Seconds()
	} else {
		a.clock = 0
	}
	a.frozen += (flagF(st.approval) - a.frozen) * min(sc.lastStep*3, 1)
	if st.approval && a.emote == "" {
		a.emote, a.emoteT = "!", 0.6
	}
	a.beacon += (flagF(st.asking) - a.beacon) * min(sc.lastStep*2, 1)
	if !d.same(a.duty) {
		sc.assign(a, d)
	} else {
		a.rest = d.rest
	}
}

// Shows every exchange since the last sync, packets flying between the members and the crew reacting
func (sc *scene) syncMessages(snap *Snapshot) {
	for _, m := range snap.MessagesAfter(sc.lastSeq) {
		sc.lastSeq = m.Seq
		from, to := sc.byName[m.From], sc.byName[m.To]
		switch m.Kind {
		case MsgAssign:
			if from != nil && to != nil {
				target := to
				sc.sendPacket(from.headPos(), to.handsPos(), colNodeWork, func(sc *scene) { target.nod = 1 })
			}
		case MsgHandoff:
			if from != nil {
				dest := sc.lay.spireTop().Add(vxl.V(0, 4, 0))
				if to != nil {
					dest = to.headPos()
				}
				sc.sendPacket(from.handsPos(), dest, colNodeRev, nil)
				from.nod = 1
			}
		case MsgVerdict:
			if to == nil {
				continue
			}
			col, verdict := colGreen, VerdictAccepted
			switch {
			case strings.HasPrefix(m.Text, string(VerdictRework)):
				col, verdict = colNodeWork, VerdictRework
			case strings.HasPrefix(m.Text, string(VerdictFailed)):
				col, verdict = colNodeFail, VerdictFailed
			case strings.HasPrefix(m.Text, string(VerdictGuidance)):
				col, verdict = colIce, VerdictGuidance
			}
			src := sc.lay.spireTop().Add(vxl.V(0, 4, 0))
			if from != nil {
				src = from.headPos()
			}
			target := to
			sc.sendPacket(src, to.handsPos(), col, func(sc *scene) { sc.verdictLanded(target, verdict) })
		case MsgQuestion:
			if from != nil {
				from.emote, from.emoteT = "?", 6
			}
		case MsgAnswer:
			if to != nil {
				to.nod = 1
				to.emote = ""
			}
		case MsgFailure:
			if from != nil {
				sc.glitch(from.headPos(), 18)
				from.emote, from.emoteT = "✘", 4
				sc.interject(from, stateStagger, 2)
				sc.wallSurge = max(sc.wallSurge, 0.5)
				for _, b := range sc.bystanders(from.pos, 12) {
					if b != from {
						b.face = vxl.HeadingOf(from.pos.Sub(b.pos))
					}
				}
			}
		}
	}
}

// Crew reacting to a verdict once its packet reaches the builder
func (sc *scene) verdictLanded(a *actor, v Verdict) {
	switch v {
	case VerdictAccepted:
		a.emote, a.emoteT = "✓", 3
		sc.interject(a, stateCheer, 2.4)
		for _, b := range sc.bystanders(a.pos, 14) {
			if b != a {
				sc.interject(b, stateCheer, 1.2)
			}
		}
	case VerdictRework:
		a.shake = 1.5
		sc.interject(a, stateSlump, 1.4)
	case VerdictFailed:
		a.emote, a.emoteT = "✘", 4
		sc.interject(a, stateStagger, 2.2)
	default:
		a.nod = 1
	}
}

// Gathers the crew at the uplink once the sprint ends, jacking out when it is done or failed
func (sc *scene) syncEnd(snap *Snapshot) {
	if snap.Running() {
		if sc.ended {
			sc.ended = false
			sc.queue = nil
			for _, a := range sc.actors {
				if a.gone {
					a.gone, a.exit, a.lift, a.landed, a.drop = false, 0, 0, false, 14
					a.pos, a.face = sc.station(a.role, a.index)
				}
				a.steps = a.steps[:0]
				a.duty = duty{}
			}
		}
		return
	}
	if sc.ended {
		return
	}
	sc.ended, sc.endedAt = true, sc.t
	switch {
	case snap.Err == nil:
		sc.endMode = endCheer
	case snap.Stopped():
		sc.endMode = endStop
	default:
		sc.endMode = endFail
		sc.wallSurge = 1
	}
	n := len(sc.actors)
	for i, a := range sc.actors {
		sc.unpair(a)
		a.steps = a.steps[:0]
		a.frozen, a.beacon = 0, 0
		spot := sc.lay.nearest(sc.lay.uplink.Add(vxl.V(float64(i)*1.6-float64(n-1)*0.8, 0, 3.6)))
		a.duty = duty{kind: dutyGather, at: spot, face: math.Pi, rest: stateIdle}
		a.rest = stateIdle
		if a.pos.Y > 0.5 {
			a.steps = append(a.steps, step{kind: stepLeap, to: sc.lay.spireFoot()})
		}
		switch sc.endMode {
		case endCheer:
			a.steps = append(a.steps, step{kind: stepHold, state: stateCheer, dur: 3 + sc.rnd.Next()*1.5})
		case endFail:
			a.steps = append(a.steps, step{kind: stepHold, state: stateStagger, dur: 1.5}, step{kind: stepHold, state: stateSlump, dur: 5 + sc.rnd.Next()*3})
		}
		a.steps = append(a.steps, step{kind: stepFace, yaw: math.Pi})
		if sc.endMode != endStop {
			sc.queue = append(sc.queue, a)
		}
	}
}

// Sends the next of the gathered crew onto the pad to jack out once the one before is gone
func (sc *scene) updateQueue() {
	if len(sc.queue) == 0 || !sc.ended {
		return
	}
	head := sc.queue[0]
	switch {
	case head.gone:
		sc.queue = sc.queue[1:]
		return
	case head.exit > 0 || head.busy() || !head.landed:
		return
	}
	for _, a := range sc.actors {
		if a != head && !a.gone && a.landed && !a.busy() && a.exit == 0 && a.pos.Flat(a.duty.at) > 1.5 && a.duty.kind == dutyGather {
			sc.send(a, a.duty.at)
			a.steps = append(a.steps, step{kind: stepFace, yaw: math.Pi})
		}
	}
	if head.pos.Flat(head.duty.at) > 1.5 && head.pos.Flat(sc.lay.uplink) > 1 {
		sc.send(head, head.duty.at)
		return
	}
	head.steps = append(sc.walkSteps(head, sc.lay.uplink), step{kind: stepFace, yaw: 0}, step{kind: stepHold, state: stateIdle, dur: 0.6}, step{kind: stepExit})
}

// Keeps a lone fixer pacing the landing while no sprint runs
func (sc *scene) syncLobby(cfg Config) {
	if sc.sprintID != "" {
		sc.reset("")
	}
	if sc.lobby == nil {
		look := cfg.LookOf(RoleDirector, 0)
		a := newActor(Member{Role: RoleDirector, Name: cfg.NameOf(RoleDirector, 0)}, look)
		a.pos, a.face = sc.station(RoleDirector, 0)
		a.yaw, a.landed, a.drop = a.face, true, 0
		a.duty = duty{kind: dutyIdle, at: a.pos, face: a.face}
		sc.lobby = a
		sc.actors = append(sc.actors, a)
		sc.byName[a.name] = a
	}
}

// Advances everything on the platform by dt seconds
func (sc *scene) update(dt float64) {
	sc.t += dt
	sc.lastStep = dt
	for _, a := range sc.actors {
		a.update(dt, sc)
		sc.ambient(a, dt)
		sc.emitState(a, dt)
	}
	sc.separate(dt)
	for _, n := range sc.nodes {
		n.update(dt, sc)
	}
	sc.updatePackets(dt)
	sc.updateQueue()
	sc.parts.Update(dt)
	for i := range sc.debris {
		d := &sc.debris[i]
		d.ang += d.speed * dt
		d.spin += dt * 0.3
	}
	for i := range sc.scans {
		s := &sc.scans[i]
		s.y += dt * s.speed * (1 + 3*sc.wallSurge)
		if s.y > 17 {
			s.y = -4
			sc.rescan(i)
		}
	}
	sc.wallSurge = max(sc.wallSurge-dt*0.08, 0)
	sc.stageFlash = max(sc.stageFlash-dt, 0)
	sc.updateBeam(dt)
	sc.dust += dt
	if sc.dust > 0.25 && sc.parts.Room(40) {
		sc.dust = 0
		sc.parts.Add(vxl.Particle{Pos: vxl.V(sc.rnd.Next()*float64(sc.lay.w), 0.3+sc.rnd.Next()*7, sc.rnd.Next()*float64(sc.lay.d)), Vel: vxl.V(sc.rnd.Spread()*0.15, 0.2, sc.rnd.Spread()*0.15), Life: 7, Col: colIce, Size: 0.1, Additive: true})
	}
	if sc.ended && sc.endMode == endCheer && sc.t-sc.endedAt < 10 && sc.rnd.Chance(30, dt) {
		cols := []vxl.RGB{colGreen, colIce, colPaper, colNodeWork}
		sc.parts.Add(vxl.Particle{Pos: sc.lay.uplink.Add(vxl.V(sc.rnd.Spread()*12, 12+sc.rnd.Next()*3, sc.rnd.Spread()*5)), Vel: vxl.V(sc.rnd.Spread()*2, -1, sc.rnd.Spread()*2), Life: 5, Col: cols[sc.rnd.Intn(4)], Size: 0.3, Weight: 2, Floor: 0.1})
	}
	if sc.ended && sc.endMode == endFail && sc.t-sc.endedAt < 12 && sc.rnd.Chance(40, dt) {
		sc.parts.Add(vxl.Particle{Pos: vxl.V(sc.rnd.Next()*float64(sc.lay.w), 14, sc.rnd.Next()*float64(sc.lay.d)), Vel: vxl.V(0, -9, 0), Life: 1.6, Col: colNodeFail, Size: 0.14, Additive: true})
	}
	if sc.wallSurge > 0.3 && sc.rnd.Chance(sc.wallSurge*25, dt) {
		at := vxl.V(-8+sc.rnd.Next()*float64(sc.lay.w+16), sc.rnd.Next()*12, float64(sc.lay.wallZ)+1)
		sc.parts.Add(vxl.Particle{Pos: at, Vel: vxl.V(sc.rnd.Spread()*3, sc.rnd.Next()*3, 2+sc.rnd.Next()*4), Life: 1.2, Col: colRed, Size: 0.22, Weight: 3, Additive: true})
	}
}

// Points the search beam at the node under review, else sweeps it over the platform now and then
func (sc *scene) updateBeam(dt float64) {
	lock := ""
	for _, a := range sc.actors {
		if a.role == RoleReviewer && a.state == stateReview {
			for _, n := range sc.nodes {
				if n.status == TicketReview {
					lock = n.id
				}
			}
		}
	}
	if lock != "" {
		sc.beamLock = lock
		sc.beamWant = sc.nodes[lock].pos
		sc.beamT = 0
	} else {
		sc.beamLock = ""
		sc.beamT -= dt
		if sc.beamT <= 0 {
			sc.beamT = 3 + sc.rnd.Next()*4
			if len(sc.order) > 0 && sc.rnd.Next() < 0.6 {
				sc.beamWant = sc.nodes[sc.order[sc.rnd.Intn(len(sc.order))]].pos
			} else {
				sc.beamWant = sc.lay.roam(&sc.rnd, sc.lay.center(), float64(sc.lay.w)/2)
			}
		}
	}
	sc.beamAt = sc.beamAt.Lerp(sc.beamWant, min(dt*2.5, 1))
}

// Motes an actor gives off in its state, sparks of its tool while jacked in and thoughts while thinking
func (sc *scene) emitState(a *actor, dt float64) {
	if a.gone || !a.landed {
		return
	}
	switch a.state {
	case stateThink, stateChart:
		if sc.rnd.Chance(2.5, dt) {
			sc.parts.Add(vxl.Particle{Pos: a.headPos().Add(vxl.V(0.3+sc.rnd.Next()*0.3, 0.3, 0)), Vel: vxl.V(0.1, 0.9, 0), Life: 1.6, Col: colPaper, Size: 0.16 + sc.rnd.Next()*0.14, Additive: true})
		}
	case stateJack:
		rate := 4 + 5*min(a.jacked/12, 1) + 10*a.heat
		if sc.rnd.Chance(rate, dt) {
			at := a.handsPos().Add(vxl.V(sc.rnd.Spread()*0.5, sc.rnd.Next()*0.6, sc.rnd.Spread()*0.5))
			sc.parts.Add(vxl.Particle{Pos: at, Vel: vxl.V(0.3*sc.rnd.Spread(), 1.4+sc.rnd.Next(), 0.3*sc.rnd.Spread()), Life: 0.7, Col: a.toolKind.color(), Size: 0.13, Additive: true})
		}
		if a.heat > 0.5 && sc.rnd.Chance((a.heat-0.5)*12, dt) {
			sc.parts.Add(vxl.Particle{Pos: a.headPos().Add(vxl.V(sc.rnd.Spread()*0.5, -0.5, sc.rnd.Spread()*0.5)), Vel: vxl.V(0, 1.8, 0), Life: 1, Col: a.traceColor(), Size: 0.18, Additive: true})
		}
	case stateCheer:
		if sc.rnd.Chance(3, dt) {
			sc.parts.Add(vxl.Particle{Pos: a.headPos().Add(vxl.V(sc.rnd.Spread(), 0.8, sc.rnd.Spread())), Vel: vxl.V(0, 2, 0), Life: 0.9, Col: colGreen, Size: 0.18, Additive: true})
		}
	case stateBrief:
		if sc.rnd.Chance(4, dt) {
			sc.parts.Add(vxl.Particle{Pos: a.headPos().Add(vxl.V(sc.rnd.Spread()*0.4, 0.6, sc.rnd.Spread()*0.4)), Vel: vxl.V(0, 3.5, 0), Life: 1.8, Col: colIce, Size: 0.12, Additive: true})
		}
	case stateInspect:
		if sc.rnd.Chance(10, dt) {
			sc.parts.Add(vxl.Particle{Pos: a.headPos().Add(a.facing().Mul(1.5 + sc.rnd.Next()*1.5)).Add(vxl.V(0, -0.5+sc.rnd.Spread()*0.4, 0)), Vel: a.facing().Mul(2), Life: 0.5, Col: colIce, Size: 0.1, Additive: true})
		}
	}
	if a.frozen > 0.5 && sc.rnd.Chance(3, dt) {
		sc.parts.Add(vxl.Particle{Pos: a.stand().Add(vxl.V(sc.rnd.Spread()*1.2, sc.rnd.Next()*5, sc.rnd.Spread()*1.2)), Vel: vxl.V(0, -0.3, 0), Life: 1.5, Col: colIce, Size: 0.1, Additive: true})
	}
	if a.drone > 0 && a.toolKind == kindShell && sc.rnd.Chance(12, dt) {
		ang := sc.t * 5
		at := a.headPos().Add(vxl.V(1.3*math.Cos(ang), 0.4, 1.3*math.Sin(ang)))
		sc.parts.Add(vxl.Particle{Pos: at, Vel: vxl.Vec3{}, Life: 0.3, Col: kindShell.color(), Size: 0.2, Additive: true})
	}
}

// One shot effects an errand step asks for
func (sc *scene) emitFor(a *actor, kind string) {
	switch kind {
	case "jack":
		sc.burst(a.handsPos(), 12, colIce, 0.8)
	case "scan":
		sc.burst(a.headPos(), 6, colIce, 0.5)
	}
}

// Sprays motes from a point in every direction, fading over about a second
func (sc *scene) burst(at vxl.Vec3, n int, col vxl.RGB, speed float64) {
	for range n {
		v := vxl.V(sc.rnd.Spread(), sc.rnd.Next()+0.4, sc.rnd.Spread()).Norm().Mul(3 * speed * (0.6 + sc.rnd.Next()))
		sc.parts.Add(vxl.Particle{Pos: at, Vel: v, Life: 0.8 + sc.rnd.Next()*0.6, Col: col, Size: 0.22, Weight: 4, Drag: 1.5, Additive: true})
	}
}

// Low puff of frost where something landed
func (sc *scene) puff(at vxl.Vec3, n int, col vxl.RGB) {
	for range n {
		v := vxl.V(sc.rnd.Spread(), 0.3, sc.rnd.Spread()).Mul(1.8)
		sc.parts.Add(vxl.Particle{Pos: at.Add(vxl.V(0, 0.1, 0)), Vel: v, Life: 0.5 + sc.rnd.Next()*0.3, Col: col, Size: 0.25, Weight: 1, Drag: 1.5, Floor: 0.05})
	}
}

// Shards of ice thrown from a shell, falling to the floor
func (sc *scene) shards(at vxl.Vec3, n int) {
	for range n {
		v := vxl.V(sc.rnd.Spread(), sc.rnd.Next()*1.5+0.5, sc.rnd.Spread()).Mul(4)
		sc.parts.Add(vxl.Particle{Pos: at.Add(vxl.V(sc.rnd.Spread(), sc.rnd.Spread(), sc.rnd.Spread())), Vel: v, Life: 1.2 + sc.rnd.Next(), Col: colIce.Mix(colPaper, sc.rnd.Next()*0.6), Size: 0.3, Weight: 9, Drag: 0.3, Floor: 0.15})
	}
}

// Black ice, red and dark motes swarming a point
func (sc *scene) glitch(at vxl.Vec3, n int) {
	for i := range n {
		col := colNodeFail
		if i%3 == 0 {
			col = colDark
		}
		v := vxl.V(sc.rnd.Spread(), sc.rnd.Spread(), sc.rnd.Spread()).Mul(3)
		sc.parts.Add(vxl.Particle{Pos: at.Add(vxl.V(sc.rnd.Spread()*1.2, sc.rnd.Spread()*1.2, sc.rnd.Spread()*1.2)), Vel: v, Life: 0.5 + sc.rnd.Next()*0.5, Col: col, Size: 0.28, Additive: i%3 != 0})
	}
}

// Color overhead at a share of the way down the frame, safe from several goroutines
func (sc *scene) sky(share float64) vxl.RGB {
	ramp := sc.skyRamp.Load()
	if ramp == nil || ramp.zenith != sc.zenith || ramp.horizon != sc.horizon {
		ramp = &skyRamp{zenith: sc.zenith, horizon: sc.horizon}
		for i := range ramp.at {
			ramp.at[i] = sc.zenith.Mix(sc.horizon, math.Pow(float64(i)/skySteps, 1.6))
		}
		sc.skyRamp.Store(ramp)
	}
	s := min(max(share, 0), 1) * skySteps
	i := min(int(s), skySteps-1)
	return ramp.at[i].Mix(ramp.at[i+1], s-float64(i))
}

// Queues the static platform when any of it is in view, meshing it again when it changed
func (sc *scene) drawStatic(r *vxl.Raster) {
	if sc.dirty {
		r.SetLights(nil)
		sc.chunks = sc.grid.Mesh(r)
		sc.dirty = false
	}
	if !r.Visible(sc.gridLo, sc.gridHi) {
		return
	}
	r.Chunks(sc.chunks)
}

// Every light on the platform this frame
func (sc *scene) collectLights() []vxl.Light {
	out := sc.lights[:0]
	for _, id := range sc.order {
		out = append(out, sc.nodes[id].light(sc.t))
	}
	out = sc.packetLights(out)
	out = append(out, vxl.Light{Pos: sc.lay.uplink.Add(vxl.V(0, 3.2, 0)), Color: colIce.Scale(0.8 + 0.4*flagF(sc.waiting)), Radius: 7})
	out = append(out, vxl.Light{Pos: sc.lay.spireTop().Add(vxl.V(0, 7, 2)), Color: colIce.Scale(0.9), Radius: 9})
	for _, a := range sc.actors {
		if a.gone {
			continue
		}
		if a.beacon > 0.05 {
			out = append(out, vxl.Light{Pos: a.headPos().Add(vxl.V(0, 2, 0)), Color: colIce.Scale(a.beacon * 1.2), Radius: 6})
		}
		if a.state == stateJack {
			out = append(out, vxl.Light{Pos: a.handsPos(), Color: a.toolKind.color().Scale(0.7), Radius: 3.5})
		}
		if a.exit > 0 {
			out = append(out, vxl.Light{Pos: a.stand().Add(vxl.V(0, 2, 0)), Color: colIce.Scale(1.5), Radius: 8})
		}
	}
	beamCol := colIce
	if sc.beamLock != "" {
		beamCol = colNodeRev
	}
	out = append(out, vxl.Light{Pos: sc.beamAt.Add(vxl.V(0, 1.5, 0)), Color: beamCol.Scale(0.6), Radius: 5})
	sc.lights = out
	return out
}

// Queues everything that moves, the solid things first and the glows and veils after them
func (sc *scene) drawDynamic(r *vxl.Raster) {
	r.SetLights(sc.collectLights())
	for _, a := range sc.actors {
		a.draw(r)
	}
	for _, id := range sc.order {
		sc.nodes[id].draw(r, sc)
	}
	sc.drawPackets(r)
	sc.drawDebris(r)
	sc.drawStations(r)
	sc.drawWall(r)
	sc.drawShadows(r)
	for _, id := range sc.order {
		sc.nodes[id].drawGlow(r, sc)
	}
	for _, a := range sc.actors {
		a.drawIce(r, sc.t)
	}
	sc.drawBeam(r)
	sc.drawBeacons(r)
	sc.drawTethers(r)
	sc.drawTalk(r)
	sc.drawClocks(r)
	sc.parts.Draw(r)
}

// Queues the drifting shards of the broken net around the platform
func (sc *scene) drawDebris(r *vxl.Raster) {
	c := sc.lay.center()
	rx, rz := float64(sc.lay.w)/2+6, float64(sc.lay.d)/2+6
	for i := range sc.debris {
		d := &sc.debris[i]
		at := vxl.V(c.X+math.Cos(d.ang)*rx*(1+d.rad*0.6), d.h+math.Sin(sc.t*0.3+d.spin)*0.6, c.Z+math.Sin(d.ang)*rz*(1+d.rad*0.6))
		r.OrientedBox(at, vxl.V(d.size, d.size*0.6, d.size*1.3), d.spin+sc.t*0.1, colCliff.Mix(colIceDeep, 0.15), 1)
	}
}

// Queues the uplink hologram, its column while the sprint waits on the person, and the chart over the table
func (sc *scene) drawStations(r *vxl.Raster) {
	top := sc.lay.spireTop()
	for k := range 4 {
		ang := float64(k)*math.Pi/2 + math.Pi/4
		at := top.Add(vxl.V(1.35*math.Cos(ang), 0.35, 1.35*math.Sin(ang)))
		r.EmissiveBox(at, vxl.V(0.3, 0.7, 0.3), 0, colIce, 1, 1.2+0.3*math.Sin(sc.t*3+float64(k)))
	}
	r.EmissiveBox(top.Add(vxl.V(0, 3, -1)), vxl.V(0.5, 0.5, 0.5), sc.t, colRed, 1, 1.3+0.5*math.Sin(sc.t*2))
	u := sc.lay.uplink.Add(vxl.V(0, 3.2, 0))
	for k := range 8 {
		ang := sc.t*1.1 + float64(k)*math.Pi/4
		at := u.Add(vxl.V(1.3*math.Cos(ang), 0.2*math.Sin(sc.t*2+float64(k)), 1.3*math.Sin(ang)))
		r.EmissiveBox(at, vxl.V(0.3, 0.3, 0.3), ang, colIce, 1, 1.3)
	}
	if sc.waiting || sc.paused {
		col := colIce
		if sc.paused {
			col = colAmber
		}
		for k := range 26 {
			y := 1 + float64(k)*0.9
			pulse := 0.5 + 0.5*math.Sin(sc.t*4-float64(k)*0.3)
			r.Billboard(sc.lay.uplink.Add(vxl.V(0, y, 0)), 2.4+0.6*pulse, col, 0.05+0.06*pulse, true)
		}
	}
	if len(sc.order) > 0 {
		base := sc.lay.chart.Add(vxl.V(0, 2.3, 0))
		w := vxl.Grey(1)
		plane := vxl.Quad{P: vxl.FloorQuad(base, 2.4, 1.6, 0), Color: colIce, Alpha: 0.08, Additive: true, TwoSided: true}
		plane.Light = [4]vxl.RGB{w, w, w, w}
		r.Quad(plane)
		for _, id := range sc.order {
			n := sc.nodes[id]
			if n.rise < 1 {
				continue
			}
			mx := (n.pos.X - float64(sc.lay.w)/2) / float64(sc.lay.w) * 4.4
			mz := (n.pos.Z - float64(sc.lay.fortD)/2) / float64(sc.lay.fortD) * 2.8
			at := base.Add(vxl.V(mx, 0.4+0.1*math.Sin(sc.t*2+n.seed*5), mz))
			r.EmissiveBox(at, vxl.V(0.28, 0.28, 0.28), sc.t*0.5, n.color(), 1, 1.3)
		}
	}
}

// Queues the scan lines crawling over the face of the blackwall
func (sc *scene) drawWall(r *vxl.Raster) {
	z := float64(sc.lay.wallZ) + 1.002
	for _, s := range sc.scans {
		k := 0.5 + 0.5*sc.wallSurge
		q := vxl.Quad{P: [4]vxl.Vec3{vxl.V(s.x0, s.y+0.4, z), vxl.V(s.x1, s.y+0.4, z), vxl.V(s.x1, s.y, z), vxl.V(s.x0, s.y, z)}, Color: colRed, Alpha: 0.5 + 0.5*sc.wallSurge, Additive: true, TwoSided: true}
		l := vxl.Grey(1.6 * k)
		q.Light = [4]vxl.RGB{l, l, l, l}
		r.Quad(q)
	}
	if sc.wallSurge > 0 {
		r.Billboard(vxl.V(float64(sc.lay.w)/2, 6, z+1), float64(sc.lay.w)*1.2, colRed, 0.1*sc.wallSurge, true)
	}
}

// Soft shadows under the crew and the packets
func (sc *scene) drawShadows(r *vxl.Raster) {
	shadow := func(at vxl.Vec3, w, d, alpha float64) {
		q := vxl.Quad{P: vxl.FloorQuad(vxl.V(at.X, 0, at.Z), w, d, 0.02), Color: vxl.RGB{}, Alpha: alpha, TwoSided: true}
		q.Light = [4]vxl.RGB{{}, {}, {}, {}}
		if at.Y > 4 {
			q.P = vxl.FloorQuad(vxl.V(at.X, 0, at.Z), w, d, spireHeight+0.02)
		}
		r.Quad(q)
	}
	for _, a := range sc.actors {
		if a.gone {
			continue
		}
		h := a.drop + a.lift + max(a.bob, 0)
		shadow(a.pos, 0.95/(1+h*0.3), 0.6/(1+h*0.3), 0.4*(1-a.exit)/(1+h*0.5))
	}
	for i := range sc.packets {
		at := sc.packets[i].at()
		shadow(at, 0.4, 0.4, 0.25/(1+at.Y*0.2))
	}
}

// Queues the search beam of the spire, a cone of light ending in a pool on the platform
func (sc *scene) drawBeam(r *vxl.Raster) {
	from := sc.lay.spireTop().Add(vxl.V(0, 2.2, 0))
	to := sc.beamAt.Add(vxl.V(0, 1.2, 0))
	col := colIce
	if sc.beamLock != "" {
		col = colNodeRev
	}
	if sc.ended && sc.endMode == endFail {
		col = colRed
	}
	k := 0.9 + 0.1*math.Sin(sc.t*7)
	_, _, forward := r.Basis()
	side := to.Sub(from).Cross(forward).Norm()
	w := vxl.Grey(1)
	for layer, width := range []float64{1, 0.45} {
		top, foot := side.Mul(0.15*width), side.Mul(1.3*width)
		ribbon := vxl.Quad{P: [4]vxl.Vec3{from.Sub(top), from.Add(top), to.Add(foot), to.Sub(foot)}, Color: col, Alpha: (0.08 + 0.1*float64(layer)) * k, Additive: true, TwoSided: true}
		ribbon.Light = [4]vxl.RGB{w, w, w, w}
		r.Quad(ribbon)
	}
	pool := vxl.Quad{P: vxl.FloorQuad(vxl.V(to.X, 0, to.Z), 2.6, 2.6, 0.03), Color: col, Alpha: 0.16 * k, Additive: true, TwoSided: true}
	pool.Light = [4]vxl.RGB{w, w, w, w}
	if sc.beamLock != "" {
		pool.P = vxl.FloorQuad(vxl.V(to.X, 0, to.Z), 2.6, 2.6, 1.05)
	}
	r.Quad(pool)
}

// Queues the beacons rising from members asking the person
func (sc *scene) drawBeacons(r *vxl.Raster) {
	for _, a := range sc.actors {
		if a.beacon <= 0.02 || a.gone {
			continue
		}
		head := a.headPos()
		for k := range 30 {
			y := 0.6 + float64(k)*0.8*a.beacon
			pulse := 0.5 + 0.5*math.Sin(sc.t*6-float64(k)*0.35)
			r.Billboard(head.Add(vxl.V(0, y, 0)), 1.1+0.5*pulse, colIce, (0.06+0.12*pulse)*a.beacon, true)
		}
	}
}

// Queues the data tether from each working builder to the core of its ticket, motes streaming in its tool color
func (sc *scene) drawTethers(r *vxl.Raster) {
	for _, a := range sc.actors {
		if a.state != stateJack || a.gone {
			continue
		}
		n := sc.nodes[a.duty.node]
		if n == nil {
			continue
		}
		from, to := a.headPos().Sub(vxl.V(0, 0.4, 0)), n.core(sc.t)
		col := a.toolKind.color()
		for k := range 12 {
			s := (float64(k) + 0.5) / 12
			p := from.Lerp(to, s)
			p.Y += math.Sin(s*math.Pi) * 0.7
			r.Billboard(p, 0.28, col, 0.25, true)
		}
		for k := range 3 {
			s := math.Mod(sc.t*1.4+float64(k)/3, 1)
			p := from.Lerp(to, s)
			p.Y += math.Sin(s*math.Pi) * 0.7
			r.Billboard(p, 0.5, col, 0.9, true)
		}
	}
}

// Queues the words passing between actors in a huddle as motes arcing between their heads
func (sc *scene) drawTalk(r *vxl.Raster) {
	for _, a := range sc.actors {
		b := a.partner
		if b == nil || a.state != stateTalk || a.gone || b.gone {
			continue
		}
		from, to := a.headPos(), b.headPos()
		for k := range 4 {
			s := math.Mod(sc.t*0.9+float64(k)/4, 1)
			p := from.Lerp(to, s)
			p.Y += math.Sin(s*math.Pi) * 0.8
			r.Billboard(p, 0.22+0.1*math.Sin(s*math.Pi), a.col, 0.7, true)
		}
	}
}

// Queues the ring under each builder draining with the time its exchange has left, red once the limit passed
func (sc *scene) drawClocks(r *vxl.Raster) {
	for _, a := range sc.actors {
		if a.clock <= 0 || a.gone || a.duty.kind != dutyNode {
			continue
		}
		share := min(a.clock, 1)
		col := colIce.Mix(colAmber, share)
		if a.clock >= 1 {
			col = colRed.Scale(0.6 + 0.4*math.Abs(math.Sin(sc.t*6)))
		}
		segs := 24
		w := vxl.Grey(1)
		for k := range segs {
			if float64(k)/float64(segs) < share && a.clock < 1 {
				continue
			}
			ang := float64(k) / float64(segs) * 2 * math.Pi
			s, c := math.Sincos(ang)
			center := a.pos.Add(vxl.V(1.5*c, 0.03, 1.5*s))
			dx, dz := -s*0.16, c*0.16
			nx, nz := c*0.1, s*0.1
			q := vxl.Quad{Color: col, Alpha: 0.8, Additive: true, TwoSided: true}
			q.Light = [4]vxl.RGB{w, w, w, w}
			q.P = [4]vxl.Vec3{center.Add(vxl.V(-dx-nx, 0, -dz-nz)), center.Add(vxl.V(-dx+nx, 0, -dz+nz)), center.Add(vxl.V(dx+nx, 0, dz+nz)), center.Add(vxl.V(dx-nx, 0, dz-nz))}
			r.Quad(q)
		}
	}
}

// Ring on the floor around a selected or hovered actor
func (sc *scene) ring(r *vxl.Raster, a *actor, col vxl.RGB, alpha float64) {
	if a == nil || a.gone {
		return
	}
	rad := 1.7 + 0.08*math.Sin(sc.t*4)
	y := 0.04
	if a.pos.Y > 4 {
		y = spireHeight + 0.04
	}
	w := vxl.Grey(1)
	for k := range 4 {
		ang := sc.t*1.2 + float64(k)*math.Pi/2
		s, c := math.Sincos(ang)
		center := a.pos.Add(vxl.V(rad*c, y-a.pos.Y, rad*s))
		q := vxl.Quad{Color: col, Alpha: alpha, Additive: true, TwoSided: true}
		q.Light = [4]vxl.RGB{w, w, w, w}
		dx, dz := -s*0.9, c*0.9
		nx, nz := c*0.12, s*0.12
		q.P = [4]vxl.Vec3{center.Add(vxl.V(-dx-nx, 0, -dz-nz)), center.Add(vxl.V(-dx+nx, 0, -dz+nz)), center.Add(vxl.V(dx+nx, 0, dz+nz)), center.Add(vxl.V(dx-nx, 0, dz-nz))}
		r.Quad(q)
	}
}
