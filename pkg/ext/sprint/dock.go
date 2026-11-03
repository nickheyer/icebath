package sprint

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/ui/cards"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Lines of the request shown before the sprint section is opened in full
const requestPreview = 4

// Body of a section as painted, its lines and the clickable rows among them by line index
type body struct {
	lines []string
	rows  []rowHit
}

// Clickable row of a section, the lines it spans and what it stands for
type rowHit struct {
	id       string
	first, n int
}

// Body of a section kept from the last paint with the key of the state it was built from
type cachedBody struct {
	key  string
	body body
}

// Dock beside the world, the sprint, the team, the tickets and the log in sections that fold, scroll and share the height
func (a *arena) paintDock(p *cards.Painter) {
	t := a.env.Theme
	db := a.lay.dock
	p.Fill(db.x, db.y, db.w, db.h, t.Surface)
	p.Handle(db.x+db.w-1, db.y, db.h, a.hoverHit == "edge:dock" || (a.drag != nil && a.drag.kind == dragDock))
	x, w := db.x+1, db.w-3
	if w < 12 || db.h < 4 {
		return
	}
	var secs []cards.Section
	if a.snap == nil {
		secs = []cards.Section{{ID: secSprint.String(), Title: secSprint.String(), Lines: a.noSprintLines(w)}}
	}
	for _, sec := range sections {
		if a.snap == nil {
			break
		}
		b := a.cachedBody(sec, w)
		s := cards.Section{ID: sec.String(), Title: sec.String(), Lines: b.lines, Flex: sec == secTickets || sec == secLog, Tail: sec == secLog}
		for _, r := range b.rows {
			s.Rows = append(s.Rows, cards.Row{ID: r.id, First: r.first, N: r.n})
		}
		switch sec {
		case secTeam:
			s.Count = strconv.Itoa(len(a.snap.Members))
		case secTickets:
			s.Count = a.ticketCount()
		case secLog:
			s.Count = strconv.Itoa(len(a.snap.Messages))
		}
		secs = append(secs, s)
	}
	a.secs.Hover = a.hoverHit
	a.secs.Paint(p, x, db.y, w, db.h, secs, func(id string, _, hy, _, hh int) {
		a.hits = append(a.hits, hit{id: id, r: rect{db.x, hy, db.w - 1, hh}})
	}, func(id string) color.Color {
		switch lit, hover := a.rowState(id); {
		case lit:
			return theme.Mix(t.Surface, t.Ice, 0.22)
		case hover:
			return t.Steel
		}
		return nil
	})
}

// Body of a section, built again only when the width, the snapshot or what its lines show has changed
func (a *arena) cachedBody(sec section, w int) body {
	var key string
	now := time.Now().Unix()
	switch sec {
	case secSprint:
		key = fmt.Sprintf("%d|%d|%v|%d", w, a.snapGen, a.sprintFull, now)
	case secTeam:
		key = fmt.Sprintf("%d|%d|%s|%d|%d", w, a.snapGen, a.sel, int(a.sc.t*8)%len(a.env.Theme.Glyphs.Spinner), now)
	default:
		key = fmt.Sprintf("%d|%d", w, a.snapGen)
	}
	if c := a.bodies[sec]; c.key == key {
		return c.body
	}
	var b body
	switch sec {
	case secSprint:
		b = a.sprintBody(w)
	case secTeam:
		b = a.teamBody(w)
	case secTickets:
		b = a.ticketBody(w)
	case secLog:
		b = a.logBody(w)
	}
	a.bodies[sec] = cachedBody{key: key, body: b}
	return b
}

// Whether a row stands selected and whether the pointer is over it or what it names
func (a *arena) rowState(id string) (lit, hover bool) {
	kind, rest, _ := strings.Cut(id, ":")
	switch kind {
	case "member":
		return rest == a.sel, rest == a.hover || id == a.hoverHit
	case "ticket":
		return rest == a.selTicket, rest == a.hoverTicket || id == a.hoverHit
	}
	return false, id == a.hoverHit
}

// Done over total tickets as text
func (a *arena) ticketCount() string {
	done, total := a.snap.Progress()
	return fmt.Sprintf("%d/%d", done, total)
}

// Lines of the sprint section while no sprint has run in this process
func (a *arena) noSprintLines(w int) []string {
	t := a.env.Theme
	lines := []string{lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(t.Glyphs.Mission + " no sprint")}
	lines = append(lines, wrapDim("run /"+Name+" <request> to start one", w, t)...)
	if a.sprintCount > 0 {
		lines = append(lines, wrapDim(fmt.Sprintf("%d earlier sprints, %s and %s switch", a.sprintCount, a.env.Keys.Display(ActSprintPrev), a.env.Keys.Display(ActSprintNext)), w, t)...)
	}
	if a.cfgErr != nil {
		lines = append(lines, cards.WrapStyled("settings: "+a.cfgErr.Error(), w, t.Alarm)...)
	}
	return lines
}

// Sprint id, request, stage pipeline, progress, spend and status, the request whole once opened
func (a *arena) sprintBody(w int) body {
	t := a.env.Theme
	s := a.snap
	title := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(t.Glyphs.Mission + " " + s.ID)
	if ord := a.sprintOrdinal(); ord != "" {
		title += " " + ord
	}
	b := body{lines: []string{title}}
	request := cards.WrapStyled(strings.TrimSpace(s.Request), w, t.Text)
	if !a.sprintFull && len(request) > requestPreview {
		more := len(request) - requestPreview
		request = append(request[:requestPreview], render.Dim(fmt.Sprintf("%s %d more lines, click to read", t.Glyphs.Caret, more), t))
	}
	b.rows = append(b.rows, rowHit{id: "sprint:request", first: len(b.lines), n: len(request)})
	b.lines = append(b.lines, request...)
	if a.sprintFull {
		if s.Brief.Objective != "" {
			b.lines = append(b.lines, render.Dim("objective", t))
			b.lines = append(b.lines, cards.WrapStyled(strings.TrimSpace(s.Brief.Objective), w, t.Text)...)
		}
		if s.Plan.Title != "" {
			b.lines = append(b.lines, render.Dim("plan", t))
			b.lines = append(b.lines, cards.WrapStyled(strings.TrimSpace(s.Plan.Title+". "+s.Plan.Summary), w, t.Text)...)
		}
		b.lines = append(b.lines, render.Dim(t.Glyphs.Up+" click to fold", t))
	}
	var stages []string
	for i, st := range Stages {
		style := lipgloss.NewStyle().Foreground(t.TextDim)
		switch {
		case s.Stage == st && s.Running():
			style = style.Foreground(t.Accent).Bold(true)
		case s.Stage == st:
			style = style.Foreground(t.Text).Bold(true)
		case stageIndex(s.Last) > i || s.Stage == StageDone || stageIndex(s.Stage) > i:
			style = style.Foreground(t.Alive)
		}
		stages = append(stages, style.Render(strings.ToUpper(string(st))))
	}
	tail := ""
	switch s.Stage {
	case StageDone:
		tail = " " + render.Good(t.Glyphs.Success+" done", t)
	case StageFailed:
		tail = " " + render.Bad(t.Glyphs.Fail+" failed", t)
	}
	b.lines = append(b.lines, strings.Join(stages, render.Dim(" "+t.Glyphs.Bullet+" ", t))+tail)
	done, total := s.Progress()
	elapsed := render.Elapsed(s.Elapsed()) + " of " + durationText(a.cfg.Limits.Total)
	if total > 0 {
		b.lines = append(b.lines, render.Bar(float64(done)/float64(total), min(12, w/3), t)+" "+fmt.Sprintf("%d/%d tickets", done, total))
	}
	b.lines = append(b.lines, render.Dim(elapsed, t))
	var used protocol.Usage
	for _, u := range s.Usage {
		used = used.Add(u)
	}
	if line := usageLine(used, a.spent, t); line != "" {
		b.lines = append(b.lines, line)
	}
	if status, fg := a.statusLine(s); status != "" {
		b.lines = append(b.lines, cards.WrapStyled(status, w, fg)...)
	}
	return b
}

// Place of a stage in the order, past the last for done and failed
func stageIndex(st Stage) int {
	for i, s := range Stages {
		if s == st {
			return i
		}
	}
	return len(Stages)
}

// Duration in whole hours and minutes
func durationText(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}

// Status worth reading beside the stage, with the color it paints in
func (a *arena) statusLine(s *Snapshot) (string, color.Color) {
	t := a.env.Theme
	switch {
	case a.cfgErr != nil:
		return "settings: " + a.cfgErr.Error(), t.Alarm
	case s.Running() && s.Paused != "":
		return t.Glyphs.Warn + " " + s.Paused, t.Accent
	case s.Running() && s.Waiting != "" && a.live[s.Waiting].asking:
		return t.Glyphs.Ask + " " + s.Waiting + " asks you, open it and answer", t.Ice
	case s.Running() && s.Waiting != "":
		return t.Glyphs.Flag + " " + s.Waiting + " waits for you", t.Accent
	case s.Exited():
		return t.Glyphs.Warn + " " + s.Status + ", " + a.env.Keys.Display(ActRestart) + " restarts", t.Accent
	case !s.Running() && s.Err != nil && !s.Stopped():
		return t.Glyphs.Fail + " " + s.Err.Error() + ", " + a.env.Keys.Display(ActRestart) + " restarts at the " + string(restartStage(*s)) + " stage", t.Alarm
	case s.Stopped():
		return "stopped, " + a.env.Keys.Display(ActRestart) + " restarts at the " + string(restartStage(*s)) + " stage", t.Accent
	}
	return s.Status, t.TextDim
}

// Glyph and color of the state of a member
func (a *arena) stateMark(st liveState) (string, color.Color) {
	t := a.env.Theme
	tick := int(a.sc.t * 8)
	switch {
	case st.approval:
		return t.Glyphs.Flag, t.Accent
	case st.asking:
		return t.Glyphs.Ask, t.Ice
	case st.running:
		return t.Glyphs.Spinner[tick%len(t.Glyphs.Spinner)], t.Alive
	case st.failed:
		return t.Glyphs.Fail, t.Alarm
	}
	return t.Glyphs.Hollow, t.TextDim
}

// Two rows per member, its state, name, role and ticket, then what it does, the selected one followed by its detail
func (a *arena) teamBody(w int) body {
	t := a.env.Theme
	var b body
	for _, m := range a.snap.Members {
		st := a.live[m.Name]
		glyph, fg := a.stateMark(st)
		name := lipgloss.NewStyle().Foreground(t.Text).Bold(true).Render(m.Name)
		if !strings.EqualFold(m.Name, string(m.Role)) && !strings.HasPrefix(strings.ToLower(m.Name), string(m.Role)) {
			name += " " + render.Dim(string(m.Role), t)
		}
		if m.Ticket != "" {
			name += "  " + lipgloss.NewStyle().Foreground(t.Ice).Render(m.Ticket)
		}
		first := len(b.lines)
		b.lines = append(b.lines, lipgloss.NewStyle().Foreground(fg).Render(glyph)+" "+name)
		doing := m.Task
		switch {
		case st.approval:
			doing = "waiting for your approval of " + st.tool
		case st.asking:
			doing = "asking you"
		case st.running && st.tool != "":
			doing = render.ToolGlyph(st.tool, t) + " " + st.tool
			if m.Task != "" {
				doing += "  " + m.Task
			}
		case doing == "":
			doing = ambientText(a.sc.byName[m.Name])
		}
		for _, l := range cards.WrapIndent(doing, w-2, 0) {
			b.lines = append(b.lines, "  "+render.Dim(l, t))
		}
		if m.Name == a.sel {
			for _, d := range a.memberDetail(m, st, w-2) {
				b.lines = append(b.lines, "  "+d)
			}
		}
		b.rows = append(b.rows, rowHit{id: "member:" + m.Name, first: first, n: len(b.lines) - first})
	}
	return b
}

// Detail lines of a member, its model, its ticket, its clock, its spend, its context and its tool tally
func (a *arena) memberDetail(m Member, live liveState, w int) []string {
	t := a.env.Theme
	var lines []string
	if m.Live != nil && m.Live.Endpoint.Model != "" {
		lines = append(lines, render.Dim(m.Live.Endpoint.Model, t))
	}
	if tk, ok := a.snap.Ticket(m.Ticket); ok {
		head := lipgloss.NewStyle().Foreground(t.Ice).Render(tk.ID) + " " + lipgloss.NewStyle().Foreground(statusColor(tk.Status, t)).Render(string(tk.Status))
		if tk.Round > 0 {
			head += render.Warm(fmt.Sprintf(" sent back %d", tk.Round), t)
		}
		lines = append(lines, cards.WrapIndent(head+" "+tk.Title, w, 3)...)
	}
	if !live.since.IsZero() && live.limit > 0 {
		share := time.Since(live.since).Seconds() / live.limit.Seconds()
		clock := render.Elapsed(time.Since(live.since)) + " of " + durationText(live.limit)
		if share >= 1 {
			lines = append(lines, render.Bad(t.Glyphs.Warn+" past the limit, told to wrap up", t))
		} else {
			lines = append(lines, render.Bar(share, 8, t)+" "+render.Dim(clock, t))
		}
	}
	if budget := a.cfg.Roles[m.Role].Budget; budget > 0 {
		spent := a.snap.Spent[m.Name]
		lines = append(lines, render.Bar(float64(spent)/float64(budget), 8, t)+" "+render.Dim(render.Tokens(spent)+" of "+render.Tokens(budget)+" budget", t))
	}
	ctx := ""
	switch {
	case live.context > 0 && live.contextLimit > 0:
		ctx = "ctx " + render.Percent(float64(live.context)/float64(live.contextLimit))
	case live.context > 0:
		ctx = "ctx " + render.Tokens(live.context) + ", window unset"
	}
	if line := usageLine(a.snap.Usage[m.Name], live.cost, t); line != "" || ctx != "" {
		if ctx != "" && line != "" {
			line = render.Dim(ctx, t) + "  " + line
		} else if ctx != "" {
			line = render.Dim(ctx, t)
		}
		lines = append(lines, line)
	}
	if n := live.tally.total(); n > 0 {
		lines = append(lines, tallyText(live.tally, t))
	}
	return lines
}

// Tool tally as glyph and count pairs after the total
func tallyText(tl toolTally, t theme.Theme) string {
	var parts []string
	for _, c := range []struct {
		n     int
		glyph string
	}{{tl.reads, t.Glyphs.Read}, {tl.edits, t.Glyphs.Edit}, {tl.shells, t.Glyphs.Shell}, {tl.fetches, t.Glyphs.Fetch}, {tl.agents, t.Glyphs.Spawn}, {tl.calls, t.Glyphs.Flow}, {tl.others, t.Glyphs.Tool}} {
		if c.n > 0 {
			parts = append(parts, lipgloss.NewStyle().Foreground(t.Ice).Render(c.glyph)+fmt.Sprint(c.n))
		}
	}
	return render.Dim(fmt.Sprintf("%d tool calls ", tl.total()), t) + strings.Join(parts, " ")
}

// Columns the id and the status of a ticket take before its title
const ticketIndent = 13

// Every ticket with its id, status and whole title, then who holds it, what it waits for and why it ended short
func (a *arena) ticketBody(w int) body {
	t := a.env.Theme
	var b body
	if len(a.snap.Tickets) == 0 {
		return body{lines: []string{render.Dim("no tickets yet", t)}}
	}
	for _, tk := range a.snap.Tickets {
		first := len(b.lines)
		head := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(fmt.Sprintf("%-3s", tk.ID)) + "  " + lipgloss.NewStyle().Foreground(statusColor(tk.Status, t)).Render(fmt.Sprintf("%-7s", string(tk.Status))) + " "
		title := lipgloss.NewStyle().Foreground(t.Text).Render(tk.Title)
		if w <= ticketIndent+8 {
			b.lines = append(b.lines, head)
			b.lines = append(b.lines, cards.WrapIndent(title, w, 4)...)
		} else {
			lines := cards.WrapIndent(title, w-ticketIndent, 0)
			b.lines = append(b.lines, head+lines[0])
			for _, l := range lines[1:] {
				b.lines = append(b.lines, strings.Repeat(" ", ticketIndent)+l)
			}
		}
		var tail []string
		if tk.Builder != "" {
			tail = append(tail, tk.Builder)
		}
		if tk.Round > 0 {
			tail = append(tail, fmt.Sprintf("sent back %d", tk.Round))
		}
		if len(tk.DependsOn) > 0 && tk.Status == TicketTodo {
			tail = append(tail, "after "+strings.Join(tk.DependsOn, " "))
		}
		if len(tail) > 0 {
			for _, l := range cards.WrapIndent(strings.Join(tail, " "+t.Glyphs.Sep+" "), w-4, 0) {
				b.lines = append(b.lines, "    "+render.Dim(l, t))
			}
		}
		if tk.Err != "" {
			for _, l := range cards.WrapIndent(t.Glyphs.Fail+" "+tk.Err, w-4, 2) {
				b.lines = append(b.lines, "    "+render.Bad(l, t))
			}
		}
		b.rows = append(b.rows, rowHit{id: "ticket:" + tk.ID, first: first, n: len(b.lines) - first})
	}
	return b
}

// Every exchange oldest first, its time, who spoke to whom and the ticket, the words whole under them when they do not fit beside
func (a *arena) logBody(w int) body {
	t := a.env.Theme
	var b body
	for _, m := range a.snap.Messages {
		who := m.From
		switch {
		case m.To != "" && who != "":
			who += " " + t.Glyphs.Steer + " " + m.To
		case m.To != "":
			who = string(m.Kind) + " " + t.Glyphs.Steer + " " + m.To
		case who == "":
			who = string(m.Kind)
		}
		head := render.Dim(m.At.Local().Format("15:04"), t) + " " + lipgloss.NewStyle().Foreground(kindColor(m.Kind, t)).Render(who)
		if m.Ticket != "" {
			head += " " + lipgloss.NewStyle().Foreground(t.Ice).Render(m.Ticket)
		}
		text := lipgloss.NewStyle().Foreground(t.Text).Render(strings.TrimSpace(m.Text))
		first := len(b.lines)
		if ansi.StringWidth(head)+1+ansi.StringWidth(text) <= w {
			b.lines = append(b.lines, head+" "+text)
		} else {
			b.lines = append(b.lines, head)
			for _, l := range cards.WrapIndent(text, w-6, 0) {
				b.lines = append(b.lines, "      "+l)
			}
		}
		id := "log:" + m.From
		if _, ok := a.snap.Member(m.From); !ok {
			id = "log:" + m.To
		}
		b.rows = append(b.rows, rowHit{id: id, first: first, n: len(b.lines) - first})
	}
	if len(b.lines) == 0 {
		b.lines = []string{render.Dim("nothing has happened yet", t)}
	}
	return b
}

// Color an exchange kind paints its names in
func kindColor(k MessageKind, t theme.Theme) color.Color {
	switch k {
	case MsgVerdict:
		return t.Alive
	case MsgHandoff:
		return t.Ice
	case MsgAssign:
		return t.Accent
	case MsgQuestion, MsgAnswer:
		return t.Pulse
	case MsgChat:
		return t.Text
	case MsgFailure:
		return t.Alarm
	}
	return t.TextDim
}

// Text wrapped to a width and dimmed
func wrapDim(text string, w int, t theme.Theme) []string {
	var out []string
	for _, line := range cards.WrapIndent(text, w, 0) {
		out = append(out, render.Dim(line, t))
	}
	return out
}

// Clickable thing under a cell of the last paint
func (a *arena) hitAt(x, y int) (string, bool) {
	for i := len(a.hits) - 1; i >= 0; i-- {
		if a.hits[i].r.contains(x, y) {
			return a.hits[i].id, true
		}
	}
	return "", false
}
