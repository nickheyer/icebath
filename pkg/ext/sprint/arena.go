package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Share of the flex height of the dock the log takes until the rule between the tickets and the log is dragged
const defaultLogShare = 0.45

// Seconds without a paint after which the arena takes itself for hidden and stops moving
const hiddenAfter = 1.5 * float64(time.Second)

// Longest step the floor advances by in one frame, so a stall never teleports the team
const maxStep = 0.1

// Floor units a followed member walks before the camera moves after it, so it rests while the member works
const followSlack = 2.5

// Frames per second the arena settles to while nobody walks, flies or turns the camera
const calmFPS = 8

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

// Where an actor stands on the cells of the last paint
type spot struct {
	x, top, bottom int
	depth          float64
}

// Box of cells
type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// Part of the dock, each with a header that folds it
type section int

const (
	secSprint section = iota
	secTeam
	secTickets
	secLog
)

// Name of a section in the saved state
func (s section) String() string {
	switch s {
	case secSprint:
		return "sprint"
	case secTeam:
		return "team"
	case secTickets:
		return "tickets"
	}
	return "log"
}

var sections = []section{secSprint, secTeam, secTickets, secLog}

// Boxes the parts of the arena take on the cells
type layout struct {
	bar   rect
	dock  rect
	world rect
	insp  rect
	full  bool
}

// Shot the cinematic camera holds, a point of interest and how long it stays on it
type cinema struct {
	target vxl.Vec3
	dist   float64
	left   float64
	yaw    float64
}

// What a press on the arena may turn into as the pointer moves
type dragKind int

const (
	dragNone dragKind = iota
	dragCamera
	dragDock
	dragInspector
	dragSplit
)

// Press that may become a drag, with where it started and what it moves
type drag struct {
	kind  dragKind
	x, y  int
	moved bool
	// Width or share the drag started from
	base float64
}

// Clickable thing painted on the last frame, a toolbar button, a section header or a dock row
type hit struct {
	id string
	r  rect
}

// Least and greatest widths of the dock and the inspector
const (
	minDockWidth = 28
	maxDockWidth = 90
	minInspWidth = 40
	maxInspWidth = 140
)

// State of the arena kept between runs of the app
type arenaState struct {
	DockWidth      int             `json:"dock_width"`
	InspectorWidth int             `json:"inspector_width"`
	Sections       map[string]bool `json:"sections"`
	LogShare       float64         `json:"log_share"`
	SprintFull     bool            `json:"sprint_full"`
	Camera         string          `json:"camera"`
	Labels         bool            `json:"labels"`
}

// Voxel world showing the newest sprint, its team moving as they work, each member openable and steerable from it
type arena struct {
	env    *cards.Env
	cfg    Config
	cfgErr error
	sc     *scene
	fr     vxl.Frame
	r      vxl.Raster
	cam    vxl.Camera
	// Floor and sky lit once per camera position, every frame drawn over it
	layer      vxl.Layer
	layerCam   vxl.Camera
	layerOK    bool
	layerWorld string
	post       vxl.Post
	// Cell encoder holding the glyph of every cell between frames so the floor does not shimmer
	enc vxl.Encoder
	// Image the floor shows as pixels in a terminal that draws them, with the bytes of the last frame and where the card sits on the screen
	surface          *gfx.Surface
	rgb              []byte
	originX, originY int
	rig              camRig
	cine             cinema
	// Tool starts of every member by session id
	tally    map[string]*toolTally
	sprintID string
	snap     *Snapshot
	// What the harness said every member was doing at the last sync, read by every paint until the next
	live map[string]liveState
	// Dollars the sprint has cost and how many sprints there are, as of the last sync
	spent       float64
	sprintCount int
	sprintIndex int
	sel         string
	selTicket   string
	hover       string
	hoverTicket string
	hoverHit    string
	inspect     bool
	typing      bool
	// Dock beside the world with the sprint, the team, the tickets and the log, each section folding on its own
	dock       bool
	dockWidth  int
	inspWidth  int
	secs       cards.Sections
	sprintFull bool
	showHelp   bool
	labels     bool
	// Bodies of the dock sections from the last paint and the count of syncs, so a section is rebuilt only on change
	bodies  map[section]cachedBody
	snapGen int
	// Session windows of the team as the last paint laid them, while no world shows
	tiles []tile
	// Clickable things of the last paint
	hits      []hit
	sessions  map[string]*cards.Session
	approvals map[string]*cards.Approval
	toolSeen  map[string]time.Time
	w, h      int
	lay       layout
	// Rows the inspector header took on the last paint
	inspHead int
	painted  time.Time
	last     time.Time
	acc      float64
	needSync bool
	synced   time.Time
	focused  bool
	spots    map[string]spot
	pending  []tea.Cmd
	apRect   rect
	drag     *drag
	// Spot the follow camera rests on and whose member it is
	followAt   vxl.Vec3
	followName string
	draftDir   string
	// Sprints whose end the person has seen in the arena
	seen map[string]bool
}

// Builds the arena over the newest sprint
func newArena(env *cards.Env) wm.Card {
	a := &arena{env: env, sessions: map[string]*cards.Session{}, approvals: map[string]*cards.Approval{}, toolSeen: map[string]time.Time{}, tally: map[string]*toolTally{}, spots: map[string]spot{}, seen: map[string]bool{}, labels: true}
	a.secs = cards.NewSections(1 - defaultLogShare)
	a.bodies = map[section]cachedBody{}
	a.live = map[string]liveState{}
	a.dockWidth, a.inspWidth, a.inspHead = 44, 72, inspectorHead
	a.reloadConfig()
	a.sc = newScene(a.cfg.Team.Builders, worldFor(a.cfg.Arena.World))
	if dir, err := profiles.ConfigDir(); err == nil {
		a.draftDir = dir
	}
	c := a.sc.lay.center()
	a.rig = camRig{preset: a.cfg.Arena.Camera, zoom: 1, tx: anim.NewValueWith(c.X, anim.Gentle), ty: anim.NewValueWith(c.Y, anim.Gentle), tz: anim.NewValueWith(c.Z, anim.Gentle), yaw: anim.NewValueWith(overviewYaw, anim.Gentle), pitch: anim.NewValueWith(overviewPitch, anim.Gentle), dist: anim.NewValueWith(50, anim.Gentle)}
	a.labels, a.dock = a.cfg.Arena.Labels, a.cfg.Arena.Sidebar
	a.refresh()
	return a
}

// Reads the sprint settings again, keeping the last good ones and the error when they do not parse
func (a *arena) reloadConfig() {
	if a.env.Editor == nil {
		a.cfg, a.cfgErr = Default(), nil
		return
	}
	cfg, err := Load(a.env.Editor.Config)
	a.cfgErr = err
	if err == nil {
		a.cfg = cfg
	} else if a.cfg.Roles == nil {
		a.cfg = Default()
	}
}

func (a *arena) Title() string {
	if a.snap != nil {
		return "ARENA " + a.env.Theme.Glyphs.Sep + " " + a.snap.ID
	}
	return "ARENA"
}

func (a *arena) Glyph() string           { return a.env.Theme.Glyphs.Mission }
func (a *arena) Key() string             { return ArenaKey }
func (a *arena) MinSize() (int, int)     { return 60, 16 }
func (a *arena) Selectable() bool        { return true }
func (a *arena) Tracks() bool            { return true }
func (a *arena) Init() tea.Cmd           { a.refresh(); return nil }
func (a *arena) Animate(dt float64) bool { return a.step() }

// Reports whether a cell lies on the dock
func (a *arena) onDock(x, y int) bool { return a.lay.dock.w > 0 && a.lay.dock.contains(x, y) }

// Reports whether the arena shows the session windows of the team instead of a world
func (a *arena) off() bool { return a.cfg.Arena.World == "off" }

// Reports whether the floor is drawn as pixels, asked for and drawn by the terminal
func (a *arena) imaged() bool {
	return a.cfg.Arena.Pixels == "image" && a.env.Graphics != nil && a.env.Graphics.Protocol() != gfx.None
}

// Takes the screen cell the content of the arena starts at
func (a *arena) Origin(x, y int) { a.originX, a.originY = x, y }

// Hands the terminal the escape text that takes the image of the floor off the screen
func (a *arena) releaseImage() {
	if a.surface == nil {
		return
	}
	if text := a.surface.Release(); text != "" && a.env.Send != nil {
		a.env.Send(tea.RawMsg{Msg: text})
	}
}

// Panel widths, folds and camera the arena keeps between runs
func (a *arena) State() any {
	secs := map[string]bool{}
	for _, s := range sections {
		secs[s.String()] = a.secs.IsOpen(s.String())
	}
	return arenaState{DockWidth: a.dockWidth, InspectorWidth: a.inspWidth, Sections: secs, LogShare: 1 - a.secs.Share, SprintFull: a.sprintFull, Camera: a.rig.preset, Labels: a.labels}
}

// Takes back the widths, folds and camera of the last run
func (a *arena) SetState(raw json.RawMessage) error {
	var st arenaState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	if st.DockWidth > 0 {
		a.dockWidth = min(max(st.DockWidth, minDockWidth), maxDockWidth)
	}
	if st.InspectorWidth > 0 {
		a.inspWidth = min(max(st.InspectorWidth, minInspWidth), maxInspWidth)
	}
	for _, s := range sections {
		if v, ok := st.Sections[s.String()]; ok {
			a.secs.Open[s.String()] = v
		}
	}
	if st.LogShare > 0 {
		a.secs.Share = 1 - min(max(st.LogShare, 0.2), 0.8)
	}
	a.sprintFull = st.SprintFull
	if slices.Contains(Cameras, st.Camera) {
		a.rig.preset = st.Camera
	}
	if st.Sections != nil {
		a.labels = st.Labels
	}
	return nil
}

// Holds the pointer over the floor for camera drags, over the panel edges for resizes and over the transcript bars for scrolling
func (a *arena) Grabs(x, y int) bool {
	if a.off() && a.lay.world.contains(x, y) {
		if tl, ok := a.tileAt(x, y); ok && y > tl.r.y {
			if s := a.sessionOf(tl.name); s != nil {
				return s.Grabs(x-tl.r.x, y-tl.r.y-1)
			}
		}
		return false
	}
	if a.lay.insp.contains(x, y) {
		if x == a.lay.insp.x && !a.lay.full {
			return true
		}
		if s := a.inspected(); s != nil {
			return s.Grabs(x-a.lay.insp.x-1, y-a.lay.insp.y-inspectorHead)
		}
		return false
	}
	if a.onDock(x, y) {
		return x == a.lay.dock.x+a.lay.dock.w-1 || y == a.secs.Split()
	}
	return a.lay.world.contains(x, y)
}

// Keymap contexts in force, the member typed to first, then the approval keys while one waits, then the arena
func (a *arena) Contexts() []string {
	if a.typing {
		if s := a.inspected(); s != nil {
			return append([]string{ContextTyping}, s.Contexts()...)
		}
		return []string{ContextTyping}
	}
	out := []string{}
	if a.approval() != nil {
		out = append(out, keys.Approval)
	}
	return append(out, ContextArena, keys.Panel)
}

// Keys worth a reminder in the mode the arena is in, the toolbar carrying the rest
func (a *arena) Hints() []wm.Hint {
	if a.typing {
		if s := a.inspected(); s != nil {
			return append([]wm.Hint{{Action: ActLeave}}, s.Hints()...)
		}
		return []wm.Hint{{Action: ActLeave}}
	}
	if a.showHelp {
		return []wm.Hint{{Action: ActHelp, Label: "close help"}}
	}
	out := []wm.Hint{{Action: ActNext, Label: "member"}, {Action: ActInspect}, {Action: ActSteer}}
	if ap := a.approval(); ap != nil {
		out = append([]wm.Hint{{Action: "approval.allow"}, {Action: "approval.deny"}}, out...)
	}
	return append(out, wm.Hint{Action: ActHelp})
}

// Stage and progress of the sprint shown, the rest standing on the toolbar
func (a *arena) Status() string {
	s := a.snap
	if s == nil {
		return "no sprint, run /" + Name + " <request>"
	}
	done, total := s.Progress()
	parts := []string{string(s.Stage)}
	if total > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d tickets", done, total))
	}
	if s.Running() && s.Paused != "" {
		parts = append(parts, "paused")
	}
	return strings.Join(parts, " "+a.env.Theme.Glyphs.Sep+" ")
}

// Chip of the workspace, the sprint and its strongest signal
func (a *arena) Lane() wm.Lane {
	s := a.snap
	if s == nil {
		return wm.Lane{Name: "arena"}
	}
	l := wm.Lane{Name: s.ID}
	done, total := s.Progress()
	l.Detail = string(s.Stage)
	if total > 0 {
		l.Detail += fmt.Sprintf(" %d/%d", done, total)
	}
	switch {
	case a.waitingText() != "":
		l.Signal = wm.Blocked
	case s.Running() && a.anyRunning():
		l.Signal = wm.Working
	case !s.Running() && !a.seen[s.ID] && s.Err != nil && !s.Stopped():
		l.Signal = wm.Failed
	case !s.Running() && !a.seen[s.ID]:
		l.Signal = wm.Fresh
	}
	return l
}

// First approval or question waiting on a member of the sprint shown
func (a *arena) Waiting() (string, wm.Signal, time.Time, bool) {
	text := a.waitingText()
	if text == "" {
		return "", wm.Idle, time.Time{}, false
	}
	return text, wm.Blocked, a.waitingSince(), true
}

// Words on the first item waiting on the person, empty while none does
func (a *arena) waitingText() string {
	s := a.snap
	if s == nil {
		return ""
	}
	for _, m := range s.Members {
		st := a.live[m.Name]
		switch {
		case st.approval:
			return "approve " + st.tool + " for " + m.Name
		case st.asking:
			return "answer " + m.Name
		}
	}
	if s.Running() && s.Paused != "" {
		return "resume " + s.ID
	}
	return ""
}

// Moment the first waiting item was raised, now when nothing is
func (a *arena) waitingSince() time.Time {
	if a.env.Approver != nil {
		if p := a.env.Approver.Pending(); len(p) > 0 {
			return p[0].Asked
		}
	}
	if a.env.Questions != nil {
		if q := a.env.Questions.Pending(); len(q) > 0 {
			return q[0].Asked
		}
	}
	return time.Now()
}

// Reports whether a run serves the session of a member
func (a *arena) ownedBy(run string, m Member) bool {
	if a.env.Editor == nil || m.Live == nil {
		return false
	}
	return a.env.Editor.LiveOfRun(run) == m.Live
}

// Reports whether any member of the sprint shown had a run going at the last sync
func (a *arena) anyRunning() bool {
	for _, st := range a.live {
		if st.running {
			return true
		}
	}
	return false
}

// Sprint shown, nil while this process started none
func (a *arena) sprint() *Sprint { return sprintByID(a.sprintID) }

// Takes the snapshot of the sprint shown again, the newest running sprint when none is chosen, and syncs the floor
func (a *arena) refresh() {
	all := Sprints()
	if a.sprintID == "" || sprintByID(a.sprintID) == nil {
		a.sprintID = ""
		for _, s := range all {
			if s.Running() {
				a.sprintID = s.ID
				break
			}
		}
		if a.sprintID == "" && len(all) > 0 {
			a.sprintID = all[0].ID
		}
	}
	a.sprintCount, a.sprintIndex = len(all), 0
	for i, s := range all {
		if s.ID == a.sprintID {
			a.sprintIndex = i
		}
	}
	if s := a.sprint(); s != nil {
		snap := s.Snapshot()
		a.snap = &snap
	} else {
		a.snap = nil
	}
	if a.snap != nil && a.sel != "" {
		if _, ok := a.snap.Member(a.sel); !ok {
			a.sel = ""
		}
	}
	if a.snap != nil && a.selTicket != "" {
		if _, ok := a.snap.Ticket(a.selTicket); !ok {
			a.selTicket = ""
		}
	}
	a.live = a.liveStates()
	a.spent = a.cost()
	a.sc.sync(a.snap, a.live, a.cfg)
	a.synced, a.needSync = time.Now(), false
	a.snapGen++
}

// What the harness says every member of the sprint shown is doing, with what it has spent and how long it has
func (a *arena) liveStates() map[string]liveState {
	out := map[string]liveState{}
	if a.snap == nil {
		return out
	}
	var runs []*harness.Run
	if a.env.Control != nil {
		runs = a.env.Control.Active()
	}
	var pending []*harness.Pending
	if a.env.Approver != nil {
		pending = a.env.Approver.Pending()
	}
	for _, m := range a.snap.Members {
		var st liveState
		st.heat = heatOf(a.snap.Spent[m.Name], a.cfg.Roles[m.Role].Budget)
		st.since, st.limit = exchangeStart(a.snap, m), stageLimit(a.cfg, m.Role)
		if t := a.tally[m.SessionID()]; t != nil {
			st.tally = *t
		}
		if m.Live != nil {
			for _, r := range runs {
				if r.Parent != "" || r.Session != m.Live.Session {
					continue
				}
				st.running = true
				st.tool, _ = r.LastTool()
			}
			if !st.running {
				st.failed = a.lastRunFailed(m)
			}
			st.toolAt = a.toolSeen[m.SessionID()]
			for _, p := range pending {
				if a.ownedBy(p.Run, m) {
					st.approval, st.tool = true, p.Request.Call.Name
				}
			}
			if a.env.Questions != nil && a.env.Questions.ForSession(m.SessionID()) != nil {
				st.asking = true
			}
			if a.env.Meter != nil {
				if tel, ok := a.env.Meter.SnapshotOf(m.SessionID()); ok {
					st.context, st.contextLimit, st.cost = tel.Context, tel.ContextLimit, tel.Cost
				}
			}
		}
		out[m.Name] = st
	}
	return out
}

// Reports whether the newest finished root run of a member ended in an error of its own
func (a *arena) lastRunFailed(m Member) bool {
	if a.env.Control == nil {
		return false
	}
	var last *harness.Run
	for _, r := range a.env.Control.Runs() {
		if r.Parent == "" && r.Session == m.Live.Session {
			last = r
		}
	}
	if last == nil || last.Running() {
		return false
	}
	err := last.Err()
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, agent.ErrInterrupted)
}

// Dollars the members of the sprint shown spent, from the meter by session
func (a *arena) cost() float64 {
	if a.env.Meter == nil || a.snap == nil {
		return 0
	}
	total := 0.0
	for _, m := range a.snap.Members {
		if tel, ok := a.env.Meter.SnapshotOf(m.SessionID()); ok {
			total += tel.Cost
		}
	}
	return total
}

// Member selected, false while none is
func (a *arena) selected() (Member, bool) {
	if a.snap == nil || a.sel == "" {
		return Member{}, false
	}
	return a.snap.Member(a.sel)
}

// Session card of a member, built on first use
func (a *arena) session(m Member) *cards.Session {
	if m.Live == nil {
		return nil
	}
	id := m.SessionID()
	if s, ok := a.sessions[id]; ok {
		return s
	}
	s := cards.NewSession(a.env, m.Live, a.draftDir)
	a.pending = append(a.pending, s.Init())
	a.sessions[id] = s
	return s
}

// Session card of a member by name, nil for one without a session
func (a *arena) sessionOf(name string) *cards.Session {
	if a.snap == nil {
		return nil
	}
	m, ok := a.snap.Member(name)
	if !ok {
		return nil
	}
	return a.session(m)
}

// Session card of the selected member while the inspector is open or the session windows show, nil otherwise
func (a *arena) inspected() *cards.Session {
	if !a.inspect && !a.off() {
		return nil
	}
	m, ok := a.selected()
	if !ok {
		return nil
	}
	return a.session(m)
}

// Approval dialog of the first member with an approval waiting, nil while none waits
func (a *arena) approval() *cards.Approval {
	if a.snap == nil || a.env.Approver == nil || a.env.Editor == nil {
		return nil
	}
	for _, m := range a.snap.Members {
		if m.Live == nil {
			continue
		}
		for _, p := range a.env.Approver.Pending() {
			if !a.ownedBy(p.Run, m) {
				continue
			}
			id := m.SessionID()
			ap, ok := a.approvals[id]
			if !ok {
				live := m.Live
				ap = cards.NewApproval(a.env, id, func(run string) bool { return a.env.Editor.LiveOfRun(run) == live })
				a.approvals[id] = ap
			}
			ap.Update(harness.ApprovalMsg{Pending: p})
			return ap
		}
	}
	return nil
}

// Session of a run, empty for one the harness no longer lists
func (a *arena) sessionOfRun(run string) string {
	if a.env.Control == nil {
		return ""
	}
	for run != "" {
		r, ok := a.env.Control.Get(run)
		if !ok {
			return ""
		}
		if r.Parent == "" {
			if r.Session == nil {
				return ""
			}
			return r.Session.ID()
		}
		run = r.Parent
	}
	return ""
}

// Handles a message, then runs whatever a session card built along the way asked for at its start
func (a *arena) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	c, cmd := a.update(msg)
	if len(a.pending) == 0 {
		return c, cmd
	}
	cmds := append(a.pending, cmd)
	a.pending = nil
	return c, tea.Batch(cmds...)
}

func (a *arena) update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case cards.ThinkingChangedMsg:
		if s, ok := a.sessions[msg.Session]; ok {
			s.Rethink()
		}
		return a, nil
	case ChangedMsg:
		if a.sprintID == "" || msg.ID == a.sprintID || a.snap == nil || !a.snap.Running() {
			a.needSync = true
		}
		return a, nil
	case ShowMsg:
		if msg.ID != a.sprintID {
			a.sprintID, a.sel, a.selTicket, a.typing = msg.ID, "", "", false
		}
		a.refresh()
		return a, nil
	case harness.FlowDoneMsg:
		a.refresh()
		return a, nil
	case harness.EventMsg:
		if msg.Event.Type == agent.EventToolStart {
			if id := a.sessionOfRun(msg.Run); id != "" {
				a.toolSeen[id] = time.Now()
				t := a.tally[id]
				if t == nil {
					t = &toolTally{}
					a.tally[id] = t
				}
				t.add(msg.Event.Call.Name)
			}
		}
		switch msg.Event.Type {
		case agent.EventRunStart, agent.EventRunEnd, agent.EventToolStart, agent.EventToolEnd, agent.EventTurnEnd:
			a.needSync = true
		}
		return a, a.forward(a.sessionOfRun(msg.Run), msg)
	case harness.RunDoneMsg:
		a.needSync = true
		return a, a.forward(a.sessionOfRun(msg.Run), msg)
	case harness.ProgressMsg:
		return a, a.forward(a.sessionOfRun(msg.Run), msg)
	case harness.NoteMsg:
		return a, a.forward(msg.Session, msg)
	case harness.AttemptMsg:
		return a, a.forward(a.sessionOfRun(msg.Attempt.Label), msg)
	case harness.ApprovalMsg, harness.ApprovalDoneMsg:
		a.needSync = true
		for _, ap := range a.approvals {
			ap.Update(msg)
		}
		return a, nil
	case harness.QuestionMsg, harness.QuestionDoneMsg:
		a.needSync = true
		var cmds []tea.Cmd
		for _, s := range a.sessions {
			_, cmd := s.Update(msg)
			cmds = append(cmds, cmd)
		}
		return a, tea.Batch(cmds...)
	case cards.SettingChangedMsg:
		if strings.HasPrefix(msg.Key, Name+".") {
			a.reloadConfig()
			a.labels, a.dock = a.cfg.Arena.Labels, a.cfg.Arena.Sidebar
			if a.cfgErr == nil && msg.Key == Name+".arena.camera" {
				a.rig.preset = a.cfg.Arena.Camera
			}
			a.refresh()
		}
		return a, nil
	case keys.ChangedMsg:
		var cmds []tea.Cmd
		for _, s := range a.sessions {
			_, cmd := s.Update(msg)
			cmds = append(cmds, cmd)
		}
		return a, tea.Batch(cmds...)
	case wm.FocusMsg:
		a.focused = msg.Focused
		if s := a.inspected(); s != nil && a.typing {
			_, cmd := s.Update(msg)
			return a, cmd
		}
		if !msg.Focused {
			a.drag = nil
		}
		return a, nil
	case wm.ClosedMsg:
		a.typing = false
		a.drag = nil
		a.releaseImage()
		return a, nil
	case wm.ActionMsg:
		return a, a.act(msg.ID)
	case tea.KeyPressMsg, tea.PasteMsg:
		if s := a.inspected(); s != nil && a.typing {
			_, cmd := s.Update(msg)
			return a, cmd
		}
		return a, nil
	case tea.MouseClickMsg:
		return a, a.click(msg)
	case tea.MouseReleaseMsg:
		return a, a.release(msg)
	case tea.MouseMotionMsg:
		return a, a.motion(msg)
	case tea.MouseWheelMsg:
		return a, a.wheel(msg)
	}
	if s := a.inspected(); s != nil {
		_, cmd := s.Update(msg)
		return a, cmd
	}
	return a, nil
}

// Hands a message to the session card of a member by session id, when one is built
func (a *arena) forward(sessionID string, msg tea.Msg) tea.Cmd {
	s, ok := a.sessions[sessionID]
	if !ok {
		return nil
	}
	_, cmd := s.Update(msg)
	return cmd
}

// Advances the floor and the camera by the wall clock, a frame due at the configured rate while things move and a calm rate otherwise
func (a *arena) step() bool {
	now := time.Now()
	if a.last.IsZero() {
		a.last = now
	}
	dt := min(now.Sub(a.last).Seconds(), maxStep)
	a.last = now
	if !a.painted.IsZero() && float64(now.Sub(a.painted)) > hiddenAfter {
		return false
	}
	if a.needSync || now.Sub(a.synced) > time.Second {
		a.refresh()
	}
	streaming := false
	if a.off() {
		for _, m := range a.roster() {
			if s := a.sessionOf(m.Name); s != nil {
				s.Animate(dt)
				streaming = streaming || s.Transcript.Busy()
			}
		}
	} else {
		a.sc.update(dt)
		a.moveCamera(dt)
		if s := a.inspected(); s != nil {
			s.Animate(dt)
			streaming = s.Transcript.Busy()
		}
	}
	a.acc += dt
	fps := float64(max(a.cfg.Arena.FPS, 5))
	if a.imaged() {
		most, _ := a.env.Graphics.Budget()
		fps = min(fps, float64(most))
	}
	if a.off() && !streaming || !a.off() && !a.sc.busy() && !streaming && a.rig.settled() && a.drag == nil {
		fps = min(fps, calmFPS)
	}
	frame := 1 / fps
	if a.acc < frame {
		return false
	}
	a.acc = math.Mod(a.acc, frame)
	return true
}

// Yaw and pitch of the overview camera, from the south east and well above the floor
const (
	overviewYaw   = 0.55
	overviewPitch = 0.72
)

// Eases the camera toward its preset, the selected member when following, the shots of the cinematic, with the turns and zoom of the person
func (a *arena) moveCamera(dt float64) {
	rig := &a.rig
	target, yaw, pitch := a.sc.lay.center(), overviewYaw, overviewPitch
	if rig.preset == "orbit" {
		rig.orbitT += dt
		yaw += rig.orbitT * 0.07
	}
	dist := a.fitDistance(yaw+rig.yawOff, min(max(pitch+rig.pitchOff, 0.12), 1.45))
	switch rig.preset {
	case "follow":
		if act := a.selectedActor(); act != nil {
			if a.followName != act.name || act.pos.Flat(a.followAt) > followSlack || math.Abs(act.pos.Y-a.followAt.Y) > 1 {
				a.followAt, a.followName = act.pos, act.name
			}
			target, pitch, dist = a.followAt.Add(vxl.V(0, 1.5, 0)), 0.6, 26
		}
	case "cinematic":
		a.frame(dt)
		target, pitch, dist, yaw = a.cine.target, 0.5, a.cine.dist, a.cine.yaw
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
	a.cam = vxl.Camera{Target: vxl.V(rig.tx.Pos, rig.ty.Pos, rig.tz.Pos), Yaw: rig.yaw.Pos, Pitch: rig.pitch.Pos, Dist: rig.dist.Pos, FOV: 0.85}
}

// Picks the next shot of the cinematic once the last ran out, the busiest point of the floor from a new angle, each shot held still
func (a *arena) frame(dt float64) {
	c := &a.cine
	c.left -= dt
	if c.left > 0 {
		return
	}
	c.left = 7 + a.sc.rnd.Next()*4
	c.yaw += 0.35 + a.sc.rnd.Next()*0.5
	sc := a.sc
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
	if len(spots) == 0 || a.sc.rnd.Next() < 0.2 {
		c.target, c.dist = sc.lay.center(), a.fitDistance(c.yaw, 0.5)*1.05
		return
	}
	c.target, c.dist = spots[a.sc.rnd.Intn(len(spots))], 24
}

// Distance at which the whole floor fits the world box at the yaw and pitch, found by projecting its corners
func (a *arena) fitDistance(yaw, pitch float64) float64 {
	ww, wh := max(a.lay.world.w, 20), max(a.lay.world.h, 8)
	mode := vxl.CellModeOf(a.cfg.Arena.Pixels)
	px, py := mode.Pixels()
	fr := &vxl.Frame{W: ww * px, H: wh * py, PixelAspect: mode.PixelAspect()}
	r := &vxl.Raster{}
	lo, hi := a.sc.lay.bounds()
	cam := vxl.Camera{Target: a.sc.lay.center(), Yaw: yaw, Pitch: pitch, Dist: 60, FOV: 0.85}
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

// Actor of the selected member, nil while none is selected
func (a *arena) selectedActor() *actor {
	if a.sel == "" {
		return nil
	}
	return a.sc.byName[a.sel]
}

// Members in roster order
func (a *arena) roster() []Member {
	if a.snap == nil {
		return nil
	}
	return a.snap.Members
}

// Moves the selection through the roster by a count, wrapping
func (a *arena) selectBy(d int) tea.Cmd {
	members := a.roster()
	if len(members) == 0 {
		return wm.Notify("no members yet", wm.Info)
	}
	i := slices.IndexFunc(members, func(m Member) bool { return m.Name == a.sel })
	if i < 0 {
		if d < 0 {
			i = 0
		} else {
			i = -1
		}
	}
	i = ((i+d)%len(members) + len(members)) % len(members)
	return a.selectMember(members[i].Name)
}

// Selects a member by name, the inspector following it when open
func (a *arena) selectMember(name string) tea.Cmd {
	if a.sel == name {
		return nil
	}
	var cmds []tea.Cmd
	if s := a.inspected(); s != nil && a.typing {
		_, cmd := s.Update(wm.FocusMsg{})
		cmds = append(cmds, cmd)
	}
	a.sel = name
	if s := a.inspected(); s != nil && a.typing {
		_, cmd := s.Update(wm.FocusMsg{Focused: true})
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// Carries out a keymap action
func (a *arena) act(id string) tea.Cmd {
	if strings.HasPrefix(id, "approval.") {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(wm.ActionMsg{ID: id})
			return cmd
		}
		return nil
	}
	if a.typing {
		if id == ActLeave {
			return a.setTyping(false)
		}
		if s := a.inspected(); s != nil {
			_, cmd := s.Update(wm.ActionMsg{ID: id})
			return cmd
		}
		return nil
	}
	switch id {
	case ActNext:
		return a.selectBy(1)
	case ActPrev:
		return a.selectBy(-1)
	case ActInspect:
		if a.off() {
			if _, ok := a.selected(); !ok {
				if cmd := a.selectBy(1); a.sel == "" {
					return cmd
				}
			}
			return a.setTyping(!a.typing)
		}
		return a.toggleInspect()
	case ActWorld:
		return a.cycleWorld()
	case ActSteer:
		if _, ok := a.selected(); !ok {
			if cmd := a.selectBy(1); a.sel == "" {
				return cmd
			}
		}
		a.inspect = true
		return a.setTyping(true)
	case ActFollow:
		if a.rig.preset == "follow" {
			a.rig.preset = a.cfg.Arena.Camera
			if a.rig.preset == "follow" {
				a.rig.preset = "overview"
			}
			return wm.Notify("camera "+a.rig.preset, wm.Info)
		}
		if _, ok := a.selected(); !ok {
			return wm.Notify("select a member to follow", wm.Info)
		}
		a.rig.preset = "follow"
		return wm.Notify("following "+a.sel, wm.Info)
	case ActBoard:
		return a.toggleSection(secTickets)
	case ActLog:
		return a.toggleSection(secLog)
	case ActSidebar:
		return a.setDock(!a.dock)
	case ActHelp:
		a.showHelp = !a.showHelp
	case ActLabels:
		a.labels = !a.labels
	case ActCustomize:
		return a.customize()
	case ActPause:
		return a.pauseOrResume()
	case ActStop:
		return a.stop()
	case ActRestart:
		return a.restart()
	case ActOpen:
		return a.openSession()
	case ActKill:
		return a.kill()
	case ActOrbitLeft:
		a.rig.yawOff -= 0.2
	case ActOrbitRight:
		a.rig.yawOff += 0.2
	case ActOrbitUp:
		a.rig.pitchOff -= 0.1
	case ActOrbitDown:
		a.rig.pitchOff += 0.1
	case ActZoomIn:
		a.rig.zoom = max(a.rig.zoom*0.85, 0.3)
	case ActZoomOut:
		a.rig.zoom = min(a.rig.zoom*1.18, 3)
	case ActCamera:
		i := slices.Index(Cameras, a.rig.preset)
		a.rig.preset = Cameras[(i+1)%len(Cameras)]
		if a.rig.preset == "follow" && a.sel == "" {
			a.rig.preset = Cameras[(i+2)%len(Cameras)]
		}
		return wm.Notify("camera "+a.rig.preset, wm.Info)
	case ActReset:
		a.rig.yawOff, a.rig.pitchOff, a.rig.zoom, a.rig.orbitT = 0, 0, 1, 0
	case ActSprintNext:
		return a.switchSprint(1)
	case ActSprintPrev:
		return a.switchSprint(-1)
	case ActPageDown:
		a.page(1)
	case ActPageUp:
		a.page(-1)
	case ActSettings:
		return func() tea.Msg { return cards.OpenPanelMsg{Key: SettingsKey} }
	}
	return nil
}

// Moves the team into the next world, the choice saved as a setting and taken up at once
func (a *arena) cycleWorld() tea.Cmd {
	i := slices.Index(Worlds, a.cfg.Arena.World)
	next := Worlds[(i+1)%len(Worlds)]
	a.cfg.Arena.World = next
	if next != "off" {
		a.sc.setWorld(worldFor(next))
	}
	a.typing = false
	return tea.Batch(a.saveSetting(Name+".arena.world", next), wm.Notify("world "+next, wm.Info))
}

// Shows or hides the dock, the choice saved as a setting
func (a *arena) setDock(on bool) tea.Cmd {
	a.dock = on
	return a.saveSetting(Name+".arena.sidebar", strconv.FormatBool(on))
}

// Folds a section of the dock or opens it, opening the dock when it was hidden
func (a *arena) toggleSection(s section) tea.Cmd {
	if !a.dock {
		a.secs.Show(s.String())
		return a.setDock(true)
	}
	a.secs.Toggle(s.String())
	return nil
}

// Opens the inspector on the selected member, or closes it
func (a *arena) toggleInspect() tea.Cmd {
	if a.inspect {
		cmd := a.setTyping(false)
		a.inspect = false
		return cmd
	}
	if _, ok := a.selected(); !ok {
		if cmd := a.selectBy(1); a.sel == "" {
			return cmd
		}
	}
	a.inspect = true
	return nil
}

// Hands the keys to the prompt of the inspected member or takes them back
func (a *arena) setTyping(on bool) tea.Cmd {
	s := a.inspected()
	if s == nil {
		a.typing = false
		return nil
	}
	if a.typing == on {
		return nil
	}
	a.typing = on
	_, cmd := s.Update(wm.FocusMsg{Focused: on})
	return cmd
}

// Scrolls the log of the dock by pages, the tickets when the log is folded
func (a *arena) page(d int) {
	sec := secLog
	if !a.secs.IsOpen(secLog.String()) {
		sec = secTickets
	}
	r, _ := a.secs.Rect(sec.String())
	a.scrollSection(sec.String(), d*max(r.H-2, 1))
}

// Scrolls a section by a count of rows, the log leaving its newest lines once scrolled up
func (a *arena) scrollSection(id string, rows int) {
	a.secs.Scroll(id, rows, id == secLog.String())
}

// Shows another sprint, by a count through the newest first list
func (a *arena) switchSprint(d int) tea.Cmd {
	all := Sprints()
	if len(all) < 2 {
		return wm.Notify("only one sprint has run", wm.Info)
	}
	i := slices.IndexFunc(all, func(s Snapshot) bool { return s.ID == a.sprintID })
	i = ((i+d)%len(all) + len(all)) % len(all)
	a.sprintID, a.sel, a.selTicket, a.typing = all[i].ID, "", "", false
	a.refresh()
	return wm.Notify("showing "+a.sprintID, wm.Info)
}

// Pauses the sprint shown before its next step, or resumes it when paused
func (a *arena) pauseOrResume() tea.Cmd {
	s := a.sprint()
	if s == nil || a.snap == nil || !a.snap.Running() {
		return wm.Notify("no sprint is running", wm.Info)
	}
	if a.snap.Paused != "" {
		if s.Resume() {
			a.refresh()
			return wm.Notify(a.sprintID+" resumes", wm.Success)
		}
		return nil
	}
	if s.Pause() {
		a.refresh()
		return wm.Notify(a.sprintID+" pauses before its next step", wm.Warn)
	}
	return nil
}

// Cancels the workflow of the sprint shown
func (a *arena) stop() tea.Cmd {
	if a.snap == nil || !a.snap.Running() {
		return wm.Notify("no sprint is running", wm.Info)
	}
	if a.env.Editor == nil || a.env.Editor.Flows == nil {
		return wm.Notify("cannot stop "+a.sprintID+", the app runs no workflows", wm.Error)
	}
	if err := a.env.Editor.Flows.Cancel(a.sprintID); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	return wm.Notify("stopping "+a.sprintID+", restart it later from where it was", wm.Warn)
}

// Starts the workflow of an ended sprint again from the stage it was in
func (a *arena) restart() tea.Cmd {
	s := a.sprint()
	if s == nil || a.snap == nil {
		return wm.Notify("no sprint to restart", wm.Info)
	}
	switch {
	case a.snap.Running():
		return wm.Notify(a.sprintID+" is running", wm.Info)
	case !a.snap.Restartable():
		return wm.Notify(a.sprintID+" is done, start another with /"+Name+" <request>", wm.Info)
	case a.env.Editor == nil || a.env.Editor.Flows == nil:
		return wm.Notify("cannot restart "+a.sprintID+", the app runs no workflows", wm.Error)
	}
	if _, err := a.env.Editor.Flows.Restart(a.env.Ctx, a.sprintID, workflow(), nil, a.snap.Request); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	a.seen[a.sprintID] = false
	return wm.Notify(a.sprintID+" restarts at the "+string(restartStage(*a.snap))+" stage", wm.Success)
}

// Stops the run of the selected member at once, the sprint then pausing for the person to steer it
func (a *arena) kill() tea.Cmd {
	m, ok := a.selected()
	if !ok || m.Live == nil || a.env.Control == nil {
		return wm.Notify("select a member first", wm.Info)
	}
	for _, r := range a.env.Control.Active() {
		if r.Parent == "" && r.Session == m.Live.Session {
			if err := a.env.Control.Cancel(r.ID); err != nil {
				return wm.Notify(err.Error(), wm.Error)
			}
			return wm.Notify("stopping the run of "+m.Name, wm.Warn)
		}
	}
	return wm.Notify(m.Name+" has no run going", wm.Info)
}

// Opens the session of the selected member as a window of its own, the stored one when none is open
func (a *arena) openSession() tea.Cmd {
	m, ok := a.selected()
	if !ok {
		return wm.Notify("select a member first", wm.Info)
	}
	return openMember(a.env, m, a.sprintID)
}

// Offers to rename or dress the selected member, each choice saved as a setting
func (a *arena) customize() tea.Cmd {
	m, ok := a.selected()
	if !ok {
		return wm.Notify("select a member to customize", wm.Info)
	}
	t := a.env.Theme
	look := a.cfg.LookOf(m.Role, m.Index)
	items := []wm.Item{{ID: "name", Title: "Rename " + m.Name, Detail: "the name shows in the arena and in every prompt from now on", Run: func() tea.Cmd { return a.renamePrompt(m) }}}
	for _, c := range ColorNames {
		c := c
		items = append(items, wm.Item{ID: "color:" + c, Title: "Color " + c, Detail: "dye the suit and trace lines of " + m.Name, Keys: mark(c == look.Color, t), Run: func() tea.Cmd { return a.setLook(m, Look{Color: c, Hat: look.Hat}) }})
	}
	for _, h := range RigNames {
		h := h
		items = append(items, wm.Item{ID: "hat:" + h, Title: "Hat " + h, Detail: "what " + m.Name + " wears on its head", Keys: mark(h == look.Hat, t), Run: func() tea.Cmd { return a.setLook(m, Look{Color: look.Color, Hat: h}) }})
	}
	return wm.RequestOverlay(wm.NewPalette(t, items))
}

// Mark a picker row carries when it is the value in force
func mark(on bool, t theme.Theme) string {
	if on {
		return t.Glyphs.Live
	}
	return ""
}

// Asks for a new name for a member and saves it
func (a *arena) renamePrompt(m Member) tea.Cmd {
	return wm.RequestOverlay(wm.NewPrompt(a.env.Theme, "name of "+m.Name, m.Name, func(text string) tea.Cmd {
		text = strings.TrimSpace(text)
		if text == "" {
			return wm.Notify("a name is required", wm.Warn)
		}
		names := slices.Clone(a.cfg.Names[m.Role])
		for len(names) <= m.Index {
			names = append(names, a.cfg.NameOf(m.Role, len(names)))
		}
		names[m.Index] = text
		return a.saveSetting(Name+".name."+string(m.Role), strings.Join(names, ", "))
	}))
}

// Saves the outfit of a member at its index of its role
func (a *arena) setLook(m Member, look Look) tea.Cmd {
	looks := slices.Clone(a.cfg.Looks[m.Role])
	for len(looks) <= m.Index {
		looks = append(looks, a.cfg.LookOf(m.Role, len(looks)))
	}
	looks[m.Index] = look
	return a.saveSetting(Name+".look."+string(m.Role), looksText(looks))
}

// Saves a setting through the app, which tells every card once it is in force
func (a *arena) saveSetting(key, value string) tea.Cmd {
	return func() tea.Msg { return cards.SettingMsg{Key: key, Value: value, Quiet: true} }
}

// Words a sprint count takes beside the id when more than one has run
func (a *arena) sprintOrdinal() string {
	if a.sprintCount < 2 {
		return ""
	}
	return render.Dim(fmt.Sprintf("%d of %d", a.sprintIndex+1, a.sprintCount), a.env.Theme)
}

var _ wm.Stateful = (*arena)(nil)
