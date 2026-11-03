package sprint

import (
	"image/color"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Seconds a sent frame may go unpainted before the worker takes the card for hidden and sleeps
const hiddenAfter = 1.5 * float64(time.Second)

// Seconds the worker waits for a frame to be painted before it draws the next one over it
const paintGrace = 250 * time.Millisecond

// Longest step the floor advances by in one frame, so a stall never teleports the team
const maxStep = 0.1

// Floor units a followed member walks before the camera moves after it, so it rests while the member works
const followSlack = 2.5

// Frames per second the floor settles to while nobody walks, flies or turns the camera
const calmFPS = 8

// Frames per second the floor draws at while the terminal window is not focused
const blurredFPS = 2

// Frames in a row the camera must change before a drifting camera draws as a turning one
const driftFrames = 2

// Yaw and pitch of the overview camera, from the south east and well above the floor
const (
	overviewYaw   = 0.55
	overviewPitch = 0.72
)

// Camera that eases toward where its preset and the person point it
type camRig struct {
	preset   string
	yawOff   float64
	pitchOff float64
	zoom     float64
	tx       anim.Value
	ty       anim.Value
	tz       anim.Value
	yaw      anim.Value
	pitch    anim.Value
	dist     anim.Value
	orbitT   float64
}

// Reports whether every spring of the camera is at rest
func (c *camRig) settled() bool {
	for _, v := range []*anim.Value{&c.tx, &c.ty, &c.tz, &c.yaw, &c.pitch, &c.dist} {
		if !v.Settled() {
			return false
		}
	}
	return true
}

// Jumps every spring of the camera to its target
func (c *camRig) snap() {
	for _, v := range []*anim.Value{&c.tx, &c.ty, &c.tz, &c.yaw, &c.pitch, &c.dist} {
		v.Snap()
	}
}

// Shot the cinematic camera holds, a point of interest and how long it stays on it
type cinema struct {
	target vxl.Vec3
	dist   float64
	left   float64
	yaw    float64
}

// Where an actor stands on the cells of a frame, relative to the top left cell of the floor
type spot struct {
	x, top, bottom int
	depth          float64
}

// Where the id of a ticket sits over its core on the cells of a frame, relative to the floor
type nodeMark struct {
	id     string
	x, y   int
	status TicketStatus
}

// What a member looked like on the floor when a frame was drawn
type actorMark struct {
	idle  bool
	emote string
	gone  bool
}

// One finished drawing of the floor, cells or packed pixels, with where everything landed on them
type frame struct {
	gen        uint64
	cols, rows int
	// Cells of the floor row by row, nil when the frame went out as pixels
	cells []uv.Cell
	// Pixels packed for the terminal and the surface that packed them, nil when the frame went out as cells
	packet  *gfx.Packet
	surface *gfx.Surface
	emitted bool
	spots   map[string]spot
	nodes   []nodeMark
	actors  map[string]actorMark
	banner  string
	moving  bool
	cost    time.Duration
	// Quality the frame was drawn at, the setting or the step it eased down to
	quality string
}

// Frame the worker finished, delivered to the arena card alone
type frameMsg struct {
	card wm.Card
	f    *frame
}

func (m frameMsg) Addressee() wm.Card { return m.card }

// Worker drawing the floor on its own goroutine, taking inputs as posted closures and handing frames back as messages
type stage struct {
	card     wm.Card
	send     func(tea.Msg)
	quantize func(vxl.RGB) color.Color
	// Colors of the rings around the selected and the hovered things on the floor
	ice, dim vxl.RGB

	mu      sync.Mutex
	pending []func(s *stage)
	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	running bool
	// Generation of the last frame the card painted, and when
	paintedGen atomic.Uint64
	paintedAt  atomic.Int64

	sc   *scene
	cfg  Config
	rig  camRig
	cine cinema
	// Cells of the floor, pixels of one cell across and down for glyphs, and the width over height of one terminal cell
	cols, rows int
	mode       vxl.CellMode
	cellAspect float64
	surface    *gfx.Surface
	fr         vxl.Frame
	r          vxl.Raster
	layer      vxl.Layer
	layerCam   vxl.Camera
	layerOK    bool
	layerWorld string
	layerQ     quality
	bloom      vxl.Post
	enc        vxl.Encoder
	rgb        []byte
	buf        *uv.Buffer
	sel        string
	hover      string
	selTicket  string
	hoverTick  string
	followAt   vxl.Vec3
	followName string
	blurred    bool
	gen        uint64
	sentAt     time.Time
	last       time.Time
	acc        float64
	// Whether the camera moved since the last frame, so one more full quality frame follows a drag
	refine bool
	// Camera of the last frame and how many frames in a row it changed, a drifting camera drawing as a turning one
	lastCam   vxl.Camera
	camStreak int
	// Running cost of a turning frame with every pass
	turnCost time.Duration
	// Steps the settled frame eased down from the setting, its running cost and the settled frames drawn since it last eased
	ease     int
	restCost time.Duration
	restRun  int
	// Cell buffers of recent frames, each reused once the card painted a newer frame
	cellBufs [3][]uv.Cell
	cellGens [3]uint64
}

// Builds a worker for a card over a team of some size in a world, the floor drawn through the theme
func newStage(card wm.Card, send func(tea.Msg), t theme.Theme, cfg Config) *stage {
	s := &stage{card: card, send: send, cfg: cfg, wake: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{}), cellAspect: 0.5, mode: vxl.CellModeOf(cfg.Arena.Glyphs), ice: vxl.FromColor(t.Ice), dim: vxl.FromColor(t.TextDim)}
	if send == nil {
		s.send = func(tea.Msg) {}
	}
	if t.Plain {
		s.quantize = vxl.ANSIQuantizer(t.Terminal)
	}
	vxl.SetWorkers(max(runtime.NumCPU()*3/4, 1))
	s.sc = newScene(cfg.Team.Builders, worldFor(cfg.Arena.World))
	c := s.sc.lay.center()
	s.rig = camRig{preset: cfg.Arena.Camera, zoom: 1, tx: anim.NewValueWith(c.X, anim.Gentle), ty: anim.NewValueWith(c.Y, anim.Gentle), tz: anim.NewValueWith(c.Z, anim.Gentle), yaw: anim.NewValueWith(overviewYaw, anim.Gentle), pitch: anim.NewValueWith(overviewPitch, anim.Gentle), dist: anim.NewValueWith(50, anim.Gentle)}
	return s
}

// Starts the worker goroutine, once
func (s *stage) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.running = true
	s.stop, s.stopped = make(chan struct{}), make(chan struct{})
	go s.run()
}

// Stops the worker goroutine and waits for it to end
func (s *stage) halt() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stop)
	stopped := s.stopped
	s.mu.Unlock()
	<-stopped
}

// Queues a closure for the worker goroutine and wakes it
func (s *stage) post(fn func(s *stage)) {
	s.mu.Lock()
	s.pending = append(s.pending, fn)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Records that the card painted a frame, waking the worker for the next one
func (s *stage) painted(gen uint64) {
	if gen > s.paintedGen.Load() {
		s.paintedGen.Store(gen)
	}
	s.paintedAt.Store(time.Now().UnixNano())
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Runs every queued closure on the worker goroutine
func (s *stage) drain() {
	s.mu.Lock()
	fns := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
}

// Worker loop, draining inputs, stepping the floor and drawing frames at the rate the state calls for
func (s *stage) run() {
	defer close(s.stopped)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-timer.C:
		}
		s.drain()
		wait := s.tick(time.Now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// Reports whether the last frame sent is still waiting to be painted
func (s *stage) unpainted() bool { return s.gen > s.paintedGen.Load() }

// Advances the floor by the wall clock and draws a frame when one is due, answering how long to wait for the next step
func (s *stage) tick(now time.Time) time.Duration {
	if s.cols <= 0 || s.rows <= 0 || s.cfg.Arena.World == "off" {
		return time.Hour
	}
	if s.unpainted() && float64(now.Sub(s.sentAt)) > hiddenAfter {
		return time.Hour
	}
	if s.last.IsZero() {
		s.last = now
	}
	dt := min(now.Sub(s.last).Seconds(), maxStep)
	s.last = now
	s.sc.update(dt)
	s.moveCamera(dt)
	s.acc += dt
	fps := s.rate()
	interval := 1 / fps
	if s.acc < interval {
		return time.Duration((interval - s.acc) * float64(time.Second))
	}
	if s.unpainted() && now.Sub(s.sentAt) < paintGrace {
		return 5 * time.Millisecond
	}
	if s.surface != nil && !s.surface.Ready(now) {
		return 10 * time.Millisecond
	}
	s.acc = math.Mod(s.acc, interval)
	s.render(now)
	return time.Duration(interval * float64(time.Second))
}

// Frames per second the floor draws at now, the configured rate while anything moves, calm while nothing does, slow while blurred
func (s *stage) rate() float64 {
	fps := float64(max(s.cfg.Arena.FPS, 5))
	if s.blurred {
		return blurredFPS
	}
	if !s.sc.busy() && s.rig.settled() && !s.refine && s.camStreak == 0 {
		fps = min(fps, calmFPS)
	}
	return fps
}

// Takes the size of the floor in cells, the glyph library and the shape of a terminal cell
func (s *stage) resize(cols, rows int, mode vxl.CellMode, cellAspect float64) {
	if cellAspect <= 0 {
		cellAspect = 0.5
	}
	s.cols, s.rows, s.mode, s.cellAspect = cols, rows, mode, cellAspect
	s.layerOK = false
	s.remeasure()
	s.acc = 1
}

// Takes the surface pixels go out through, nil for cells
func (s *stage) setSurface(sf *gfx.Surface) {
	s.surface = sf
	if sf != nil {
		sf.SetRate(max(s.cfg.Arena.FPS, 5))
	}
	s.layerOK = false
	s.remeasure()
	s.acc = 1
}

// Takes the settings again, moving the team into the world and the camera onto the preset they name
func (s *stage) configure(cfg Config) {
	world := s.cfg.Arena.World
	camera := s.cfg.Arena.Camera
	s.cfg = cfg
	s.mode = vxl.CellModeOf(cfg.Arena.Glyphs)
	if cfg.Arena.World != "off" && cfg.Arena.World != world {
		s.sc.setWorld(worldFor(cfg.Arena.World))
	}
	if cfg.Arena.Camera != camera {
		s.rig.preset = cfg.Arena.Camera
	}
	if s.surface != nil {
		s.surface.SetRate(max(cfg.Arena.FPS, 5))
	}
	s.layerOK = false
	s.remeasure()
	s.acc = 1
}

// Brings the floor in line with a sprint and what the harness says each member is doing
func (s *stage) sync(snap *Snapshot, live map[string]liveState) {
	s.sc.sync(snap, live, s.cfg)
	s.acc = 1
}

// Turns the camera by the person, so the next frame follows the hand
func (s *stage) orbit(dyaw, dpitch float64) {
	s.rig.yawOff += dyaw
	s.rig.pitchOff += dpitch
	s.refine = true
}

// Moves the camera closer or farther by a factor
func (s *stage) zoom(factor float64) {
	s.rig.zoom = min(max(s.rig.zoom*factor, 0.3), 3)
	s.refine = true
	s.remeasure()
}

// Puts the camera back where its preset starts
func (s *stage) resetView() {
	s.rig.yawOff, s.rig.pitchOff, s.rig.zoom, s.rig.orbitT = 0, 0, 1, 0
	s.refine = true
	s.remeasure()
}

// Switches the camera preset
func (s *stage) preset(name string) {
	s.rig.preset = name
	s.refine = true
	s.remeasure()
}

// Takes the member and ticket the person selected and the ones under the pointer, for the rings on the floor
func (s *stage) mark(sel, hover, selTicket, hoverTicket string) {
	s.sel, s.hover, s.selTicket, s.hoverTick = sel, hover, selTicket, hoverTicket
}

// Takes whether the terminal window is focused
func (s *stage) focus(on bool) { s.blurred = !on }

// Actor of the selected member, nil while none is selected
func (s *stage) selectedActor() *actor {
	if s.sel == "" {
		return nil
	}
	return s.sc.byName[s.sel]
}

// Eases the camera toward its preset, the selected member when following, the shots of the cinematic, with the turns and zoom of the person
func (s *stage) moveCamera(dt float64) {
	rig := &s.rig
	target, yaw, pitch := s.sc.lay.center(), overviewYaw, overviewPitch
	if rig.preset == "orbit" {
		rig.orbitT += dt
		yaw += rig.orbitT * 0.07
	}
	dist := s.fitDistance(yaw+rig.yawOff, min(max(pitch+rig.pitchOff, 0.12), 1.45))
	switch rig.preset {
	case "follow":
		if act := s.selectedActor(); act != nil {
			if s.followName != act.name || act.pos.Flat(s.followAt) > followSlack || math.Abs(act.pos.Y-s.followAt.Y) > 1 {
				s.followAt, s.followName = act.pos, act.name
			}
			target, pitch, dist = s.followAt.Add(vxl.V(0, 1.5, 0)), 0.6, 26
		}
	case "cinematic":
		s.shoot(dt)
		target, pitch, dist, yaw = s.cine.target, 0.5, s.cine.dist, s.cine.yaw
	}
	rig.tx.Set(target.X)
	rig.ty.Set(target.Y)
	rig.tz.Set(target.Z)
	rig.yaw.Set(yaw + rig.yawOff)
	rig.pitch.Set(min(max(pitch+rig.pitchOff, 0.12), 1.45))
	rig.dist.Set(max(dist*rig.zoom, 6))
	for _, v := range []*anim.Value{&rig.tx, &rig.ty, &rig.tz, &rig.yaw, &rig.pitch, &rig.dist} {
		v.Step(dt)
	}
}

// Camera the rig stands at now
func (s *stage) camera() vxl.Camera {
	rig := &s.rig
	return vxl.Camera{Target: vxl.V(rig.tx.Pos, rig.ty.Pos, rig.tz.Pos), Yaw: rig.yaw.Pos, Pitch: rig.pitch.Pos, Dist: rig.dist.Pos, FOV: 0.85}
}

// Picks the next cinematic shot once the last ran out, the busiest point of the floor from a new angle
func (s *stage) shoot(dt float64) {
	c := &s.cine
	c.left -= dt
	if c.left > 0 {
		return
	}
	sc := s.sc
	c.left = 7 + sc.rnd.Next()*4
	c.yaw += 0.35 + sc.rnd.Next()*0.5
	var spots []vxl.Vec3
	for _, act := range sc.actors {
		if act.gone {
			continue
		}
		switch {
		case act.beacon > 0.5, act.frozen > 0.5, act.state == stateJack, act.state == stateBrief, act.state == stateChart, act.state == stateReview && sc.beamLock != "":
			spots = append(spots, act.stand().Add(vxl.V(0, 2.5, 0)))
		}
	}
	for _, id := range sc.order {
		if n := sc.nodes[id]; n.status == TicketReview || n.glow > 0.5 {
			spots = append(spots, n.core(sc.t))
		}
	}
	if len(spots) == 0 || sc.rnd.Next() < 0.2 {
		c.target, c.dist = sc.lay.center(), s.fitDistance(c.yaw, 0.5)*1.05
		return
	}
	c.target, c.dist = spots[sc.rnd.Intn(len(spots))], 24
}

// Distance at which the whole floor fits the world box at the yaw and pitch, found by projecting its corners
func (s *stage) fitDistance(yaw, pitch float64) float64 {
	cols, rows := max(s.cols, 20), max(s.rows, 8)
	px, py := s.mode.Pixels()
	fr := &vxl.Frame{W: cols * px, H: rows * py, PixelAspect: s.pixelAspect(float64(px), float64(py))}
	r := &vxl.Raster{}
	lo, hi := s.sc.lay.bounds()
	cam := vxl.Camera{Target: s.sc.lay.center(), Yaw: yaw, Pitch: pitch, Dist: 60, FOV: 0.85}
	for range 6 {
		r.Begin(fr, cam)
		spread := 0.0
		for i := range 8 {
			c := lo
			if i&1 != 0 {
				c.X = hi.X
			}
			if i&2 != 0 {
				c.Y = hi.Y
			}
			if i&4 != 0 {
				c.Z = hi.Z
			}
			sx, sy, _, ok := r.Project(c)
			if !ok {
				spread = 2
				break
			}
			spread = max(spread, math.Abs(sx/float64(fr.W)-0.5)/0.48, math.Abs(sy/float64(fr.H)-0.5)/0.46)
		}
		cam.Dist *= spread
	}
	return cam.Dist
}

// Width over height of one frame pixel on screen when a cell holds px by py of them
func (s *stage) pixelAspect(px, py float64) float64 {
	return s.cellAspect * py / px
}

// Names of the steps a settled frame eases down through, the setting itself first
func (s *stage) easedName() string {
	switch s.ease {
	case 0:
		return s.cfg.Arena.Quality
	case 1:
		if s.cfg.Arena.Quality == "ultra" {
			return "high"
		}
		return s.cfg.Arena.Quality
	case 2:
		return "medium"
	}
	return "low"
}

// Quality of a settled frame, the setting eased down step by step while frames cost more than their budget
func (s *stage) restQuality() quality {
	q := qualityOf(s.cfg.Arena.Quality)
	switch s.ease {
	case 0:
		return q
	case 1:
		q.super = 1
		return q
	case 2:
		if q.occlude || q.haze || q.mirror {
			return qualityOf("medium")
		}
		return q
	}
	return qualityOf("low")
}

// Seconds one frame may take at the configured rate
func (s *stage) budget() time.Duration {
	return time.Duration(float64(time.Second) / float64(max(s.cfg.Arena.FPS, 5)))
}

// Folds the cost of a settled frame into the running cost, easing the quality down a step once two frames ran past the budget
func (s *stage) noteRest(cost time.Duration) {
	if s.restCost == 0 {
		s.restCost = cost
	} else {
		s.restCost = (s.restCost*3 + cost) / 4
	}
	s.restRun++
	if budget := s.budget(); s.restCost > budget+budget/4 && s.ease < 3 && s.restRun >= 2 {
		s.ease++
		s.restCost, s.restRun = 0, 0
	}
}

// Forgets every eased step and measured cost, so the next frames measure the view anew after it changed shape
func (s *stage) remeasure() {
	s.ease, s.restCost, s.restRun = 0, 0, 0
	s.turnCost = 0
}

// Quality the next frame draws at, the eased setting at rest, at one pixel per cell pixel while turning, bare once the passes cost more than a frame
func (s *stage) renderQuality(moving bool) quality {
	q := s.restQuality()
	if !moving {
		return q
	}
	if s.turnCost > s.budget() {
		return qualityOf("low")
	}
	q.super = 1
	return q
}

// Folds the cost of a turning frame drawn with every pass into the running cost
func (s *stage) noteTurn(cost time.Duration) {
	if s.turnCost == 0 {
		s.turnCost = cost
		return
	}
	s.turnCost = (s.turnCost*3 + cost) / 4
}

// Draws the floor through the camera into cells or pixels, places every mark and sends the frame to the card
func (s *stage) render(now time.Time) {
	if s.cols <= 0 || s.rows <= 0 || s.cfg.Arena.World == "off" {
		return
	}
	started := time.Now()
	cols, rows := s.cols, s.rows
	cam := s.camera()
	if cam != s.lastCam {
		s.camStreak++
	} else {
		s.camStreak = 0
	}
	s.lastCam = cam
	moving := !s.rig.settled() || s.camStreak >= driftFrames
	if !moving {
		s.refine = false
	}
	q := s.renderQuality(moving)
	bare := q.super == 1 && !q.occlude && !q.haze && !q.mirror
	fr := &s.fr
	image := s.surface != nil
	var px, py float64
	if image {
		w, h := s.surface.Budget(cols, rows)
		fr.PixelAspect = s.pixelAspect(float64(w)/float64(cols), float64(h)/float64(rows))
		fr.Resize(w, h)
		px, py = float64(w)/float64(cols), float64(h)/float64(rows)
	} else {
		cx, cy := s.mode.Pixels()
		cx, cy = cx*q.super, cy*q.super
		fr.PixelAspect = s.pixelAspect(float64(cx), float64(cy))
		fr.Resize(cols*cx, rows*cy)
		px, py = float64(cx), float64(cy)
	}
	sc := s.sc
	r := &s.r
	sc.world.light(r, sc, cam.Dist)
	q.apply(r)
	sky := func(x, y int) vxl.RGB { return sc.sky(r.Ray(float64(x)+0.5, float64(y)+0.5)) }
	fresh := moving || !s.layerOK || s.layerCam != cam || !s.layer.Fits(fr.W, fr.H) || sc.dirty || s.layerWorld != sc.world.name() || s.layerQ != q
	switch {
	case moving:
		r.SetLights(nil)
		r.Begin(fr, cam)
		fr.Clear(sky)
		sc.drawStatic(r)
		s.layerOK = false
	case fresh:
		r.SetLights(nil)
		r.Begin(fr, cam)
		fr.Clear(sky)
		sc.drawStatic(r)
		r.FlushInto(&s.layer)
		s.layerCam, s.layerOK, s.layerWorld, s.layerQ = cam, true, sc.world.name(), q
		s.layer.Restore(fr)
		r.Begin(fr, cam)
	default:
		s.layer.Restore(fr)
		r.Begin(fr, cam)
	}
	sc.drawDynamic(r)
	t := s.marks(r)
	if moving {
		r.Flush()
	} else {
		r.FlushOver(&s.layer)
	}
	fin := sc.world.finish()
	if !image {
		fin.grade.Aberration = 0
	}
	fr.Outline(fin.outline)
	if s.quantize == nil {
		fr.Bloom(&s.bloom, fin.bloomThreshold, fin.bloomStrength)
	}
	fr.Grade(fin.grade)
	if s.quantize == nil {
		fr.Vignette(fin.vignette)
	}
	f := &frame{cols: cols, rows: rows, moving: moving, spots: map[string]spot{}, actors: map[string]actorMark{}}
	if image {
		s.rgb = fr.Bytes(s.rgb)
		f.packet, f.surface = s.surface.Pack(s.rgb, fr.W, fr.H, cols, rows), s.surface
	} else {
		if s.buf == nil || s.buf.Width() != cols || s.buf.Height() != rows {
			s.buf = uv.NewBuffer(cols, rows)
		}
		s.enc.Encode(fr, s.buf, 0, 0, cols, rows, s.mode, s.quantize)
		f.cells = s.cellBuffer(cols * rows)
		for y := range rows {
			copy(f.cells[y*cols:(y+1)*cols], s.buf.Lines[y])
		}
	}
	cell := func(sx, sy float64) (int, int) { return int(sx / px), int(sy / py) }
	for _, act := range sc.actors {
		f.actors[act.name] = actorMark{idle: ambientText(act) != "", emote: act.emote, gone: act.gone}
		if act.gone {
			continue
		}
		fx, fy, depth, ok := r.Project(act.stand())
		hx, hy, _, ok2 := r.Project(act.headPos().Add(vxl.V(0, 0.5, 0)))
		if !ok || !ok2 {
			continue
		}
		cx, bottom := cell(fx, fy)
		_, top := cell(hx, hy)
		f.spots[act.name] = spot{x: cx, top: top, bottom: bottom, depth: depth}
	}
	for _, id := range sc.order {
		n := sc.nodes[id]
		if n.rise < 1 {
			continue
		}
		sx, sy, _, ok := r.Project(n.core(sc.t).Add(vxl.V(0, 1.6, 0)))
		if !ok {
			continue
		}
		cx, cy := cell(sx, sy)
		f.nodes = append(f.nodes, nodeMark{id: id, x: cx, y: cy, status: n.status})
	}
	f.banner = t
	f.cost = time.Since(started)
	switch {
	case moving && !bare:
		s.noteTurn(f.cost)
	case !moving && !fresh:
		s.noteRest(f.cost)
	}
	if moving {
		f.quality = "low"
		if !bare {
			f.quality = s.easedName()
		}
	} else {
		f.quality = s.easedName()
	}
	s.gen++
	f.gen = s.gen
	s.sentAt = now
	s.send(frameMsg{card: s.card, f: f})
}

// Cell buffer for a frame of n cells, one of the recent ones once the card painted past it, else a fresh one
func (s *stage) cellBuffer(n int) []uv.Cell {
	painted := s.paintedGen.Load()
	for i := range s.cellBufs {
		if s.cellGens[i] < painted && cap(s.cellBufs[i]) >= n {
			s.cellGens[i] = s.gen + 1
			return s.cellBufs[i][:n]
		}
	}
	buf := make([]uv.Cell, n)
	for i := range s.cellBufs {
		if s.cellBufs[i] == nil || s.cellGens[i] < painted {
			s.cellBufs[i], s.cellGens[i] = buf, s.gen+1
			break
		}
	}
	return buf
}

// Queues the rings around the selected and hovered member and ticket, answering the stage banner
func (s *stage) marks(r *vxl.Raster) string {
	sc := s.sc
	ice, dim := s.ice, s.dim
	if act := s.selectedActor(); act != nil {
		sc.ring(r, act, ice, 0.85)
	}
	if s.hover != "" && s.hover != s.sel {
		sc.ring(r, sc.byName[s.hover], dim, 0.5)
	}
	if n := sc.nodes[s.selTicket]; n != nil {
		sc.nodeRing(r, n, ice, 0.7)
	}
	if n := sc.nodes[s.hoverTick]; n != nil && s.hoverTick != s.selTicket {
		sc.nodeRing(r, n, dim, 0.4)
	}
	if sc.stageFlash <= 0 || sc.stageText == "" {
		return ""
	}
	return sc.stageText
}
