package sprint

import (
	"math"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Height of the core of a node over its pedestal
const coreHeight = 2.8

// Ticket as a data core on a pedestal, sealed in ice until its runner cracks it open
type node struct {
	id     string
	status TicketStatus
	pos    vxl.Vec3
	level  int
	deps   []string
	round  int
	// Share of the shell the edits of the runner have broken
	cracks float64
	// Seconds left of the scan sweep a read started
	scan float64
	// Share of the ice shell left, eased toward what the status wants
	shell float64
	// Flash after a status change
	glow float64
	// Share of the way the node has risen from the void
	rise      float64
	riseDelay float64
	spin      float64
	// Strength of the corruption shown on a failed node
	corrupt float64
	flow    float64
	seed    float64
	edits   int
	reads   int
}

// Center of the core
func (n *node) core(t float64) vxl.Vec3 {
	return n.pos.Add(vxl.V(0, 1+coreHeight+0.15*math.Sin(t*1.3+n.seed*6)-14*(1-vxl.Smoothstep(n.rise)), 0))
}

// Share of shell the status keeps
func (n *node) shellWant() float64 {
	switch n.status {
	case TicketActive:
		return 1 - 0.7*n.cracks
	case TicketReview:
		return 0.25
	case TicketDone, TicketFailed:
		return 0
	}
	return 1
}

// Color of the core now, flashing after a change
func (n *node) color() vxl.RGB {
	c := nodeColor(n.status)
	if n.glow > 0 {
		c = c.Mix(colPaper, n.glow*0.7)
	}
	return c
}

// Advances the animation of the node, shards flying as the shell gives way
func (n *node) update(dt float64, sc *scene) {
	n.glow = max(n.glow-dt*0.8, 0)
	n.scan = max(n.scan-dt, 0)
	n.spin += dt * 0.4
	n.flow += dt * 2.2
	if n.rise < 1 {
		if n.riseDelay > 0 {
			n.riseDelay -= dt
			return
		}
		n.rise = min(n.rise+dt*0.9, 1)
		if n.rise >= 1 {
			sc.puff(n.pos.Add(vxl.V(0, 1, 0)), 10, colIce)
		}
		return
	}
	want := n.shellWant()
	if n.shell > want {
		gap := n.shell - want
		step := min(dt*1.4, gap)
		n.shell -= step
		if sc.rnd.Chance(gap*40, dt) {
			sc.shards(n.core(sc.t), 3)
		}
	} else {
		n.shell = min(n.shell+dt*0.5, want)
	}
	switch n.status {
	case TicketFailed:
		n.corrupt = min(n.corrupt+dt, 1)
		if sc.rnd.Chance(4, dt) {
			sc.glitch(n.core(sc.t), 3)
		}
	default:
		n.corrupt = max(n.corrupt-dt, 0)
	}
	if n.status == TicketActive && n.scan > 0 && sc.rnd.Chance(30, dt) {
		c := n.core(sc.t)
		sc.parts.Add(vxl.Particle{Pos: c.Add(vxl.V(sc.rnd.Spread()*1.4, sc.rnd.Spread()*1.4, sc.rnd.Spread()*1.4)), Vel: vxl.V(0, 1.6, 0), Life: 0.6, Col: colIce, Size: 0.12, Additive: true})
	}
}

// Light the core throws on everything near it
func (n *node) light(t float64) vxl.Light {
	k := 0.9
	switch n.status {
	case TicketActive:
		k = 1.1 + 0.25*math.Sin(t*5+n.seed*9)
	case TicketReview:
		k = 1.2 + 0.4*math.Sin(t*9)
	case TicketDone:
		k = 1.3
	case TicketFailed:
		k = 0.9 + 0.5*math.Abs(math.Sin(t*13+n.seed))
	case TicketBlocked:
		k = 0.45
	}
	if n.rise < 1 {
		k *= n.rise
	}
	return vxl.Light{Pos: n.core(t), Color: n.color().Scale(k * (0.6 + 0.4*n.glow)), Radius: 7}
}

// Queues the solid parts of the node, its core and socket, and the conduits feeding it
func (n *node) draw(r *vxl.Raster, sc *scene) {
	t := sc.t
	if n.rise <= 0 {
		return
	}
	c := n.core(t)
	col := n.color()
	alpha := vxl.Smoothstep(n.rise)
	r.EmissiveBox(c, vxl.V(1.5, 1.5, 1.5), n.spin, col, alpha, 1.1+0.3*n.glow)
	r.EmissiveBox(c, vxl.V(0.7, 2.2, 0.7), n.spin+0.785, col.Mix(colPaper, 0.5), alpha, 1.4)
	socket := vxl.FlatQuad(vxl.FloorQuad(n.pos, 0.5, 0.5, 1.01), col, 1.3)
	socket.Alpha = alpha
	r.Quad(socket)
	for k := range min(n.round, 4) {
		band := c.Add(vxl.V(0, 0.55-float64(k)*0.35, 0))
		r.EmissiveBox(band, vxl.V(1.7, 0.12, 1.7), n.spin+float64(k)*0.4, colNodeFail.Mix(colDark, 0.3), alpha, 1.1)
	}
	if n.corrupt > 0 {
		for k := range 5 {
			j := float64(k)
			off := vxl.V(math.Sin(t*7+j*2.1)*1.3, math.Cos(t*5+j*1.3)*1.1, math.Sin(t*6+j*0.7)*1.3)
			gc := colNodeFail
			if k%2 == 0 {
				gc = colDark
			}
			r.EmissiveBox(c.Add(off.Mul(n.corrupt)), vxl.V(0.35, 0.35, 0.35), t*3+j, gc, n.corrupt, 1.2)
		}
	}
	for _, dep := range n.deps {
		sc.drawConduit(r, n, dep)
	}
}

// Queues the translucent parts of the node, its ice shell, kernel glow and pool of light on the floor
func (n *node) drawGlow(r *vxl.Raster, sc *scene) {
	if n.rise <= 0 {
		return
	}
	t := sc.t
	c := n.core(t)
	col := n.color()
	alpha := vxl.Smoothstep(n.rise)
	if n.shell > 0.02 {
		ice := colIce
		if n.status == TicketBlocked {
			ice = colNodeBlock.Mix(colDark, 0.4)
		}
		size := 3.0 * (0.85 + 0.15*n.shell)
		r.OrientedBox(c, vxl.V(size, size, size), 0.3+0.05*math.Sin(t*0.5), ice, (0.1+0.3*n.shell)*alpha)
	}
	k := 0.6 + 0.4*math.Sin(t*4+n.seed*7)
	r.Billboard(c, 2.4+n.glow*2, col, (0.18+0.25*n.glow)*alpha*k, true)
	pool := vxl.Quad{P: vxl.FloorQuad(n.pos, 3.6, 3.6, 1.03), Color: col, Alpha: (0.13 + 0.07*k) * alpha, Additive: true, TwoSided: true}
	w := vxl.Grey(1)
	pool.Light = [4]vxl.RGB{w, w, w, w}
	r.Quad(pool)
	pool.P = vxl.FloorQuad(n.pos, 1.8, 1.8, 1.04)
	pool.Alpha = (0.15 + 0.1*k) * alpha
	r.Quad(pool)
}

// Queues the conduit from a dependency to a node, dim until the dependency is done, then flowing light
func (sc *scene) drawConduit(r *vxl.Raster, n *node, dep string) {
	cells := sc.lay.conduits[[2]string{n.id, dep}]
	d := sc.nodes[dep]
	if d == nil || len(cells) == 0 || n.rise < 1 || d.rise < 1 {
		return
	}
	col, base, flow := colIce.Scale(0.5), 0.35, false
	switch {
	case d.status == TicketDone && n.status == TicketDone:
		col, base = colNodeDone, 0.9
	case d.status == TicketDone:
		col, base, flow = colNodeDone, 0.6, true
	case d.status == TicketFailed || d.status == TicketBlocked:
		col, base = colNodeFail.Mix(colDark, 0.5), 0.4
	}
	w := vxl.Grey(1)
	for i, c := range cells {
		k := base
		if flow {
			k = base + 0.9*max(0, math.Sin(n.flow-float64(i)*0.55))
		}
		at := vxl.V(float64(c[0])+0.5, 0, float64(c[1])+0.5)
		q := vxl.Quad{P: vxl.FloorQuad(at, 0.22, 0.22, 0.02), Color: col, Alpha: 1, Light: [4]vxl.RGB{w, w, w, w}}
		q.Light = [4]vxl.RGB{vxl.Grey(k * 1.6), vxl.Grey(k * 1.6), vxl.Grey(k * 1.6), vxl.Grey(k * 1.6)}
		r.Quad(q)
	}
}

// Spark of data flying from one point to another along an arc, doing something on arrival
type packet struct {
	from, to vxl.Vec3
	t, dur   float64
	col      vxl.RGB
	arc      float64
	done     func(sc *scene)
}

// Where the packet is along its arc
func (p *packet) at() vxl.Vec3 {
	s := vxl.Smoothstep(min(p.t/p.dur, 1))
	out := p.from.Lerp(p.to, s)
	out.Y += math.Sin(s*math.Pi) * p.arc
	return out
}

// Sends a packet, its flight scaled to the distance
func (sc *scene) sendPacket(from, to vxl.Vec3, col vxl.RGB, done func(sc *scene)) {
	dist := from.Sub(to).Len()
	sc.packets = append(sc.packets, packet{from: from, to: to, dur: min(max(dist/22, 0.5), 1.8), col: col, arc: 2 + dist*0.12, done: done})
}

// Advances every packet, trails glowing behind them, the done ones calling back
func (sc *scene) updatePackets(dt float64) {
	kept := sc.packets[:0]
	for i := range sc.packets {
		p := &sc.packets[i]
		p.t += dt
		at := p.at()
		if sc.rnd.Chance(40, dt) {
			sc.parts.Add(vxl.Particle{Pos: at, Vel: vxl.V(sc.rnd.Spread()*0.4, 0.2, sc.rnd.Spread()*0.4), Life: 0.5, Col: p.col, Size: 0.16, Additive: true})
		}
		if p.t >= p.dur {
			sc.burst(p.to, 10, p.col, 0.6)
			if p.done != nil {
				p.done(sc)
			}
			continue
		}
		kept = append(kept, *p)
	}
	sc.packets = kept
}

// Queues every packet as a bright cube with a glow
func (sc *scene) drawPackets(r *vxl.Raster) {
	for i := range sc.packets {
		p := &sc.packets[i]
		at := p.at()
		r.EmissiveBox(at, vxl.V(0.45, 0.45, 0.45), p.t*9, p.col, 1, 1.5)
		r.Billboard(at, 1.4, p.col, 0.5, true)
	}
}

// Lights of every packet in flight
func (sc *scene) packetLights(out []vxl.Light) []vxl.Light {
	for i := range sc.packets {
		p := &sc.packets[i]
		out = append(out, vxl.Light{Pos: p.at(), Color: p.col.Scale(0.9), Radius: 4})
	}
	return out
}
