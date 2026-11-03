//go:build ignore

package arena

import (
	"math"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// What the sprint requires of a member right now
type dutyKind int

const (
	dutyIdle dutyKind = iota
	dutySpot
	dutyNode
	dutyWaitNode
	dutySpire
	dutyGather
)

// Where a duty puts an actor and how it stands there
type duty struct {
	kind dutyKind
	at   vxl.Vec3
	face float64
	node string
	rest actorState
}

// Reports whether two duties send the actor to the same place for the same reason
func (d duty) same(o duty) bool { return d.kind == o.kind && d.node == o.node && d.at == o.at }

// One thing an actor does next
type stepKind int

const (
	stepWalk stepKind = iota
	stepFace
	stepHold
	stepLeap
	stepEmit
	stepExit
	stepUnpair
)

type step struct {
	kind  stepKind
	to    vxl.Vec3
	yaw   float64
	dur   float64
	state actorState
	emit  string
}

// Seconds a leap takes
const leapTime = 0.75

// Walking steps along the platform to a spot
func (sc *scene) walkSteps(a *actor, to vxl.Vec3) []step {
	var out []step
	for _, p := range sc.lay.path(a.pos, to) {
		out = append(out, step{kind: stepWalk, to: p})
	}
	return out
}

// Clears the errands and sends the actor walking to a spot
func (sc *scene) send(a *actor, to vxl.Vec3) {
	a.steps = append(a.steps[:0], sc.walkSteps(a, to)...)
}

// Breaks a huddle the actor is in
func (sc *scene) unpair(a *actor) {
	if a.partner != nil {
		if a.partner.partner == a {
			a.partner.partner = nil
			a.partner.ambient = ""
			a.partner.steps = a.partner.steps[:0]
		}
		a.partner = nil
	}
	if a.ambient == "huddle" {
		a.ambient = ""
	}
}

// Gives the actor a duty, planning the errand that takes it there
func (sc *scene) assign(a *actor, d duty) {
	sc.unpair(a)
	a.duty, a.ambient, a.rest = d, "", d.rest
	a.steps = a.steps[:0]
	switch d.kind {
	case dutyIdle:
		a.cool = 0.5 + sc.rnd.Next()
	case dutySpire:
		if a.pos.Y < spireHeight-0.5 {
			a.steps = append(a.steps, sc.walkSteps(a, sc.lay.spireFoot())...)
			a.steps = append(a.steps, step{kind: stepLeap, to: sc.lay.spireTop()})
		}
		a.steps = append(a.steps, step{kind: stepFace, yaw: d.face})
	default:
		if a.pos.Y > 0.5 {
			a.steps = append(a.steps, step{kind: stepLeap, to: sc.lay.ground(a.pos)})
		}
		a.steps = append(a.steps, sc.walkSteps(a, d.at)...)
		a.steps = append(a.steps, step{kind: stepFace, yaw: d.face})
		if d.kind == dutyNode {
			a.steps = append(a.steps, step{kind: stepEmit, emit: "jack"})
		}
	}
}

// Advances the errands, the motion and the pose of an actor by dt seconds
func (a *actor) update(dt float64, sc *scene) {
	if a.gone {
		return
	}
	if !a.landed {
		a.dropVel -= 40 * dt
		a.drop += a.dropVel * dt
		if a.drop <= 0 {
			a.drop, a.dropVel, a.landed = 0, 0, true
			sc.puff(a.pos, 12, colIce)
			sc.burst(a.pos.Add(vxl.V(0, 2.5, 0)), 8, colIce, 0.8)
		}
		a.phase += dt
		a.state = stateExit
		a.pose(dt)
		return
	}
	a.nod = max(a.nod-dt, 0)
	a.shake = max(a.shake-dt, 0)
	a.drone = max(a.drone-dt, 0)
	if a.emoteT > 0 {
		a.emoteT -= dt
		if a.emoteT <= 0 {
			a.emote = ""
		}
	}
	a.gloom += (flagF(a.rest == stateSlump) - a.gloom) * min(dt*1.5, 1)
	if a.exit > 0 {
		a.exit = min(a.exit+dt/1.7, 1)
		a.lift += dt * (1.5 + 6*a.exit)
		a.state = stateExit
		a.pose(dt)
		if a.exit >= 1 {
			a.gone = true
			sc.burst(a.stand().Add(vxl.V(0, 3, 0)), 24, colIce, 1.4)
		}
		return
	}
	if a.frozen > 0.5 {
		a.state = stateFrozen
		return
	}
	if a.leapT > 0 && a.leapT < 1 {
		a.leapT = min(a.leapT+dt/leapTime, 1)
		s := vxl.Smoothstep(a.leapT)
		a.pos = a.leapFrom.Lerp(a.leapTo, s)
		a.lift = math.Sin(a.leapT*math.Pi) * 4
		a.state = stateLeap
		a.lookAt(a.leapTo)
		a.turnToward(a.face, dt*2)
		if a.leapT >= 1 {
			a.lift = 0
			sc.puff(a.pos, 8, colIce)
		}
		a.pose(dt)
		return
	}
	for len(a.steps) > 0 {
		s := &a.steps[0]
		switch s.kind {
		case stepWalk:
			d := s.to.Sub(a.pos)
			d.Y = 0
			dist := d.Len()
			if dist < 0.08 {
				a.pos.X, a.pos.Z = s.to.X, s.to.Z
				a.steps = a.steps[1:]
				continue
			}
			a.state = stateWalk
			a.lookAt(s.to)
			a.turnToward(a.face, dt)
			speed := walkSpeed
			if a.role == RoleReviewer {
				speed *= 0.85
			}
			next := a.pos.Add(d.Mul(min(speed*dt, dist) / dist))
			here := cellOf(a.pos)
			if !sc.lay.free(cellOf(next)) && sc.lay.free(here) && a.pos.Y < 0.5 {
				rest := append([]step{{kind: stepWalk, to: centerOf(here)}}, sc.walkSteps(a, s.to)...)
				a.steps = append(rest, a.steps[1:]...)
				a.pose(dt)
				return
			}
			a.pos = next
			a.pose(dt)
			return
		case stepFace:
			a.face = s.yaw
			a.steps = a.steps[1:]
		case stepHold:
			a.state = s.state
			s.dur -= dt
			a.turnToward(a.face, dt)
			a.pose(dt)
			if s.dur <= 0 {
				a.steps = a.steps[1:]
			}
			return
		case stepLeap:
			a.leapFrom, a.leapTo, a.leapT = a.pos, s.to, 1e-6
			a.steps = a.steps[1:]
			return
		case stepEmit:
			sc.emitFor(a, s.emit)
			a.steps = a.steps[1:]
		case stepExit:
			a.exit = 1e-6
			a.steps = a.steps[1:]
			return
		case stepUnpair:
			sc.unpair(a)
			a.steps = a.steps[1:]
		}
	}
	a.state = a.rest
	if a.state == stateJack {
		a.jacked += dt
	} else {
		a.jacked = 0
	}
	a.turnToward(a.face, dt)
	a.pose(dt)
}

// Nudges standing or walking actors apart so none stands inside another
func (sc *scene) separate(dt float64) {
	for i, a := range sc.actors {
		if a.gone || !a.landed || a.frozen > 0 || a.state == stateSit || a.state == stateJack || a.leapT > 0 && a.leapT < 1 {
			continue
		}
		for _, b := range sc.actors[i+1:] {
			if b.gone || !b.landed || math.Abs(a.pos.Y-b.pos.Y) > 0.5 {
				continue
			}
			d := a.pos.Sub(b.pos)
			d.Y = 0
			dist := d.Len()
			if dist >= 1.4 || dist < 1e-6 {
				continue
			}
			push := d.Mul((1.4 - dist) / dist * dt * 3)
			if b.state != stateJack && b.state != stateSit && b.frozen == 0 && b.pos.Y < 0.5 && sc.lay.free(cellOf(b.pos.Sub(push))) {
				b.pos = b.pos.Sub(push)
			}
			if a.pos.Y > 0.5 || sc.lay.free(cellOf(a.pos.Add(push))) {
				a.pos = a.pos.Add(push)
			}
		}
	}
}

// Behaviors an idle actor may take up, weighted by its role
func (sc *scene) ambientWeights(a *actor) map[string]float64 {
	switch a.role {
	case RoleDirector:
		return map[string]float64{"wander": 2, "watch": 3, "gaze": 3, "huddle": 1.5, "inspect": 1, "perch": 1}
	case RoleArchitect:
		return map[string]float64{"inspect": 3, "huddle": 2, "wander": 1, "perch": 1, "watch": 2, "gaze": 1}
	}
	return map[string]float64{"perch": 3, "huddle": 2, "wander": 2, "inspect": 2, "watch": 1, "gaze": 1}
}

// Behaviors in the order the pick runs through them, so the same roll picks the same one
var ambientOrder = []string{"wander", "watch", "gaze", "huddle", "inspect", "perch"}

// Picks a behavior for an idle actor whose cooldown ran out, planning its steps
func (sc *scene) ambient(a *actor, dt float64) {
	if a.duty.kind != dutyIdle && a.duty.kind != dutyGather || a.busy() || !a.landed || a.gone || a.frozen > 0 || a.exit > 0 {
		return
	}
	a.cool -= dt
	if a.cool > 0 {
		return
	}
	weights := sc.ambientWeights(a)
	if a.duty.kind == dutyGather {
		weights = map[string]float64{"wander": 2, "huddle": 2, "perch": 2}
		if len(sc.queue) > 0 {
			weights = map[string]float64{"wander": 3, "huddle": 1}
		}
	}
	if len(sc.nodes) == 0 {
		delete(weights, "inspect")
	}
	if sc.findPartner(a) == nil {
		delete(weights, "huddle")
	}
	if sc.activeRunner(a) == nil {
		delete(weights, "watch")
	}
	if sc.freePerch(a) == nil {
		delete(weights, "perch")
	}
	total := 0.0
	for _, w := range weights {
		total += w
	}
	pick := sc.rnd.Next() * total
	name := "wander"
	for _, n := range ambientOrder {
		w, ok := weights[n]
		if !ok {
			continue
		}
		pick -= w
		if pick <= 0 {
			name = n
			break
		}
	}
	a.ambient = name
	a.cool = 4 + sc.rnd.Next()*6
	hold := func(state actorState, lo, hi float64) step {
		return step{kind: stepHold, state: state, dur: lo + sc.rnd.Next()*(hi-lo)}
	}
	switch name {
	case "inspect":
		id := sc.order[sc.rnd.Intn(len(sc.order))]
		for range 4 {
			if n := sc.nodes[id]; n.status == TicketDone || n.status == TicketFailed || n.status == TicketReview {
				break
			}
			id = sc.order[sc.rnd.Intn(len(sc.order))]
		}
		at, yaw := sc.lay.viewSpot(id, sc.rnd.Next())
		a.steps = append(sc.walkSteps(a, at), step{kind: stepFace, yaw: yaw}, step{kind: stepEmit, emit: "scan"}, hold(stateInspect, 3, 5))
	case "huddle":
		sc.huddle(a, sc.findPartner(a))
	case "perch":
		p := sc.freePerch(a)
		a.seat = p.at
		a.steps = append(sc.walkSteps(a, p.at), step{kind: stepFace, yaw: p.yaw}, hold(stateSit, 8, 15))
	case "watch":
		b := sc.activeRunner(a)
		at := sc.lay.roam(&sc.rnd, b.pos.Add(vxl.V(-3, 0, 3)), 2.5)
		a.steps = append(sc.walkSteps(a, at), step{kind: stepFace, yaw: vxl.HeadingOf(b.pos.Sub(at))}, hold(stateIdle, 4, 7))
	case "gaze":
		at := sc.lay.roam(&sc.rnd, vxl.V(float64(sc.lay.w)/2, 0, 5), 6)
		a.steps = append(sc.walkSteps(a, at), step{kind: stepFace, yaw: math.Pi}, hold(stateIdle, 5, 9))
	default:
		reach := 9.0
		if a.duty.kind == dutyGather {
			reach = 5
		}
		at := sc.lay.roam(&sc.rnd, a.pos, reach)
		a.steps = append(sc.walkSteps(a, at), hold(stateIdle, 2, 4))
	}
}

// Idle actor near enough to huddle with, nil when none is free
func (sc *scene) findPartner(a *actor) *actor {
	var best *actor
	bestD := 30.0
	for _, b := range sc.actors {
		if b == a || b.gone || !b.landed || b.busy() || b.partner != nil || b.frozen > 0 || b.role == RoleReviewer || b.duty.kind != dutyIdle && b.duty.kind != dutyGather {
			continue
		}
		if d := a.pos.Flat(b.pos); d < bestD {
			best, bestD = b, d
		}
	}
	return best
}

// Builder working at a node, the nearest to the actor, nil when none is
func (sc *scene) activeRunner(a *actor) *actor {
	var best *actor
	bestD := math.Inf(1)
	for _, b := range sc.actors {
		if b == a || b.gone || b.duty.kind != dutyNode {
			continue
		}
		if d := a.pos.Flat(b.pos); d < bestD {
			best, bestD = b, d
		}
	}
	return best
}

// Perch nobody sits at or heads for, nil when every one is taken
func (sc *scene) freePerch(a *actor) *perch {
	var free []*perch
	for i := range sc.lay.perches {
		p := &sc.lay.perches[i]
		taken := false
		for _, b := range sc.actors {
			if b != a && !b.gone && b.seat == p.at && (b.state == stateSit || b.busy()) {
				taken = true
				break
			}
		}
		if !taken {
			free = append(free, p)
		}
	}
	if len(free) == 0 {
		return nil
	}
	return free[sc.rnd.Intn(len(free))]
}

// Sends two actors to meet and talk a while
func (sc *scene) huddle(a, b *actor) {
	if b == nil {
		return
	}
	mid := a.pos.Lerp(b.pos, 0.5)
	at := sc.lay.roam(&sc.rnd, mid, 3)
	side := vxl.V(1, 0, 0)
	if sc.rnd.Next() < 0.5 {
		side = vxl.V(0, 0, 1)
	}
	pa, pb := sc.lay.nearest(at.Sub(side.Mul(0.8))), sc.lay.nearest(at.Add(side.Mul(0.8)))
	dur := 4 + sc.rnd.Next()*4
	a.partner, b.partner = b, a
	b.ambient, b.cool = "huddle", 2+sc.rnd.Next()*4
	a.steps = append(sc.walkSteps(a, pa), step{kind: stepFace, yaw: vxl.HeadingOf(pb.Sub(pa))}, step{kind: stepHold, state: stateTalk, dur: dur}, step{kind: stepUnpair})
	b.steps = append(sc.walkSteps(b, pb), step{kind: stepFace, yaw: vxl.HeadingOf(pa.Sub(pb))}, step{kind: stepHold, state: stateTalk, dur: dur + 0.5}, step{kind: stepUnpair})
}

// Interrupts the errands of an actor with a pose held a while, its duty resumed after
func (sc *scene) interject(a *actor, state actorState, dur float64) {
	if a.gone || !a.landed || a.frozen > 0 || a.exit > 0 {
		return
	}
	sc.unpair(a)
	a.steps = append([]step{{kind: stepHold, state: state, dur: dur}}, a.steps...)
}

// Idle actors within a reach of a spot, so a few can react to what happened there
func (sc *scene) bystanders(at vxl.Vec3, reach float64) []*actor {
	var out []*actor
	for _, b := range sc.actors {
		if b.gone || !b.landed || b.frozen > 0 || b.duty.kind == dutyNode || b.duty.kind == dutySpire || b.duty.kind == dutyWaitNode {
			continue
		}
		if b.pos.Flat(at) <= reach {
			out = append(out, b)
		}
	}
	return out
}

func flagF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
