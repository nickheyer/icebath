package sprint

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Width of the sidebar on a wide card and on a narrower one
const (
	sideWide   = 40
	sideNarrow = 34
)

// What a row of the sidebar stands for, so a click on it selects a member
type sideRow struct {
	member string
	r      rect
}

// Sidebar beside the floor with the sprint, its team, its tickets and its log
func (a *arena) paintSide(p *painter) {
	t := a.env.Theme
	sb := a.lay.side
	p.fill(sb.x, sb.y, sb.w, sb.h, t.Surface)
	for y := sb.y; y < sb.y+sb.h; y++ {
		p.label(sb.x+sb.w-1, y, t.Glyphs.Sep, t.Steel, t.Surface, false)
	}
	a.sideRows = a.sideRows[:0]
	a.sideTickets, a.sideLog = rect{}, rect{}
	x, w := sb.x+1, sb.w-3
	if w < 10 || sb.h < 4 {
		return
	}
	y := sb.y
	head := a.sideHead(w)
	p.lines(x, y, w, min(len(head), sb.h), head)
	y += len(head)
	if a.snap == nil {
		return
	}
	live := a.liveStates()
	team, rows := a.sideTeam(w, live)
	if y+len(team) > sb.y+sb.h {
		team = team[:max(sb.y+sb.h-y, 0)]
	}
	p.lines(x, y, w, len(team), team)
	for _, r := range rows {
		if r.r.y < len(team) {
			a.sideRows = append(a.sideRows, sideRow{member: r.member, r: rect{sb.x, y + r.r.y, sb.w - 1, 1}})
		}
	}
	y += len(team)
	rem := sb.y + sb.h - y
	if rem < 2 {
		return
	}
	tickets := a.sideTicketLines(w)
	log := a.sideLogLines(w)
	th, lh := a.split(rem, len(tickets), len(log))
	if th > 0 {
		a.sideTickets = rect{sb.x, y, sb.w - 1, th}
		a.boardTop = min(a.boardTop, max(len(tickets)-(th-1), 0))
		head := a.rule("TICKETS", a.ticketCount(), w, t)
		p.lines(x, y, w, 1, []string{head})
		p.lines(x, y+1, w, th-1, tickets[a.boardTop:])
		y += th
	}
	if lh > 0 {
		a.sideLog = rect{sb.x, y, sb.w - 1, lh}
		rows := lh - 1
		a.logTop = min(a.logTop, max(len(log)-rows, 0))
		end := len(log) - a.logTop
		start := max(end-rows, 0)
		p.lines(x, y, w, 1, []string{a.rule("LOG", fmt.Sprint(len(a.snap.Messages)), w, t)})
		p.lines(x, y+1, w, rows, log[start:end])
	}
}

// Rows of the lower sidebar the tickets and the log get, one of them taking everything when expanded
func (a *arena) split(rem, tickets, log int) (th, lh int) {
	switch {
	case a.showBoard:
		return rem, 0
	case a.showLog:
		return 0, rem
	case rem < 6:
		return rem, 0
	}
	th = min(tickets+1, max(rem*55/100, 3))
	lh = rem - th
	if lh < 3 {
		return rem, 0
	}
	return th, lh
}

// Section rule with a title and a count at the right
func (a *arena) rule(title, count string, w int, t theme.Theme) string {
	left := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(title)
	right := render.Dim(count, t)
	gap := w - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	if gap < 1 {
		return left
	}
	return left + " " + lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, gap)) + " " + right
}

// Done over total tickets as text
func (a *arena) ticketCount() string {
	done, total := a.snap.Progress()
	return fmt.Sprintf("%d/%d", done, total)
}

// Sprint id, request, stage pipeline, progress, spend and status at the top of the sidebar
func (a *arena) sideHead(w int) []string {
	t := a.env.Theme
	s := a.snap
	var lines []string
	if s == nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(t.Glyphs.Mission+" no sprint"))
		lines = append(lines, wrapDim("run /"+Name+" <request> to start one", w, t)...)
		if n := len(Sprints()); n > 0 {
			lines = append(lines, wrapDim(fmt.Sprintf("%d earlier sprints, %s and %s switch", n, a.env.Keys.Display(ActSprintPrev), a.env.Keys.Display(ActSprintNext)), w, t)...)
		}
		if a.cfgErr != nil {
			lines = append(lines, wrapStyled("settings: "+a.cfgErr.Error(), w, t.Alarm)...)
		}
		return append(lines, "")
	}
	title := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(t.Glyphs.Mission + " " + s.ID)
	if all := Sprints(); len(all) > 1 {
		for i, other := range all {
			if other.ID == s.ID {
				title += " " + render.Dim(fmt.Sprintf("%d of %d", i+1, len(all)), t)
			}
		}
	}
	lines = append(lines, title)
	lines = append(lines, wrapDim(firstLine(s.Request), w, t)...)
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
		tail = " " + render.Bad(t.Glyphs.Fail, t)
	}
	lines = append(lines, strings.Join(stages, render.Dim(" "+t.Glyphs.Bullet+" ", t))+tail)
	done, total := s.Progress()
	progress := render.Elapsed(s.Elapsed()) + " of " + durationText(a.cfg.Limits.Total)
	if total > 0 {
		progress = bar(float64(done)/float64(total), 10, t) + " " + fmt.Sprintf("%d/%d", done, total) + "  " + progress
	}
	lines = append(lines, render.Truncate(progress, w, t))
	spent := 0
	for _, n := range s.Spent {
		spent += n
	}
	var meta []string
	if spent > 0 {
		meta = append(meta, render.Tokens(spent)+" tok")
	}
	if cost := a.cost(s); cost > 0 {
		meta = append(meta, render.Money(cost))
	}
	if len(meta) > 0 {
		lines = append(lines, render.Dim(strings.Join(meta, "  "), t))
	}
	status, fg := a.statusLine(s)
	if status != "" {
		lines = append(lines, wrapStyled(status, w, fg)...)
	}
	return append(lines, "")
}

// Status worth reading beside the stage, with the color it paints in
func (a *arena) statusLine(s *Snapshot) (string, color.Color) {
	t := a.env.Theme
	live := a.liveStates()
	switch {
	case a.cfgErr != nil:
		return "settings: " + a.cfgErr.Error(), t.Alarm
	case s.Running() && s.Paused != "":
		return t.Glyphs.Warn + " " + s.Paused, t.Accent
	case s.Running() && s.Waiting != "" && live[s.Waiting].asking:
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

// One row per member with its state, ticket and tool, the selected one lit and followed by its detail
func (a *arena) sideTeam(w int, live map[string]liveState) ([]string, []sideRow) {
	t := a.env.Theme
	var lines []string
	var rows []sideRow
	lines = append(lines, a.rule("TEAM", fmt.Sprint(len(a.snap.Members)), w, t))
	tick := int(a.sc.t * 8)
	for _, m := range a.snap.Members {
		st := live[m.Name]
		glyph, fg := t.Glyphs.Hollow, t.TextDim
		switch {
		case st.approval:
			glyph, fg = t.Glyphs.Flag, t.Accent
		case st.asking:
			glyph, fg = t.Glyphs.Ask, t.Ice
		case st.running:
			glyph, fg = t.Glyphs.Spinner[tick%len(t.Glyphs.Spinner)], t.Alive
		case st.failed:
			glyph, fg = t.Glyphs.Fail, t.Alarm
		}
		doing := m.Task
		switch {
		case st.approval:
			doing = "waiting for your approval"
		case st.asking:
			doing = "asking you"
		case st.running && st.tool != "":
			doing = st.tool
		case doing == "":
			doing = ambientText(a.sc.byName[m.Name])
		}
		name := m.Name
		if !strings.EqualFold(m.Name, string(m.Role)) && !strings.HasPrefix(strings.ToLower(m.Name), string(m.Role)) {
			name += " " + render.Dim(string(m.Role), t)
		}
		if m.Ticket != "" {
			name += " " + lipgloss.NewStyle().Foreground(t.Ice).Render(m.Ticket)
		}
		row := lipgloss.NewStyle().Foreground(fg).Render(glyph) + " " + name
		room := w - ansi.StringWidth(row) - 1
		if room > 6 {
			row += " " + render.Dim(render.Truncate(doing, room, t), t)
		}
		if m.Name == a.sel {
			row = lipgloss.NewStyle().Background(t.Ice).Foreground(t.Bg).Render(ansi.Truncate(" "+ansi.Strip(row), w, "") + strings.Repeat(" ", max(w-ansi.StringWidth(ansi.Strip(row))-1, 0)))
		} else if m.Name == a.hover {
			row = lipgloss.NewStyle().Background(t.Steel).Render(" " + row)
		} else {
			row = " " + row
		}
		rows = append(rows, sideRow{member: m.Name, r: rect{0, len(lines), w, 1}})
		lines = append(lines, row)
		if m.Name == a.sel {
			for _, d := range a.memberDetail(m, st) {
				lines = append(lines, "   "+render.Truncate(d, w-3, t))
			}
		}
	}
	return append(lines, ""), rows
}

// Detail lines of a member, its task, its clock, its spend and its tool tally
func (a *arena) memberDetail(m Member, live liveState) []string {
	t := a.env.Theme
	var lines []string
	if m.Task != "" {
		lines = append(lines, m.Task)
	}
	if m.Live != nil && m.Live.Endpoint.Model != "" {
		lines = append(lines, render.Dim(m.Live.Endpoint.Model, t))
	}
	if tk, ok := a.snap.Ticket(m.Ticket); ok {
		line := lipgloss.NewStyle().Foreground(t.Ice).Render(tk.ID) + " " + statusText(tk.Status, t) + " " + tk.Title
		if tk.Round > 0 {
			line += render.Warm(fmt.Sprintf(" sent back %d", tk.Round), t)
		}
		lines = append(lines, line)
	}
	if !live.since.IsZero() && live.limit > 0 {
		share := time.Since(live.since).Seconds() / live.limit.Seconds()
		clock := render.Elapsed(time.Since(live.since)) + " of " + durationText(live.limit)
		if share >= 1 {
			lines = append(lines, render.Bad(t.Glyphs.Warn+" past the limit, told to wrap up", t))
		} else {
			lines = append(lines, bar(share, 8, t)+" "+render.Dim(clock, t))
		}
	}
	spent := a.snap.Spent[m.Name]
	budget := a.cfg.Roles[m.Role].Budget
	switch {
	case budget > 0:
		lines = append(lines, bar(float64(spent)/float64(budget), 8, t)+" "+render.Dim(render.Tokens(spent)+" of "+render.Tokens(budget)+" tok", t))
	case spent > 0:
		lines = append(lines, render.Dim(render.Tokens(spent)+" tok", t))
	}
	if a.env.Meter != nil && m.SessionID() != "" {
		if tel, ok := a.env.Meter.SnapshotOf(m.SessionID()); ok && tel.Context > 0 {
			ctx := "ctx " + render.Tokens(tel.Context)
			if tel.ContextLimit > 0 {
				ctx = "ctx " + bar(float64(tel.Context)/float64(tel.ContextLimit), 8, t) + " " + render.Percent(float64(tel.Context)/float64(tel.ContextLimit))
			}
			if tel.Cost > 0 {
				ctx += "  " + render.Money(tel.Cost)
			}
			lines = append(lines, render.Dim(ctx, t))
		}
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

// One line per ticket with its status, title, builder and what holds it
func (a *arena) sideTicketLines(w int) []string {
	t := a.env.Theme
	var lines []string
	if len(a.snap.Tickets) == 0 {
		return []string{render.Dim("no tickets yet", t)}
	}
	for _, tk := range a.snap.Tickets {
		line := lipgloss.NewStyle().Foreground(t.Ice).Render(fmt.Sprintf("%-3s", tk.ID)) + " " + statusText(tk.Status, t) + " " + tk.Title
		var tail []string
		if tk.Builder != "" {
			tail = append(tail, tk.Builder)
		}
		if tk.Round > 0 {
			tail = append(tail, fmt.Sprintf("back %d", tk.Round))
		}
		if len(tk.DependsOn) > 0 && tk.Status == TicketTodo {
			tail = append(tail, "after "+strings.Join(tk.DependsOn, " "))
		}
		if len(tail) > 0 {
			line += " " + render.Dim(strings.Join(tail, " "+t.Glyphs.Sep+" "), t)
		}
		lines = append(lines, render.Truncate(line, w, t))
		if tk.Err != "" {
			lines = append(lines, "    "+render.Bad(render.Truncate(tk.Err, w-4, t), t))
		}
	}
	return lines
}

// One line per exchange, oldest first
func (a *arena) sideLogLines(w int) []string {
	t := a.env.Theme
	var lines []string
	for _, m := range a.snap.Messages {
		who := m.From
		if m.To != "" {
			who += t.Glyphs.Steer + m.To
		}
		if who == "" {
			who = string(m.Kind)
		}
		line := render.Dim(m.At.Local().Format("15:04"), t) + " " + lipgloss.NewStyle().Foreground(kindColor(m.Kind, t)).Render(who)
		if m.Ticket != "" {
			line += " " + lipgloss.NewStyle().Foreground(t.Ice).Render(m.Ticket)
		}
		lines = append(lines, render.Truncate(line+" "+firstLine(m.Text), w, t))
	}
	if len(lines) == 0 {
		lines = []string{render.Dim("nothing has happened yet", t)}
	}
	return lines
}

// Text wrapped to a width and dimmed, at most two lines
func wrapDim(text string, w int, t theme.Theme) []string {
	var out []string
	for _, line := range strings.Split(render.Wrap(text, w), "\n") {
		out = append(out, render.Dim(line, t))
		if len(out) == 2 {
			break
		}
	}
	return out
}

// Text wrapped to a width in a color, at most three lines, the last cut
func wrapStyled(text string, w int, fg color.Color) []string {
	style := lipgloss.NewStyle().Foreground(fg)
	lines := strings.Split(render.Wrap(text, w), "\n")
	if len(lines) > 3 {
		lines = lines[:3]
		lines[2] = ansi.Truncate(lines[2], w, "…")
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, style.Render(l))
	}
	return out
}

// Member whose sidebar row sits under a cell
func (a *arena) sideRowAt(x, y int) (string, bool) {
	for _, r := range a.sideRows {
		if r.r.contains(x, y) {
			return r.member, true
		}
	}
	return "", false
}
