package sprint

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What a board row stands for
type rowKind int

const (
	rowSprint rowKind = iota
	rowMember
	rowTicket
)

// Row of the board as its kind, the snapshot it belongs to and the member or ticket index
type rowRef struct {
	kind  rowKind
	snap  int
	index int
}

// Tick the board schedules while a sprint runs, naming the board and its loop
type boardTickMsg struct {
	owner *board
	gen   int
}

// Panel listing every sprint with its team and tickets, refreshed every second while one runs
type board struct {
	*cards.List
	env   *cards.Env
	snaps []Snapshot
	refs  []rowRef
	gen   int
	armed bool
}

// Builds the sprint board panel
func newBoard(env *cards.Env) wm.Card {
	b := &board{env: env}
	b.List = cards.NewList(env, cards.ListSpec{
		Title:   "SPRINTS",
		Glyph:   env.Theme.Glyphs.Plan,
		Key:     BoardKey,
		Context: ContextBoard,
		Primary: BoardOpen,
		Columns: []render.Column{{Title: "KIND", Max: 10}, {Title: "NAME", Max: 30}, {Title: "STATE", Max: 16}, {Title: "DETAIL", Flex: 1}},
		Rows:    b.rows,
		Detail:  b.detail,
		Actions: map[string]func(*cards.List, int) tea.Cmd{BoardOpen: b.open, BoardArena: b.arena, BoardStop: b.stop, BoardPause: b.pause, BoardResume: b.resume, BoardForget: b.forget, BoardSettings: b.settings},
		Free:    map[string]bool{BoardSettings: true, BoardArena: true},
		Hints:   b.hints,
		Empty:   func() string { return "no sprint yet, run /" + Name + " <request> to start one" },
	})
	return b
}

// Refreshes the rows and starts the clock afresh, any tick pending from before the card closed abandoned
func (b *board) Init() tea.Cmd {
	b.List.Init()
	b.armed = false
	return b.tick()
}

// Refreshes on sprint changes, harness traffic and ticks, handing everything else to the table
func (b *board) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case ChangedMsg, harness.FlowDoneMsg, harness.NoticeMsg, harness.RunDoneMsg:
		b.Refresh()
		return b, b.tick()
	case boardTickMsg:
		if msg.owner != b || msg.gen != b.gen || !b.armed {
			return b, nil
		}
		b.armed = false
		b.Refresh()
		return b, b.tick()
	}
	_, cmd := b.List.Update(msg)
	return b, tea.Batch(cmd, b.tick())
}

// Schedules the next refresh a second out while a sprint runs and none is pending
func (b *board) tick() tea.Cmd {
	if b.armed || b.running() == 0 {
		return nil
	}
	b.gen++
	b.armed = true
	owner, gen := b, b.gen
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return boardTickMsg{owner: owner, gen: gen} })
}

// Count of sprints still going as of the last refresh
func (b *board) running() int {
	n := 0
	for _, s := range b.snaps {
		if s.Running() {
			n++
		}
	}
	return n
}

// Trailer naming how many sprints run, idle when none does
func (b *board) Status() string {
	if n := b.running(); n > 0 {
		return strconv.Itoa(n) + " running"
	}
	return "idle"
}

// Every sprint newest first, each followed by its members and tickets, with the descriptor of every row kept
func (b *board) rows() [][]string {
	t := b.env.Theme
	sep := " " + t.Glyphs.Sep + " "
	b.snaps = Sprints()
	b.refs = b.refs[:0]
	busy := b.busy()
	var out [][]string
	for si, s := range b.snaps {
		done, n := s.Progress()
		detail := render.Elapsed(s.Elapsed()) + sep + fmt.Sprintf("%d/%d tickets", done, n)
		if s.Status != "" {
			detail = render.Truncate(s.Status, 60, t) + sep + detail
		}
		state := b.stage(s.Stage)
		switch {
		case s.Running() && s.Paused != "":
			state = render.Warm("paused", t)
		case s.Exited():
			state = render.Warm("interrupted", t)
		case s.Stopped():
			state = render.Warm("stopped", t)
		}
		out = append(out, []string{"sprint", s.ID, state, detail})
		b.refs = append(b.refs, rowRef{kind: rowSprint, snap: si})
		for mi, m := range s.Members {
			state := render.Dim("idle", t)
			switch {
			case m.Live != nil && busy[m.Live.Session]:
				state = render.Cool("running", t)
			case s.Running() && s.Waiting == m.Name:
				state = render.Warm("waiting for you", t)
			}
			detail := m.Task
			if detail == "" {
				detail = sessionName(m.Live)
			}
			out = append(out, []string{string(m.Role), m.Name, state, detail})
			b.refs = append(b.refs, rowRef{kind: rowMember, snap: si, index: mi})
		}
		for ti, tk := range s.Tickets {
			var parts []string
			for _, p := range []string{tk.Builder, tk.Size} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			if tk.Round > 0 {
				parts = append(parts, fmt.Sprintf("sent back %d", tk.Round))
			}
			out = append(out, []string{"ticket", render.Truncate(tk.ID+" "+tk.Title, 30, t), b.ticketState(tk.Status), strings.Join(parts, sep)})
			b.refs = append(b.refs, rowRef{kind: rowTicket, snap: si, index: ti})
		}
	}
	return out
}

// Sessions a root run is going on now
func (b *board) busy() map[*session.Session]bool {
	out := map[*session.Session]bool{}
	if b.env.Control == nil {
		return out
	}
	for _, r := range b.env.Control.Runs() {
		if r.Running() && r.Parent == "" && r.Session != nil {
			out[r.Session] = true
		}
	}
	return out
}

// Stage colored cool while it runs, good once done and bad once failed
func (b *board) stage(s Stage) string {
	t := b.env.Theme
	switch s {
	case StageDone:
		return render.Good(string(s), t)
	case StageFailed:
		return render.Bad(string(s), t)
	}
	return render.Cool(string(s), t)
}

// Ticket status colored by where the ticket stands
func (b *board) ticketState(s TicketStatus) string {
	t := b.env.Theme
	switch s {
	case TicketActive:
		return render.Warm(string(s), t)
	case TicketReview:
		return render.Cool(string(s), t)
	case TicketDone:
		return render.Good(string(s), t)
	case TicketFailed, TicketBlocked:
		return render.Bad(string(s), t)
	}
	return render.Dim(string(s), t)
}

// Descriptor and snapshot of a row, false for an index past the rows
func (b *board) ref(i int) (rowRef, Snapshot, bool) {
	if i < 0 || i >= len(b.refs) {
		return rowRef{}, Snapshot{}, false
	}
	r := b.refs[i]
	return r, b.snaps[r.snap], true
}

// Detail lines of the sprint, member or ticket a row stands for
func (b *board) detail(i int) []string {
	r, s, ok := b.ref(i)
	if !ok {
		return nil
	}
	t := b.env.Theme
	var out []string
	add := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
		out = append(out, render.KV(key, lines[0], t))
		out = append(out, lines[1:]...)
	}
	switch r.kind {
	case rowSprint:
		add("request", s.Request)
		add("objective", s.Brief.Objective)
		for _, a := range s.Brief.Acceptance {
			add("acceptance", a)
		}
		for _, c := range s.Brief.Constraints {
			add("constraint", c)
		}
		for _, o := range s.Brief.Untouched {
			add("leave alone", o)
		}
		add("notes", s.Brief.Notes)
		add("plan", s.Plan.Title)
		add("summary", s.Plan.Summary)
		add("approach", s.Plan.Approach)
		for _, risk := range s.Plan.Risks {
			add("risk", risk)
		}
		add("report", s.Summary)
		if s.Running() && s.Paused != "" {
			add("paused", s.Paused)
			add("resume", "press p to resume, or x to stop the sprint")
		}
		if s.Running() && s.Waiting != "" {
			add("waiting", "answer "+s.Waiting+" in the arena")
		}
		if s.Err != nil && !s.Stopped() {
			add("error", s.Err.Error())
		}
		if s.Restartable() {
			add("restart", fmt.Sprintf("press r to restart at the %s stage", restartStage(s)))
		}
	case rowMember:
		m := s.Members[r.index]
		add("role", string(m.Role))
		add("session", m.SessionID())
		add("task", m.Task)
		if n := s.Spent[m.Name]; n > 0 {
			add("spent", render.Tokens(n)+" tokens")
		}
		if !m.Joined.IsZero() {
			add("joined", m.Joined.Local().Format("15:04:05"))
		}
	case rowTicket:
		tk := s.Tickets[r.index]
		add("description", tk.Description)
		add("files", strings.Join(tk.Files, ", "))
		add("depends on", strings.Join(tk.DependsOn, ", "))
		add("builder", tk.Builder)
		for n, note := range tk.Notes {
			add("reviewer note "+strconv.Itoa(n+1), note)
		}
		add("handoff", tk.Report)
		add("error", tk.Err)
	}
	return out
}

// Opens the session of a member row, stored or open, and toggles the detail of a sprint or ticket row
func (b *board) open(l *cards.List, i int) tea.Cmd {
	r, s, ok := b.ref(i)
	if !ok {
		return nil
	}
	if r.kind != rowMember {
		return l.ToggleDetail()
	}
	return openMember(b.env, s.Members[r.index], s.ID)
}

// Opens the session of a member as a window, loading the stored one when none is open
func openMember(env *cards.Env, m Member, flow string) tea.Cmd {
	if env.Editor == nil {
		return wm.Notify("the app holds no sessions", wm.Error)
	}
	live := m.Live
	if live == nil || env.Editor.LiveOf(live.Session) == nil {
		if m.Session == "" {
			return wm.Notify(m.Name+" has no session yet", wm.Info)
		}
		opened, err := env.Editor.Open(env.Ctx, m.Session)
		if err != nil {
			return wm.Notify("the session of "+m.Name+" could not be opened, "+err.Error(), wm.Error)
		}
		live = opened
	}
	return func() tea.Msg { return harness.OpenedMsg{Live: live, Flow: flow} }
}

// Shows the sprint of the row in the arena
func (b *board) arena(l *cards.List, i int) tea.Cmd {
	open := func() tea.Msg { return cards.OpenPanelMsg{Key: ArenaKey} }
	if _, s, ok := b.ref(i); ok {
		return tea.Sequence(open, func() tea.Msg { return ShowMsg{ID: s.ID} })
	}
	return open
}

// Cancels the workflow of the sprint the row belongs to, saying so when it has ended already
func (b *board) stop(l *cards.List, i int) tea.Cmd {
	_, s, ok := b.ref(i)
	if !ok {
		return nil
	}
	if !s.Running() {
		return wm.Notify(s.ID+" already ended, press r to restart it", wm.Info)
	}
	if b.env.Editor == nil || b.env.Editor.Flows == nil {
		return wm.Notify("cannot stop "+s.ID+", the app runs no workflows", wm.Error)
	}
	if err := b.env.Editor.Flows.Cancel(s.ID); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	l.Refresh()
	return wm.Notify("stopping "+s.ID+", press r to restart it later", wm.Warn)
}

// Pauses the sprint of the row before its next step, or resumes it when paused
func (b *board) pause(l *cards.List, i int) tea.Cmd {
	_, snap, ok := b.ref(i)
	if !ok {
		return nil
	}
	s := sprintByID(snap.ID)
	switch {
	case s == nil:
		return wm.Notify(snap.ID+" is no longer known", wm.Warn)
	case !snap.Running():
		return wm.Notify(snap.ID+" is not running", wm.Info)
	case snap.Paused != "" && s.Resume():
		l.Refresh()
		return wm.Notify(snap.ID+" resumes", wm.Success)
	case s.Pause():
		l.Refresh()
		return wm.Notify(snap.ID+" pauses before its next step", wm.Warn)
	}
	return nil
}

// Restarts a sprint that ended short of done at the stage it was in
func (b *board) resume(l *cards.List, i int) tea.Cmd {
	_, snap, ok := b.ref(i)
	if !ok {
		return nil
	}
	s := sprintByID(snap.ID)
	switch {
	case s == nil:
		return wm.Notify(snap.ID+" is no longer known", wm.Warn)
	case snap.Running() && snap.Paused != "":
		return b.pause(l, i)
	case snap.Running():
		return wm.Notify(snap.ID+" is running", wm.Info)
	case !snap.Restartable():
		return wm.Notify(snap.ID+" is done, start another with /"+Name+" <request>", wm.Info)
	case b.env.Editor == nil || b.env.Editor.Flows == nil:
		return wm.Notify("cannot restart "+snap.ID+", the app runs no workflows", wm.Error)
	}
	if _, err := b.env.Editor.Flows.Restart(b.env.Ctx, snap.ID, workflow(), nil, snap.Request); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	l.Refresh()
	return wm.Notify(fmt.Sprintf("%s restarts at the %s stage", snap.ID, restartStage(snap)), wm.Success)
}

// Forgets an ended sprint, closing and storing the sessions of its team
func (b *board) forget(l *cards.List, i int) tea.Cmd {
	_, snap, ok := b.ref(i)
	if !ok {
		return nil
	}
	if snap.Running() {
		return wm.Notify(snap.ID+" is running, stop it first", wm.Warn)
	}
	released, err := Forget(b.env.Editor, snap.ID)
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	l.Refresh()
	return wm.Notify(fmt.Sprintf("forgot %s, %d sessions stored", snap.ID, released), wm.Success)
}

// Opens the settings panel of the extension
func (b *board) settings(*cards.List, int) tea.Cmd {
	return func() tea.Msg { return cards.OpenPanelMsg{Key: SettingsKey} }
}

// Legend of the row under the cursor, the keys that apply to the sprint in its state
func (b *board) hints(_ *cards.List, i int) []wm.Hint {
	out := []wm.Hint{{Action: BoardArena}, {Action: BoardSettings}}
	r, s, ok := b.ref(i)
	if !ok {
		return out
	}
	open := wm.Hint{Action: BoardOpen, Label: "details"}
	if r.kind == rowMember {
		open.Label = "open session"
	}
	out = append([]wm.Hint{open}, out...)
	switch {
	case s.Running() && s.Paused != "":
		out = append(out, wm.Hint{Action: BoardPause, Label: "resume"}, wm.Hint{Action: BoardStop})
	case s.Running():
		out = append(out, wm.Hint{Action: BoardPause}, wm.Hint{Action: BoardStop})
	case s.Restartable():
		out = append(out, wm.Hint{Action: BoardResume, Label: "restart"}, wm.Hint{Action: BoardForget})
	default:
		out = append(out, wm.Hint{Action: BoardForget})
	}
	return out
}

// Title of the session, its id while it has none
func sessionName(l *harness.Live) string {
	if l == nil || l.Session == nil {
		return ""
	}
	if title := l.Session.Title(); title != "" {
		return title
	}
	return l.Session.ID()
}

// Asks the arena to show a sprint
type ShowMsg struct {
	ID string
}

// Drops an ended sprint from the registry and its file, closing the open sessions of its team, reporting how many
func Forget(app *harness.App, id string) (int, error) {
	s := sprintByID(id)
	if s == nil {
		return 0, fmt.Errorf("no sprint %s is known", id)
	}
	if s.Snapshot().Running() {
		return 0, fmt.Errorf("%s is running", id)
	}
	released := 0
	for _, m := range s.Snapshot().Members {
		if m.Live == nil || app == nil || app.LiveOf(m.Live.Session) == nil {
			continue
		}
		if err := app.Release(m.Live); err != nil {
			return released, err
		}
		released++
	}
	if err := s.unkeep(); err != nil {
		return released, err
	}
	regMu.Lock()
	defer regMu.Unlock()
	for i, have := range sprints {
		if have == s {
			sprints = append(sprints[:i], sprints[i+1:]...)
			break
		}
	}
	return released, nil
}
