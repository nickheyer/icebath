package arena

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Crew rows the sprint section shows before it folds the rest behind a row that shows them all
const crewRows = 6

// Sections of the dock as last built, kept until what they show changes
type dockCache struct {
	key  string
	secs []cards.Section
}

// Column of the dock its sections, the resize handle at its right edge
func (a *arena) paintDock(p *cards.Painter) {
	db := a.lay.dock
	t := a.env.Theme
	p.Fill(db.x, db.y, db.w, db.h, t.Surface)
	inner := db.w - 2
	a.secs.Hover = a.hoverHit
	selBg, hoverBg := theme.Mix(t.Surface, t.Ice, 0.18), theme.Mix(t.Surface, t.Steel, 0.45)
	a.secs.Paint(p, db.x+1, db.y, inner, db.h, a.dockSections(inner), func(id string, x, y, w, h int) {
		a.hits = append(a.hits, hit{id: id, r: rect{x, y, w, h}})
	}, func(id string) color.Color {
		kind, rest, _ := strings.Cut(id, ":")
		switch {
		case kind == "member" && rest == a.sel, kind == "ticket" && rest == a.selTicket, kind == "turn" && a.snap != nil && rest == a.snap.Sprint:
			return selBg
		case id == a.hoverHit:
			return hoverBg
		}
		return nil
	})
	p.Handle(db.x+db.w-1, db.y, db.h, a.hoverHit == "edge:dock" || a.drag != nil && a.drag.kind == dragDock)
}

// Sprint, ticket and log sections of the dock at a width, built again only when what they show changed
func (a *arena) dockSections(w int) []cards.Section {
	key := fmt.Sprintf("%p|%d|%s|%s|%v|%d|%d|%v|%s", a.snap, w, a.sel, a.selTicket, a.sprintFull, len(a.sprints), time.Now().Unix(), a.cfgErr, a.displayNote())
	if a.dockMemo.key == key {
		return a.dockMemo.secs
	}
	secs := []cards.Section{a.sprintSection(w), a.ticketSection(w), a.logSection(w)}
	a.dockMemo = dockCache{key: key, secs: secs}
	return secs
}

// Sprint section, the turn and a row per member, or the turns of the session in the lobby
func (a *arena) sprintSection(w int) cards.Section {
	t := a.env.Theme
	sec := cards.Section{ID: secSprint.String(), Title: "sprint"}
	add := func(id string, lines ...string) {
		if id != "" {
			sec.Rows = append(sec.Rows, cards.Row{ID: id, First: len(sec.Lines), N: len(lines)})
		}
		sec.Lines = append(sec.Lines, lines...)
	}
	if a.cfgErr != nil {
		for _, l := range cards.WrapStyled("settings: "+a.cfgErr.Error(), w, t.Alarm) {
			add("", l)
		}
	}
	if note := a.displayNote(); note != "" {
		for _, l := range cards.WrapStyled(note, w, t.Accent) {
			add("", l)
		}
	}
	s := a.snap
	if s == nil {
		sec.Title = "lobby"
		if len(a.sprints) == 0 {
			add("", render.Dim("no turn yet, the crew lands once the agent starts working", t))
			return sec
		}
		sec.Count = render.Count(len(a.sprints), "turn")
		add("", render.Dim("no turn running, pick one to replay it", t))
		for i := len(a.sprints) - 1; i >= 0; i-- {
			id := a.sprints[i]
			if snap, _, ok := a.j.View(id, time.Time{}); ok {
				add("turn:"+id, a.turnRow(snap, w))
			}
		}
		return sec
	}
	sec.Count = render.Count(len(s.Members), "member")
	title := a.turnLabel(s)
	if s.Title != "" {
		title += "  " + s.Title
	}
	head := lipgloss.NewStyle().Foreground(t.Text).Bold(true)
	if a.sprintFull {
		var lines []string
		for _, l := range cards.WrapIndent(title, w, 2) {
			lines = append(lines, head.Render(l))
		}
		add("sprint", lines...)
	} else {
		add("sprint", head.Render(ansi.Truncate(title, w, t.Glyphs.Ellipsis)))
	}
	add("sprint", a.turnState(s))
	if s.Waiting != "" {
		add("", render.Warm(t.Glyphs.Flag+" waiting on you, "+ansi.Truncate(s.Waiting, max(w-20, 8), t.Glyphs.Ellipsis), t))
	}
	if s.Paused != "" {
		add("", render.Warm(t.Glyphs.Flag+" "+s.Paused, t))
	}
	if s.Err != nil {
		for _, l := range cards.WrapStyled(t.Glyphs.Fail+" "+s.Err.Error(), w, t.Alarm) {
			add("", l)
		}
	}
	shown := s.Members
	if !a.sprintFull && len(shown) > crewRows {
		shown = shown[:crewRows]
	}
	for _, m := range shown {
		add("member:"+m.Name, a.crewRow(m, w))
	}
	if rest := len(s.Members) - len(shown); rest > 0 {
		add("sprint", render.Dim(fmt.Sprintf("%s %d more, show them all", t.Glyphs.Caret, rest), t))
	}
	return sec
}

// One row of the lobby list, a turn with how it ended, its crew and its prompt
func (a *arena) turnRow(s *Snapshot, w int) string {
	t := a.env.Theme
	mark := render.Good(t.Glyphs.Done, t)
	switch {
	case s.Running():
		mark = render.Cool(t.Glyphs.Live, t)
	case s.Err != nil:
		mark = render.Bad(t.Glyphs.Fail, t)
	case s.Stopped():
		mark = render.Warm(t.Glyphs.Skip, t)
	}
	end := s.Ended
	if s.Running() || end.IsZero() {
		end = time.Now()
	}
	meta := fmt.Sprintf("%s %s %s", a.turnLabel(s), outcomeWord(s), clockText(end.Sub(s.Started)))
	if n := len(s.Members) - 1; n > 0 {
		meta += " " + render.Count(n, "agent")
	}
	line := mark + " " + lipgloss.NewStyle().Foreground(t.Text).Render(meta)
	if s.Title != "" {
		line += "  " + render.Dim(s.Title, t)
	}
	return ansi.Truncate(line, w, t.Glyphs.Ellipsis)
}

// Stage, state, time and spend of the turn shown
func (a *arena) turnState(s *Snapshot) string {
	t := a.env.Theme
	var parts []string
	if s.Stage != "" {
		parts = append(parts, render.Cool(strings.ToUpper(string(s.Stage)), t))
	}
	switch word := outcomeWord(s); {
	case a.replay != nil:
		parts = append(parts, render.Cool("replay", t))
	case s.Running():
		parts = append(parts, render.Good(word, t))
	case s.Err != nil:
		parts = append(parts, render.Bad(word, t))
	default:
		parts = append(parts, render.Dim(word, t))
	}
	parts = append(parts, render.Dim(clockText(a.moment().Sub(s.Started)), t))
	var spend Usage
	for _, u := range s.Usage {
		spend.Input += u.Input
		spend.Output += u.Output
		spend.Cost += u.Cost
	}
	if n := spend.Input + spend.Output; n > 0 {
		parts = append(parts, render.Dim(render.Tokens(n)+" tokens", t))
	}
	if spend.Cost > 0 {
		parts = append(parts, render.Money(spend.Cost))
	}
	return strings.Join(parts, "  ")
}

// Glyph and color of what a member does now
func (a *arena) memberMark(m Member) string {
	t := a.env.Theme
	live := a.live[m.Name]
	switch {
	case live.approval:
		return render.Warm(t.Glyphs.Flag, t)
	case live.asking:
		return render.Cool(t.Glyphs.Ask, t)
	case live.running:
		return render.Good(t.Glyphs.Live, t)
	case live.failed:
		return render.Bad(t.Glyphs.Fail, t)
	case m.Status == "completed" || m.Status == string(OutcomeDone):
		return render.Good(t.Glyphs.Done, t)
	case m.Status == "queued":
		return render.Dim(t.Glyphs.Todo, t)
	}
	return render.Dim(t.Glyphs.Hollow, t)
}

// One crew row, the mark, the name, the role and what the member works on
func (a *arena) crewRow(m Member, w int) string {
	t := a.env.Theme
	indent := strings.Repeat(" ", min(m.Depth, 4))
	name := lipgloss.NewStyle().Foreground(t.Text).Bold(m.Name == a.sel).Render(m.Name)
	line := indent + a.memberMark(m) + " " + name + " " + render.Dim(string(m.Role), t)
	if tool := a.live[m.Name].tool; tool != "" && a.live[m.Name].running {
		line += " " + render.ToolGlyph(tool, t) + " " + tool
	}
	if m.Task != "" {
		line += "  " + render.Dim(strings.ReplaceAll(m.Task, "\n", " "), t)
	}
	return ansi.Truncate(line, w, t.Glyphs.Ellipsis)
}

// Ticket section, one row per delegated goal and todo item with its state and who works it
func (a *arena) ticketSection(w int) cards.Section {
	t := a.env.Theme
	sec := cards.Section{ID: secTickets.String(), Title: "tickets", Flex: true}
	s := a.snap
	if s == nil || len(s.Tickets) == 0 {
		sec.Lines = []string{render.Dim("no tickets", t)}
		return sec
	}
	done := 0
	for _, tk := range s.Tickets {
		if tk.Status == TicketDone {
			done++
		}
		glyph := t.Glyphs.Todo
		switch tk.Status {
		case TicketActive:
			glyph = t.Glyphs.Active
		case TicketReview:
			glyph = t.Glyphs.Ask
		case TicketDone:
			glyph = t.Glyphs.Done
		case TicketFailed:
			glyph = t.Glyphs.Fail
		case TicketBlocked:
			glyph = t.Glyphs.Skip
		}
		mark := lipgloss.NewStyle().Foreground(statusColor(tk.Status, t)).Render(glyph)
		id := lipgloss.NewStyle().Foreground(statusColor(tk.Status, t)).Bold(tk.ID == a.selTicket).Render(fmt.Sprintf("%-3s", tk.ID))
		line := mark + " " + id + " " + lipgloss.NewStyle().Foreground(t.Text).Render(strings.ReplaceAll(tk.Title, "\n", " "))
		owner := tk.Builder
		if tk.Kind == KindTodo {
			owner = "todo"
		}
		line += "  " + render.Dim(owner, t)
		if tk.Status != TicketTodo && !tk.Opened.IsZero() {
			end := tk.Closed
			if end.IsZero() {
				end = a.moment()
			}
			line += " " + render.Dim(clockText(end.Sub(tk.Opened)), t)
		}
		if tk.Round > 0 {
			line += render.Warm(fmt.Sprintf(" ×%d", tk.Round+1), t)
		}
		sec.Rows = append(sec.Rows, cards.Row{ID: "ticket:" + tk.ID, First: len(sec.Lines), N: 1})
		sec.Lines = append(sec.Lines, ansi.Truncate(line, w, t.Glyphs.Ellipsis))
	}
	sec.Count = fmt.Sprintf("%d/%d done", done, len(s.Tickets))
	return sec
}

// Color a message kind is logged in
func kindColor(k MsgKind, text string, t theme.Theme) color.Color {
	switch k {
	case MsgAssign:
		return t.Accent
	case MsgHandoff, MsgQuestion, MsgAnswer:
		return t.Ice
	case MsgFailure:
		return t.Alarm
	case MsgVerdict:
		switch {
		case strings.HasPrefix(text, string(VerdictAccepted)):
			return t.Alive
		case strings.HasPrefix(text, string(VerdictFailed)):
			return t.Alarm
		case strings.HasPrefix(text, string(VerdictRework)):
			return t.Accent
		}
		return t.Ice
	}
	return t.TextDim
}

// Log section, every exchange of the turn up to the moment shown, following its end
func (a *arena) logSection(w int) cards.Section {
	t := a.env.Theme
	sec := cards.Section{ID: secLog.String(), Title: "log", Flex: true, Tail: true}
	s := a.snap
	if s == nil || len(s.Messages) == 0 {
		sec.Lines = []string{render.Dim("nothing exchanged yet", t)}
		return sec
	}
	sec.Count = render.Count(len(s.Messages), "message")
	for _, m := range s.Messages {
		stamp := render.Dim(clockText(m.At.Sub(s.Started)), t)
		who := m.From
		if m.To != "" {
			who += " " + t.Glyphs.Caret + " " + m.To
		}
		kind := lipgloss.NewStyle().Foreground(kindColor(m.Kind, m.Text, t)).Render(string(m.Kind))
		line := stamp + " " + lipgloss.NewStyle().Foreground(t.Text).Render(who) + " " + kind
		if m.Ticket != "" {
			line += " " + render.Dim(m.Ticket, t)
		}
		if text := strings.ReplaceAll(m.Text, "\n", " "); text != "" {
			line += "  " + render.Dim(text, t)
		}
		if _, ok := s.Member(m.From); ok {
			sec.Rows = append(sec.Rows, cards.Row{ID: "log:" + m.From, First: len(sec.Lines), N: 1})
		}
		sec.Lines = append(sec.Lines, ansi.Truncate(line, w, t.Glyphs.Ellipsis))
	}
	return sec
}
