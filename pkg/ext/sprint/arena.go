package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
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

// Seconds between syncs of the floor with the harness while nothing announces a change
const syncEvery = time.Second

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
	// Worker drawing the floor, and the newest frame it handed over
	stage *stage
	frame *frame
	// Surface the floor goes out through as pixels, nil while it goes out as glyphs
	surface *gfx.Surface
	proto   gfx.Protocol
	// Screen cell the content of the card starts at, so placed images land under the floor
	originX, originY int
	// Size and glyph library the worker was last told to draw for
	sentCols, sentRows int
	sentMode           vxl.CellMode
	sentAspect         float64
	// Camera preset in force, mirrored from the worker for the buttons and the saved state
	camera string
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
	needSync bool
	synced   time.Time
	focused  bool
	// Where every actor stood on the cells of the last paint, in card coordinates
	spots    map[string]spot
	pending  []tea.Cmd
	apRect   rect
	drag     *drag
	draftDir string
	// Sprints whose end the person has seen in the arena
	seen map[string]bool
	// Whether the terminal window lost focus since the arena last saw input
	blurred bool
}

// Builds the arena over the newest sprint
func newArena(env *cards.Env) wm.Card {
	a := &arena{env: env, sessions: map[string]*cards.Session{}, approvals: map[string]*cards.Approval{}, toolSeen: map[string]time.Time{}, tally: map[string]*toolTally{}, spots: map[string]spot{}, seen: map[string]bool{}, labels: true}
	a.secs = cards.NewSections(1 - defaultLogShare)
	a.bodies = map[section]cachedBody{}
	a.live = map[string]liveState{}
	a.dockWidth, a.inspWidth, a.inspHead = 44, 72, inspectorHead
	a.reloadConfig()
	if dir, err := profiles.ConfigDir(); err == nil {
		a.draftDir = dir
	}
	a.camera = a.cfg.Arena.Camera
	a.labels, a.dock = a.cfg.Arena.Labels, a.cfg.Arena.Sidebar
	a.stage = newStage(a, env.Send, env.Theme, a.cfg)
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

func (a *arena) Glyph() string       { return a.env.Theme.Glyphs.Mission }
func (a *arena) Key() string         { return ArenaKey }
func (a *arena) MinSize() (int, int) { return 60, 16 }
func (a *arena) Selectable() bool    { return true }
func (a *arena) Tracks() bool        { return true }

// Starts the worker and syncs the floor
func (a *arena) Init() tea.Cmd {
	a.stage.start()
	a.refresh()
	return nil
}

// Steps the session windows and syncs the floor with the harness now and then, the floor itself moving on its worker
func (a *arena) Animate(dt float64) bool {
	if a.needSync || time.Since(a.synced) > syncEvery {
		a.refresh()
	}
	changed := false
	if a.off() {
		for _, m := range a.roster() {
			if s := a.sessionOf(m.Name); s != nil && s.Animate(dt) {
				changed = true
			}
		}
		return changed
	}
	if s := a.inspected(); s != nil && s.Animate(dt) {
		changed = true
	}
	return changed
}

// Reports whether a cell lies on the dock
func (a *arena) onDock(x, y int) bool { return a.lay.dock.w > 0 && a.lay.dock.contains(x, y) }

// Reports whether the arena shows the session windows of the team instead of a world
func (a *arena) off() bool { return a.cfg.Arena.World == "off" }

// Protocol the floor goes out through as pixels, none while it goes out as glyphs
func (a *arena) protocol() gfx.Protocol {
	g := a.env.Graphics
	if g == nil || a.off() {
		return gfx.None
	}
	switch a.cfg.Arena.Display {
	case "glyphs":
		return gfx.None
	case "image":
		if p := g.Protocol(); p != gfx.None {
			return p
		}
		return g.Guess()
	}
	if p := g.Protocol(); p == gfx.KittyCells {
		return p
	}
	return gfx.None
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
	a.surface, a.proto = nil, gfx.None
	a.stage.post(func(s *stage) { s.setSurface(nil) })
}

// Panel widths, folds and camera the arena keeps between runs
func (a *arena) State() any {
	secs := map[string]bool{}
	for _, s := range sections {
		secs[s.String()] = a.secs.IsOpen(s.String())
	}
	return arenaState{DockWidth: a.dockWidth, InspectorWidth: a.inspWidth, Sections: secs, LogShare: 1 - a.secs.Share, SprintFull: a.sprintFull, Camera: a.camera, Labels: a.labels}
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
		a.setCamera(st.Camera)
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
	out := []wm.Hint{{Action: ActNext, Label: "member"}, {Action: ActInspect}, {Action: ActSteer}, {Action: ActView}}
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

// Takes the snapshot of the sprint shown again, the newest running sprint when none is chosen, and hands it to the floor
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
	a.synced, a.needSync = time.Now(), false
	a.snapGen++
	var snap *Snapshot
	if a.snap != nil {
		copy := *a.snap
		snap = &copy
	}
	live := make(map[string]liveState, len(a.live))
	for k, v := range a.live {
		live[k] = v
	}
	sel, hover, selTicket, hoverTicket := a.sel, a.hover, a.selTicket, a.hoverTicket
	a.stage.post(func(s *stage) {
		s.sync(snap, live)
		s.mark(sel, hover, selTicket, hoverTicket)
	})
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
	case frameMsg:
		if msg.f.gen > a.frameGen() {
			a.frame = msg.f
		}
		if a.needSync || time.Since(a.synced) > syncEvery {
			a.refresh()
		}
		return a, nil
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
				a.camera = a.cfg.Arena.Camera
			}
			cfg := a.cfg
			a.stage.post(func(s *stage) { s.configure(cfg) })
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
	case tea.FocusMsg:
		a.setBlurred(false)
		return a, nil
	case tea.BlurMsg:
		a.setBlurred(true)
		return a, nil
	case wm.FocusMsg:
		a.focused = msg.Focused
		if msg.Focused {
			a.setBlurred(false)
		}
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
		a.stage.halt()
		return a, nil
	case wm.ActionMsg:
		a.setBlurred(false)
		return a, a.act(msg.ID)
	case tea.KeyPressMsg, tea.PasteMsg:
		a.setBlurred(false)
		if s := a.inspected(); s != nil && a.typing {
			_, cmd := s.Update(msg)
			return a, cmd
		}
		return a, nil
	case tea.MouseClickMsg:
		a.setBlurred(false)
		return a, a.click(msg)
	case tea.MouseReleaseMsg:
		return a, a.release(msg)
	case tea.MouseMotionMsg:
		a.setBlurred(false)
		return a, a.motion(msg)
	case tea.MouseWheelMsg:
		a.setBlurred(false)
		return a, a.wheel(msg)
	}
	if s := a.inspected(); s != nil {
		_, cmd := s.Update(msg)
		return a, cmd
	}
	return a, nil
}

// Tells the floor whether the terminal window is focused, once per change
func (a *arena) setBlurred(blurred bool) {
	if a.blurred == blurred {
		return
	}
	a.blurred = blurred
	a.stage.post(func(s *stage) { s.focus(!blurred) })
}

// Generation of the frame shown, zero while none has arrived
func (a *arena) frameGen() uint64 {
	if a.frame == nil {
		return 0
	}
	return a.frame.gen
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

// Tells the floor which member and ticket are selected and under the pointer
func (a *arena) markFloor() {
	sel, hover, selTicket, hoverTicket := a.sel, a.hover, a.selTicket, a.hoverTicket
	a.stage.post(func(s *stage) { s.mark(sel, hover, selTicket, hoverTicket) })
}

// Switches the camera preset on the floor and remembers it for the buttons
func (a *arena) setCamera(name string) {
	a.camera = name
	a.stage.post(func(s *stage) { s.preset(name) })
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
	a.markFloor()
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
		return a.toggleFollow()
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
	case ActView:
		return a.view()
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
		a.stage.post(func(s *stage) { s.orbit(-0.2, 0) })
	case ActOrbitRight:
		a.stage.post(func(s *stage) { s.orbit(0.2, 0) })
	case ActOrbitUp:
		a.stage.post(func(s *stage) { s.orbit(0, -0.1) })
	case ActOrbitDown:
		a.stage.post(func(s *stage) { s.orbit(0, 0.1) })
	case ActZoomIn:
		a.stage.post(func(s *stage) { s.zoom(0.85) })
	case ActZoomOut:
		a.stage.post(func(s *stage) { s.zoom(1.18) })
	case ActCamera:
		return a.cycleCamera()
	case ActReset:
		a.stage.post(func(s *stage) { s.resetView() })
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

// Moves to the next camera preset, skipping the following one while nobody is selected
func (a *arena) cycleCamera() tea.Cmd {
	i := slices.Index(Cameras, a.camera)
	next := Cameras[(i+1)%len(Cameras)]
	if next == "follow" && a.sel == "" {
		next = Cameras[(i+2)%len(Cameras)]
	}
	a.setCamera(next)
	return wm.Notify("camera "+next, wm.Info)
}

// Keeps the camera on the selected member, or lets it go back to the configured preset
func (a *arena) toggleFollow() tea.Cmd {
	if a.camera == "follow" {
		next := a.cfg.Arena.Camera
		if next == "follow" {
			next = "overview"
		}
		a.setCamera(next)
		return wm.Notify("camera "+next, wm.Info)
	}
	if _, ok := a.selected(); !ok {
		return wm.Notify("select a member to follow", wm.Info)
	}
	a.setCamera("follow")
	return wm.Notify("following "+a.sel, wm.Info)
}

// Moves the team into the next world, the choice saved as a setting and taken up at once
func (a *arena) cycleWorld() tea.Cmd {
	i := slices.Index(Worlds, a.cfg.Arena.World)
	next := Worlds[(i+1)%len(Worlds)]
	return a.setWorld(next)
}

// Moves the team into a world, the choice saved as a setting and taken up at once
func (a *arena) setWorld(name string) tea.Cmd {
	a.cfg.Arena.World = name
	a.typing = false
	if name == "off" {
		a.releaseImage()
	}
	cfg := a.cfg
	a.stage.post(func(s *stage) { s.configure(cfg) })
	return tea.Batch(a.saveSetting(Name+".arena.world", name), wm.Notify("world "+name, wm.Info))
}

// Picker over the look of the floor, the world, the camera, the labels, the display and the quality
func (a *arena) view() tea.Cmd {
	t := a.env.Theme
	var items []wm.Item
	for _, w := range Worlds {
		w := w
		items = append(items, wm.Item{ID: "world:" + w, Title: "World " + w, Detail: worldDetail(w), Keys: mark(w == a.cfg.Arena.World, t), Run: func() tea.Cmd { return a.setWorld(w) }})
	}
	for _, c := range Cameras {
		c := c
		items = append(items, wm.Item{ID: "camera:" + c, Title: "Camera " + c, Detail: cameraDetail(c), Keys: mark(c == a.camera, t), Run: func() tea.Cmd {
			if c == "follow" {
				return a.toggleFollow()
			}
			a.setCamera(c)
			return wm.Notify("camera "+c, wm.Info)
		}})
	}
	items = append(items, wm.Item{ID: "labels", Title: "Labels", Detail: "names over the team and ids over the tickets", Keys: mark(a.labels, t), Run: func() tea.Cmd { a.labels = !a.labels; return nil }})
	for _, d := range Displays {
		d := d
		items = append(items, wm.Item{ID: "display:" + d, Title: "Display " + d, Detail: displayDetail(d, a.env.Graphics), Keys: mark(d == a.cfg.Arena.Display, t), Run: func() tea.Cmd { return a.saveSetting(Name+".arena.display", d) }})
	}
	for _, q := range Qualities {
		q := q
		items = append(items, wm.Item{ID: "quality:" + q, Title: "Quality " + q, Detail: qualityDetail(q), Keys: mark(q == a.cfg.Arena.Quality, t), Run: func() tea.Cmd { return a.saveSetting(Name+".arena.quality", q) }})
	}
	items = append(items, wm.Item{ID: "reset", Title: "Reset view", Detail: "put the camera back where the preset starts", Run: func() tea.Cmd { a.stage.post(func(s *stage) { s.resetView() }); return nil }})
	return wm.RequestOverlay(wm.NewPalette(t, items))
}

// Words on a world for the view picker
func worldDetail(name string) string {
	switch name {
	case "blackwall":
		return "red strands over a glossy dark floor"
	case "soulkiller":
		return "the cyan data sea"
	case "nightcity":
		return "a rooftop over the city at dawn"
	case "den":
		return "a netrunner den walled with screens"
	}
	return "the session windows of the team side by side"
}

// Words on a camera preset for the view picker
func cameraDetail(name string) string {
	switch name {
	case "overview":
		return "the whole floor"
	case "follow":
		return "stays on the selected member"
	case "orbit":
		return "turns slowly around the floor"
	}
	return "cuts between whatever is busiest"
}

// Words on a display choice for the view picker, naming what the terminal answered
func displayDetail(name string, g *gfx.Terminal) string {
	found := "no terminal graphics found"
	if g != nil {
		found = g.Describe()
	}
	switch name {
	case "glyphs":
		return "always glyphs"
	case "image":
		return "pixels through whatever the terminal draws, " + found
	}
	return "pixels only where images follow the cells, " + found
}

// Words on a quality for the view picker
func qualityDetail(name string) string {
	switch name {
	case "low":
		return "the bare raster"
	case "medium":
		return "adds the glow of the air and soft shadows"
	case "ultra":
		return "twice the resolution with every pass"
	}
	return "adds mirrored floors"
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
