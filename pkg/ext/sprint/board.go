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
		Title:   "SPRINT",
		Glyph:   env.Theme.Glyphs.Plan,
		Key:     "ext:sprint:board",
		Context: Context,
		Primary: ActionOpen,
		Columns: []render.Column{{Title: "KIND", Max: 10}, {Title: "NAME", Max: 28}, {Title: "STATE", Max: 14}, {Title: "DETAIL", Flex: 1}},
		Rows:    b.rows,
		Detail:  b.detail,
		Actions: map[string]func(*cards.List, int) tea.Cmd{ActionOpen: b.open, ActionStop: b.stop, ActionResume: b.resume, ActionSettings: b.settings},
		Free:    map[string]bool{ActionSettings: true},
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

// Refreshes on harness traffic and ticks, handing everything else to the table
func (b *board) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg, harness.FlowDoneMsg, harness.NoticeMsg, harness.OpenedMsg:
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
	total := time.Duration(Current().Limits.Total)
	var out [][]string
	for si, s := range b.snaps {
		done, n := s.Progress()
		detail := span(s.Elapsed()) + " of " + span(total) + sep + fmt.Sprintf("%d/%d tickets", done, n)
		if s.Status != "" {
			detail = s.Status + sep + detail
		}
		state := b.stage(s.Stage)
		if s.Running() && s.Paused != "" {
			state = render.Warm("paused", t)
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
			out = append(out, []string{m.Role, m.Name, state, sessionName(m.Live)})
			b.refs = append(b.refs, rowRef{kind: rowMember, snap: si, index: mi})
		}
		for ti, tk := range s.Tickets {
			var parts []string
			for _, p := range []string{tk.Engineer, tk.Estimate} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			out = append(out, []string{"ticket", render.Truncate(tk.ID+" "+tk.Title, 28, t), b.ticketState(tk.Status), strings.Join(parts, sep)})
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
		return render.Cool(string(s), t)
	case TicketDone:
		return render.Good(string(s), t)
	case TicketRework:
		return render.Warm(string(s), t)
	case TicketFailed:
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
		if value == "" {
			return
		}
		lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
		out = append(out, render.KV(key, lines[0], t))
		out = append(out, lines[1:]...)
	}
	switch r.kind {
	case rowSprint:
		add("prompt", s.Prompt)
		add("objective", s.Objective.Objective)
		for _, a := range s.Objective.Acceptance {
			add("acceptance", a)
		}
		for _, c := range s.Objective.Constraints {
			add("constraint", c)
		}
		add("notes", s.Objective.Notes)
		add("story", s.Story.Title)
		add("story summary", s.Story.Summary)
		add("size", s.Story.Size)
		add("review round", strconv.Itoa(s.Round))
		add("summary", s.Summary)
		if s.Running() && s.Paused != "" {
			add("paused", s.Paused)
			add("go on", "press r once it is fixed, or x to stop the sprint")
		}
		if s.Running() && s.Waiting != "" {
			add("waiting", "open the session of "+s.Waiting+" and tell it to go on, or press r to send it on")
		}
		if s.Err != nil {
			add("error", s.Err.Error())
		}
	case rowMember:
		m := s.Members[r.index]
		add("role", m.Role)
		if m.Live != nil && m.Live.Session != nil {
			add("session", m.Live.Session.ID())
		}
		if !m.Started.IsZero() {
			add("started", m.Started.Local().Format("15:04:05"))
		}
	case rowTicket:
		tk := s.Tickets[r.index]
		add("description", tk.Description)
		add("files", strings.Join(tk.Files, ", "))
		add("depends on", strings.Join(tk.DependsOn, ", "))
		add("engineer", tk.Engineer)
		if tk.Round > 0 {
			add("review rounds", strconv.Itoa(tk.Round))
		}
		for n, note := range tk.Notes {
			add("review note "+strconv.Itoa(n+1), note)
		}
		add("report", tk.Report)
		add("error", tk.Err)
	}
	return out
}

// Opens the session of a member row and toggles the detail of a sprint or ticket row
func (b *board) open(l *cards.List, i int) tea.Cmd {
	r, s, ok := b.ref(i)
	if !ok {
		return nil
	}
	if r.kind != rowMember {
		return l.ToggleDetail()
	}
	m := s.Members[r.index]
	if m.Live == nil {
		return wm.Notify(m.Name+" has no session", wm.Warn)
	}
	live, flow := m.Live, s.ID
	return func() tea.Msg { return harness.OpenedMsg{Live: live, Flow: flow} }
}

// Cancels the workflow of the sprint the row belongs to, refusing one that has ended
func (b *board) stop(l *cards.List, i int) tea.Cmd {
	_, s, ok := b.ref(i)
	if !ok {
		return nil
	}
	if !s.Running() {
		return wm.Notify("sprint "+s.ID+" is not running", wm.Info)
	}
	if b.env.Editor == nil || b.env.Editor.Flows == nil {
		return wm.Notify("cannot stop sprint "+s.ID+", the app runs no workflows", wm.Error)
	}
	if err := b.env.Editor.Flows.Cancel(s.ID); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	l.Refresh()
	return wm.Notify("stopping sprint "+s.ID, wm.Success)
}

// Tells the sprint of the row to go on, refusing one that is not waiting for the person
func (b *board) resume(l *cards.List, i int) tea.Cmd {
	_, snap, ok := b.ref(i)
	if !ok {
		return nil
	}
	s := sprintByID(snap.ID)
	if s == nil || !s.Resume() {
		return wm.Notify("sprint "+snap.ID+" is not waiting for you", wm.Info)
	}
	l.Refresh()
	return wm.Notify("sprint "+snap.ID+" told to go on", wm.Success)
}

// Opens the settings panel of the extension
func (b *board) settings(*cards.List, int) tea.Cmd {
	return func() tea.Msg { return cards.OpenPanelMsg{Key: SettingsKey} }
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

// Duration in whole hours and minutes, in seconds while under a minute
func span(d time.Duration) string {
	d = max(d, 0)
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}
