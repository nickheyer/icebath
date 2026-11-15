package arena

import (
	"fmt"
	"image/color"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Widths the dock and the inspector may be dragged between, and the width they open at
const (
	minDockWidth  = 30
	maxDockWidth  = 80
	dockOpenWidth = 46
	minInspWidth  = 34
	maxInspWidth  = 90
	inspOpenWidth = 48
)

// Seconds an ended turn stays on the floor for its finale before the lobby comes back
const finaleHold = 30 * time.Second

// Operations the harness offers the arena, each answering the command that carries it out
type Harness interface {
	// Interrupts a subagent, or the turn for the director
	Interrupt(s *Snapshot, m Member) tea.Cmd
	// Sends guidance to a running subagent, or to the main agent
	Steer(s *Snapshot, m Member, text string) tea.Cmd
	// Shows the newest output of a subagent, or the session of the main agent
	Open(s *Snapshot, m Member) tea.Cmd
	// Pauses or resumes spawning of new agents
	Pause(paused bool) tea.Cmd
	// Interrupts the turn, or every agent still running in it
	Stop(s *Snapshot) tea.Cmd
	// Runs the prompt of the newest turn again
	Retry() tea.Cmd
}

// Arena card as the app holds it, told about settings and theme changes from outside its update loop
type Card interface {
	wm.Card
	// Pulls the newest state of the journal onto the floor
	Refresh()
	// Takes new settings and the error of the ones that did not parse
	Configure(cfg Config, err error)
	// Takes a new theme
	Retheme(t theme.Theme)
}

// Box of cells inside the card
type rect struct{ x, y, w, h int }

// Reports whether a cell lies in the box
func (r rect) contains(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// Boxes of one paint, full when the inspector takes the whole card
type layout struct {
	bar, dock, world, tape, insp rect
	full                         bool
}

// Box a painted thing took and the id a press on it carries out
type hit struct {
	id string
	r  rect
}

// What a held pointer drags
type dragKind int

const (
	dragCamera dragKind = iota
	dragInspector
	dragSplit
	dragDock
	dragTape
)

// Drag under way, where it started, the size it started from and whether it moved
type drag struct {
	kind  dragKind
	x, y  int
	base  float64
	moved bool
}

// Section of the dock
type section int

const (
	secSprint section = iota
	secTickets
	secLog
)

// Every section of the dock in order
var sections = []section{secSprint, secTickets, secLog}

func (s section) String() string {
	switch s {
	case secTickets:
		return "tickets"
	case secLog:
		return "log"
	}
	return "sprint"
}

// Tape of a turn as last read at a revision of the journal, and whether the turn ran then
type tapeMemo struct {
	id      string
	rev     uint64
	running bool
	t       tape
}

// Card showing the orchestration of the session as a crew on a voxel platform
type arena struct {
	env    *cards.Env
	j      *Journal
	ops    Harness
	cfg    Config
	cfgErr error
	stage  *stage

	w, h             int
	lay              layout
	originX, originY int

	frame      *frame
	paintedGen uint64
	surface    *gfx.Surface
	proto      gfx.Protocol
	sentCols   int
	sentRows   int
	sentMode   vxl.CellMode
	sentAspect float64
	spots      map[string]spot
	hits       []hit
	labels     bool

	sprintID    string
	sprints     []string
	sprintCount int
	snap        *Snapshot
	live        map[string]liveState
	seenRev     uint64
	tapeMemo    tapeMemo
	posted      string
	since       map[string]int
	replay      *replay
	ticked      time.Time

	sel         string
	hover       string
	selTicket   string
	hoverTicket string
	hoverHit    string

	dock       bool
	dockWidth  int
	inspect    bool
	inspWidth  int
	inspHead   int
	secs       cards.Sections
	dockMemo   dockCache
	sprintFull bool
	showHelp   bool
	drag       *drag
	sheets     map[string]*sheet
	tiles      []tile
	closed     bool
}

// Arena card over a journal, operating the harness and drawing in the settings given
func New(env *cards.Env, j *Journal, h Harness, cfg Config, cfgErr error) Card {
	a := &arena{env: env, j: j, ops: h, cfg: cfg, cfgErr: cfgErr, labels: cfg.Arena.Labels, dock: true, dockWidth: dockOpenWidth, inspWidth: inspOpenWidth,
		secs: cards.NewSections(0.45), since: map[string]int{}, sheets: map[string]*sheet{}, spots: map[string]spot{}}
	a.stage = newStage(a, env.Send, env.Theme, cfg)
	a.refresh()
	return a
}

func (a *arena) Title() string       { return "ARENA" }
func (a *arena) Key() string         { return Name }
func (a *arena) Glyph() string       { return a.env.Theme.Glyphs.Mission }
func (a *arena) Contexts() []string  { return []string{Context} }
func (a *arena) MinSize() (int, int) { return 44, 14 }
func (a *arena) Tracks() bool        { return true }
func (a *arena) Grabs(x, y int) bool { return true }
func (a *arena) Origin(x, y int)     { a.originX, a.originY = x, y }
func (a *arena) Refresh()            { a.refresh() }
func (a *arena) Selectable() bool    { return false }
func (a *arena) Init() tea.Cmd       { a.stage.start(); return nil }
func (a *arena) off() bool           { return a.cfg.Arena.World == "off" }
func (a *arena) Configure(cfg Config, err error) {
	if a.closed {
		return
	}
	if cfg.Arena.Labels != a.cfg.Arena.Labels {
		a.labels = cfg.Arena.Labels
	}
	a.cfg, a.cfgErr = cfg, err
	a.stage.post(func(s *stage) { s.configure(cfg) })
}

// Takes a new theme, the rings and the cell colors of the floor following it
func (a *arena) Retheme(t theme.Theme) {
	if a.closed {
		return
	}
	a.env.Theme = t
	ice, dim := vxl.FromColor(t.Ice), vxl.FromColor(t.TextDim)
	var quantize func(vxl.RGB) color.Color
	if t.Plain {
		quantize = vxl.ANSIQuantizer(t.Terminal)
	}
	a.stage.post(func(s *stage) { s.ice, s.dim, s.quantize = ice, dim, quantize; s.layerOK = false })
}

// Trailer of the top edge, the turn shown, its stage and what replays
func (a *arena) Status() string {
	s := a.snap
	if s == nil {
		if n := len(a.sprints); n > 0 {
			return fmt.Sprintf("lobby · %d turns", n)
		}
		return "lobby"
	}
	parts := []string{a.turnLabel(s)}
	if s.Stage != "" {
		parts = append(parts, string(s.Stage))
	}
	switch {
	case a.replay != nil:
		parts = append(parts, "replay "+clockText(a.moment().Sub(s.Started)))
	case s.Running():
		parts = append(parts, "live")
	default:
		parts = append(parts, outcomeWord(s))
	}
	return strings.Join(parts, " · ")
}

// Name of a turn, its number or a mark for agents the journal found already running
func (a *arena) turnLabel(s *Snapshot) string {
	if s.Turn == 0 {
		return "agents"
	}
	return fmt.Sprintf("turn %d", s.Turn)
}

// Moment the turn shown stands at, the replay cursor, now while it runs, else its end
func (a *arena) moment() time.Time {
	s := a.snap
	switch {
	case a.replay != nil:
		return a.replay.cursor
	case s == nil, s.Running():
		return time.Now()
	case !s.Ended.IsZero():
		return s.Ended
	}
	return s.At
}

// Word on how a turn ended
func outcomeWord(s *Snapshot) string {
	switch {
	case s.Running():
		return "running"
	case s.Err != nil:
		return "failed"
	case s.Stopped():
		return "stopped"
	}
	return "done"
}

// Bound keys the bottom edge offers
func (a *arena) Hints() []wm.Hint {
	out := []wm.Hint{{Action: ActSidebar}, {Action: ActInspect}}
	if a.snap != nil {
		out = append(out, wm.Hint{Action: ActReplay})
	}
	if len(a.sprints) > 1 {
		out = append(out, wm.Hint{Action: ActSprintPrev, Pair: ActSprintNext, Label: "turns"})
	}
	return append(out, wm.Hint{Action: ActHelp})
}

// Moves the replay and syncs the floor, reporting whether a new frame waits to be painted
func (a *arena) Animate(dt float64) bool {
	now := time.Now()
	if r := a.replay; r != nil && r.playing {
		if t, ok := a.tape(r.sprint); ok {
			step := now.Sub(a.ticked)
			if a.ticked.IsZero() || step > time.Second {
				step = time.Duration(dt * float64(time.Second))
			}
			if r.advance(t, step) {
				if running, _, ok := a.j.Status(r.sprint); ok && running {
					a.replay = nil
				}
			}
		}
	}
	a.ticked = now
	a.refresh()
	return a.frame != nil && a.frame.gen != a.paintedGen || a.replay != nil && a.replay.playing
}

// Routes a message to the floor, the pointer handlers and the actions
func (a *arena) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if a.closed {
		return a, nil
	}
	var cmd tea.Cmd
	switch m := msg.(type) {
	case frameMsg:
		if a.frame == nil || m.f.gen > a.frame.gen {
			a.frame = m.f
		}
	case wm.ActionMsg:
		cmd = a.act(m.ID)
	case tea.MouseClickMsg:
		cmd = a.click(m)
	case tea.MouseReleaseMsg:
		cmd = a.release(m)
	case tea.MouseMotionMsg:
		cmd = a.motion(m)
	case tea.MouseWheelMsg:
		cmd = a.wheel(m)
	case tea.KeyPressMsg:
		if a.showHelp {
			a.showHelp = false
		}
	case tea.FocusMsg:
		a.stage.post(func(s *stage) { s.focus(true) })
	case tea.BlurMsg:
		a.stage.post(func(s *stage) { s.focus(false) })
	case wm.ClosedMsg:
		a.close()
		return a, nil
	}
	a.refresh()
	return a, cmd
}

// Stops the worker and takes the image off the terminal once the card closed
func (a *arena) close() {
	if a.closed {
		return
	}
	a.closed = true
	a.releaseImage()
	a.stage.halt()
}

// Drops the image surface, telling the terminal to forget its images
func (a *arena) releaseImage() {
	if a.surface == nil {
		a.proto = gfx.None
		return
	}
	if text := a.surface.Release(); text != "" && a.env.Send != nil {
		a.env.Send(tea.RawMsg{Msg: text})
	}
	a.surface, a.proto = nil, gfx.None
	a.stage.post(func(s *stage) { s.setSurface(nil) })
}

// Protocol the floor goes out in, images only where the settings ask for them and the terminal draws them
func (a *arena) protocol() gfx.Protocol {
	if a.env.Graphics == nil || a.off() {
		return gfx.None
	}
	switch a.cfg.Arena.Display {
	case "glyphs":
		return gfx.None
	case "image":
		return a.env.Graphics.Guess()
	}
	if p := a.env.Graphics.Protocol(); p == gfx.KittyCells {
		return p
	}
	return gfx.None
}

// Turn shown when following the newest, none in the lobby once the newest ended past its finale
func (a *arena) followed(now time.Time) string {
	if len(a.sprints) == 0 {
		return ""
	}
	id := a.sprints[len(a.sprints)-1]
	running, ended, ok := a.j.Status(id)
	if !ok || !running && !ended.IsZero() && now.Sub(ended) > finaleHold {
		return ""
	}
	return id
}

// Tape of a turn, read from the journal again only when it changed, a running one reaching to now
func (a *arena) tape(id string) (tape, bool) {
	rev := a.j.Revision()
	if a.tapeMemo.id != id || a.tapeMemo.rev != rev {
		t, ok := a.j.Tape(id)
		if !ok {
			return tape{}, false
		}
		running, _, _ := a.j.Status(id)
		a.tapeMemo = tapeMemo{id: id, rev: rev, running: running, t: t}
	}
	t := a.tapeMemo.t
	if now := time.Now(); a.tapeMemo.running && now.After(t.end) {
		t.end = now
	}
	return t, true
}

// Pulls the turn the arena shows from the journal and sends it to the floor when it changed
func (a *arena) refresh() {
	if a.j == nil || a.closed {
		return
	}
	now := time.Now()
	rev := a.j.Revision()
	if rev != a.seenRev || a.sprints == nil {
		a.seenRev = rev
		a.sprints = a.j.Sprints(a.j.Session())
		a.sprintCount = len(a.sprints)
	}
	if a.sprintID != "" && !slices.Contains(a.sprints, a.sprintID) {
		a.sprintID = ""
	}
	id := a.sprintID
	if id == "" {
		id = a.followed(now)
	}
	if a.replay != nil && a.replay.sprint != id {
		a.replay = nil
	}
	key := fmt.Sprintf("%d|%s", rev, id)
	if r := a.replay; r != nil {
		mark := r.cursor.UnixNano()
		if t, ok := a.tape(r.sprint); ok {
			mark = int64(sort.Search(len(t.times), func(i int) bool { return t.times[i].After(r.cursor) }))
		}
		key += fmt.Sprintf("|%d|%d", r.epoch, mark)
	}
	if key == a.posted {
		return
	}
	a.posted = key
	var snap *Snapshot
	var live map[string]liveState
	if id != "" {
		at := time.Time{}
		if a.replay != nil {
			at = a.replay.cursor
		}
		snap, live, _ = a.j.View(id, at)
	}
	if snap != nil {
		if a.replay != nil {
			snap.ID = fmt.Sprintf("%s@%d", snap.Sprint, a.replay.epoch)
			if a.replay.epochSeq < 0 {
				a.replay.epochSeq = len(snap.Messages)
			}
			snap.Since = a.replay.epochSeq
		} else {
			if _, seen := a.since[snap.Sprint]; !seen {
				a.since[snap.Sprint] = len(snap.Messages)
			}
			snap.Since = a.since[snap.Sprint]
		}
	}
	if snap == nil || a.snap == nil || snap.Sprint != a.snap.Sprint {
		a.selTicket, a.hoverTicket = "", ""
	}
	a.snap, a.live = snap, live
	if _, ok := snap.Member(a.sel); !ok {
		a.sel = ""
	}
	sent := snap
	lv := live
	a.stage.post(func(s *stage) { s.sync(sent, lv) })
	a.markFloor()
}

// Member selected, false while none is
func (a *arena) selected() (Member, bool) { return a.snap.Member(a.sel) }

// Selects a member, opening its inspector when the inspector is on
func (a *arena) selectMember(name string) tea.Cmd {
	if _, ok := a.snap.Member(name); !ok {
		return nil
	}
	a.sel, a.selTicket = name, ""
	a.markFloor()
	return nil
}

// Shows or hides the inspector, selecting the director when nobody is selected
func (a *arena) toggleInspect() tea.Cmd {
	if a.sel == "" && a.snap != nil && len(a.snap.Members) > 0 {
		a.sel = a.snap.Members[0].Name
		a.markFloor()
		a.inspect = true
		return nil
	}
	a.inspect = !a.inspect
	return nil
}

// Tells the floor what is selected and hovered, for its rings and the follow camera
func (a *arena) markFloor() {
	sel, hover, st, ht := a.sel, a.hover, a.selTicket, a.hoverTicket
	a.stage.post(func(s *stage) { s.mark(sel, hover, st, ht) })
}

// Reports whether a cell lies on the dock
func (a *arena) onDock(x, y int) bool { return a.lay.dock.w > 0 && a.lay.dock.contains(x, y) }

// Id of the painted thing under a cell, the newest painted first
func (a *arena) hitAt(x, y int) (string, bool) {
	for i := len(a.hits) - 1; i >= 0; i-- {
		if a.hits[i].r.contains(x, y) {
			return a.hits[i].id, true
		}
	}
	return "", false
}

// Folds or opens a section of the dock
func (a *arena) toggleSection(s section) tea.Cmd {
	a.secs.Toggle(s.String())
	return nil
}

// Scrolls a section of the dock by a count of rows
func (a *arena) scrollSection(id string, step int) {
	a.secs.Scroll(id, step, id == secLog.String())
}

// Sheet of a member, made on first use and kept so its scroll stays
func (a *arena) sessionOf(name string) *sheet {
	if _, ok := a.snap.Member(name); !ok {
		return nil
	}
	s := a.sheets[name]
	if s == nil {
		s = &sheet{a: a, name: name}
		a.sheets[name] = s
	}
	return s
}

// Sheet of the selected member, nil while none is selected
func (a *arena) inspected() *sheet { return a.sessionOf(a.sel) }

// Moves the selection along the crew by a step
func (a *arena) cycle(step int) tea.Cmd {
	if a.snap == nil || len(a.snap.Members) == 0 {
		return nil
	}
	i := slices.IndexFunc(a.snap.Members, func(m Member) bool { return m.Name == a.sel })
	n := len(a.snap.Members)
	switch {
	case i < 0 && step > 0:
		i = 0
	case i < 0:
		i = n - 1
	default:
		i = ((i+step)%n + n) % n
	}
	return a.selectMember(a.snap.Members[i].Name)
}

// Shows the turn of the session a step along, past the newest back to following it
func (a *arena) browse(step int) tea.Cmd {
	if len(a.sprints) == 0 {
		return nil
	}
	cur := slices.Index(a.sprints, a.sprintID)
	if cur < 0 {
		if a.snap != nil {
			cur = slices.Index(a.sprints, a.snap.Sprint)
		} else {
			cur = len(a.sprints)
		}
	}
	next := cur + step
	switch {
	case next < 0:
		next = 0
	case next >= len(a.sprints):
		a.sprintID, a.replay = "", nil
		return nil
	}
	a.sprintID, a.replay = a.sprints[next], nil
	return nil
}

// Turn the view settings change on, the shown one
func (a *arena) viewConfig(change func(c *ArenaConfig)) {
	cfg := a.cfg
	change(&cfg.Arena)
	a.Configure(cfg, a.cfgErr)
}

// Next value of a list after the current one, the first after the last
func nextOf(list []string, cur string) string {
	i := slices.Index(list, cur)
	return list[(i+1)%len(list)]
}

// Carries out an action of the arena
func (a *arena) act(id string) tea.Cmd {
	s := a.snap
	switch id {
	case ActHelp:
		a.showHelp = !a.showHelp
	case ActLeave:
		switch {
		case a.showHelp:
			a.showHelp = false
		case a.inspect && a.sel != "":
			a.inspect = false
		default:
			a.sel, a.selTicket = "", ""
			a.markFloor()
		}
	case ActNext:
		return a.cycle(1)
	case ActPrev:
		return a.cycle(-1)
	case ActInspect:
		return a.toggleInspect()
	case ActSidebar:
		a.dock = !a.dock
	case ActBoard:
		return a.toggleSection(secTickets)
	case ActLog:
		return a.toggleSection(secLog)
	case ActPageDown, ActPageUp:
		step := max(a.lay.world.h-2, 4)
		if id == ActPageUp {
			step = -step
		}
		if sh := a.inspected(); sh != nil && a.lay.insp.w > 0 {
			sh.scrollBy(step)
		} else {
			a.scrollSection(secLog.String(), -step)
		}
	case ActView:
		return wm.RequestOverlay(a.viewPalette())
	case ActWorld:
		a.viewConfig(func(c *ArenaConfig) { c.World = nextOf(worlds, c.World) })
	case ActCamera:
		a.viewConfig(func(c *ArenaConfig) { c.Camera = nextOf(cameras, c.Camera) })
	case ActFollow:
		a.viewConfig(func(c *ArenaConfig) {
			if c.Camera == "follow" {
				c.Camera = "overview"
			} else {
				c.Camera = "follow"
			}
		})
		if a.sel == "" {
			return a.cycle(1)
		}
	case ActReset:
		a.stage.post(func(s *stage) { s.resetView() })
	case ActOrbitLeft:
		a.stage.post(func(s *stage) { s.orbit(-0.2, 0) })
	case ActOrbitRight:
		a.stage.post(func(s *stage) { s.orbit(0.2, 0) })
	case ActOrbitUp:
		a.stage.post(func(s *stage) { s.orbit(0, -0.1) })
	case ActOrbitDown:
		a.stage.post(func(s *stage) { s.orbit(0, 0.1) })
	case ActZoomIn:
		a.stage.post(func(s *stage) { s.zoom(0.9) })
	case ActZoomOut:
		a.stage.post(func(s *stage) { s.zoom(1.11) })
	case ActLabels:
		a.labels = !a.labels
	case ActPause:
		if s == nil || !s.Running() {
			return wm.Notify("spawning pauses for the turn shown while it runs, it does not run now", wm.Info)
		}
		return a.ops.Pause(s.Paused == "")
	case ActStop:
		if s == nil || !s.Running() || a.replay != nil {
			return wm.Notify("the turn shown is not running", wm.Info)
		}
		return a.ops.Stop(s)
	case ActRestart:
		if !s.Restartable() {
			return wm.Notify("only a failed or stopped newest turn can be retried", wm.Info)
		}
		return a.ops.Retry()
	case ActSprintNext:
		return a.browse(1)
	case ActSprintPrev:
		return a.browse(-1)
	case ActSteer:
		return a.steer()
	case ActOpen:
		m, ok := a.selected()
		if !ok {
			return wm.Notify("select a member first", wm.Info)
		}
		return a.ops.Open(s, m)
	case ActKill:
		m, ok := a.selected()
		switch {
		case !ok:
			return wm.Notify("select a member first", wm.Info)
		case a.replay != nil || !s.Running():
			return wm.Notify("the turn shown is not running", wm.Info)
		case m.Role != RoleDirector && !activeStatus(m.Status):
			return wm.Notify(m.Name+" already finished", wm.Info)
		}
		return a.ops.Interrupt(s, m)
	case ActReplay, ActStepBack, ActStepForward, ActSpeed, ActLive:
		return a.replayAct(id)
	default:
		if strings.HasPrefix(id, "view:") {
			return a.viewChoice(strings.TrimPrefix(id, "view:"))
		}
		if turn, ok := strings.CutPrefix(id, "turn:"); ok {
			a.sprintID, a.replay = turn, nil
		}
	}
	return nil
}

// Prompt for guidance to the selected member, a running subagent or the main agent
func (a *arena) steer() tea.Cmd {
	m, ok := a.selected()
	switch {
	case !ok:
		return wm.Notify("select a member first", wm.Info)
	case a.replay != nil:
		return wm.Notify("guidance reaches the turn as it stands now, leave the replay first", wm.Info)
	case m.Role != RoleDirector && m.Status != "running":
		return wm.Notify(m.Name+" is not running, guidance only reaches running agents", wm.Info)
	}
	s := a.snap
	label := "guidance for " + m.Name
	if m.Role == RoleDirector && !s.Running() {
		label = "prompt for " + m.Name
	}
	return wm.RequestOverlay(wm.NewPrompt(a.env.Theme, label, "", func(text string) tea.Cmd {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return a.ops.Steer(s, m, strings.TrimSpace(text))
	}))
}

// Palette of the view settings this arena draws in, each applied at once
func (a *arena) viewPalette() *wm.Palette {
	c := a.cfg.Arena
	var items []wm.Item
	add := func(group string, list []string, cur string) {
		for _, v := range list {
			title := group + " " + v
			if v == cur {
				title = "● " + title
			}
			id := "view:" + group + ":" + v
			items = append(items, wm.Item{ID: id, Title: title, Run: func() tea.Cmd { return a.act(id) }})
		}
	}
	add("display", displays, c.Display)
	add("glyphs", glyphSets, c.Glyphs)
	add("world", worlds, c.World)
	add("quality", qualities, c.Quality)
	add("camera", cameras, c.Camera)
	p := wm.NewPalette(a.env.Theme, items)
	p.Placeholder = "arena view"
	return p
}

// Applies one choice of the view palette
func (a *arena) viewChoice(choice string) tea.Cmd {
	group, value, _ := strings.Cut(choice, ":")
	a.viewConfig(func(c *ArenaConfig) {
		switch group {
		case "display":
			c.Display = value
		case "glyphs":
			c.Glyphs = value
		case "world":
			c.World = value
		case "quality":
			c.Quality = value
		case "camera":
			c.Camera = value
		}
	})
	if note := a.displayNote(); note != "" {
		return wm.Notify(note, wm.Warn)
	}
	return nil
}

// Words on images asked for that the terminal cannot draw, empty while the floor goes out as asked
func (a *arena) displayNote() string {
	if a.cfg.Arena.Display != "image" || a.off() || a.protocol() != gfx.None {
		return ""
	}
	return "this terminal answered no image protocol the arena draws, the floor goes out in glyphs"
}

var (
	_ Card          = (*arena)(nil)
	_ wm.Positioned = (*arena)(nil)
	_ wm.Animator   = (*arena)(nil)
	_ wm.Tracker    = (*arena)(nil)
	_ wm.Grabber    = (*arena)(nil)
	_ wm.Statuser   = (*arena)(nil)
	_ wm.Hinter     = (*arena)(nil)
	_ wm.Contexter  = (*arena)(nil)
	_ wm.Keyed      = (*arena)(nil)
	_ wm.Glypher    = (*arena)(nil)
	_ wm.Sized      = (*arena)(nil)
	_ wm.Selectable = (*arena)(nil)
)
