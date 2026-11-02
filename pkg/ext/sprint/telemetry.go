package sprint

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Tokens a member with no budget is taken to have, so its heat still shows
const nominalBudget = 400000

// Share of the budget spent, of the nominal budget when none is set, capped at one
func heatOf(spent, budget int) float64 {
	if budget <= 0 {
		budget = nominalBudget
	}
	return min(float64(spent)/float64(budget), 1)
}

// Time one exchange of a role may take under the limits
func stageLimit(cfg Config, role Role) time.Duration {
	switch role {
	case RoleDirector:
		return cfg.Limits.Brief
	case RoleArchitect:
		return cfg.Limits.Plan
	case RoleReviewer:
		return cfg.Limits.Review
	}
	return cfg.Limits.Build
}

// Moment the current exchange of a member began, from the message that opened it, zero while none is open
func exchangeStart(snap *Snapshot, m Member) time.Time {
	var at time.Time
	for _, msg := range snap.Messages {
		switch m.Role {
		case RoleBuilder:
			if m.Ticket == "" {
				continue
			}
			if msg.Ticket == m.Ticket && msg.To == m.Name && (msg.Kind == MsgAssign || msg.Kind == MsgVerdict) {
				at = msg.At
			}
		case RoleReviewer:
			if msg.Kind == MsgHandoff && msg.To == m.Name && msg.Ticket == m.Ticket {
				at = msg.At
			}
		case RoleDirector:
			if msg.Kind == MsgStage && (strings.HasPrefix(msg.Text, string(StageBrief)) || strings.HasPrefix(msg.Text, string(StageReport))) {
				at = msg.At
			}
		case RoleArchitect:
			if msg.Kind == MsgStage && strings.HasPrefix(msg.Text, string(StagePlan)) {
				at = msg.At
			}
		}
	}
	if m.Role == RoleBuilder && m.Ticket != "" {
		if t, ok := snap.Ticket(m.Ticket); ok && at.Before(t.Started) {
			at = t.Started
		}
	}
	if m.Task == "" {
		return time.Time{}
	}
	return at
}

// Card over the floor beside a member with what the transcript would take minutes to tell
func (a *arena) paintTelemetry(p *painter, act *actor, sp spot, live liveState) {
	t := a.env.Theme
	wb := a.lay.world
	m, ok := a.snap.Member(act.name)
	if !ok {
		return
	}
	var lines []string
	title := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(m.Name)
	if !strings.EqualFold(m.Name, string(m.Role)) {
		title += " " + render.Dim(string(m.Role), t)
	}
	lines = append(lines, title)
	switch {
	case live.approval:
		lines = append(lines, render.Warm(t.Glyphs.Flag+" waiting for your approval", t))
	case live.asking:
		lines = append(lines, render.Cool(t.Glyphs.Ask+" asking you", t))
	case live.running && live.tool != "":
		lines = append(lines, render.Good(t.Glyphs.Live+" running", t)+"  "+render.ToolGlyph(live.tool, t)+" "+live.tool)
	case live.running:
		lines = append(lines, render.Good(t.Glyphs.Live+" thinking", t))
	case live.failed:
		lines = append(lines, render.Bad(t.Glyphs.Fail+" last run failed", t))
	default:
		lines = append(lines, render.Dim(t.Glyphs.Hollow+" idle", t))
	}
	if m.Task != "" {
		lines = append(lines, render.Truncate(m.Task, 34, t))
	} else if doing := ambientText(act); doing != "" {
		lines = append(lines, render.Dim(doing, t))
	}
	if tk, ok := a.snap.Ticket(m.Ticket); ok {
		line := lipgloss.NewStyle().Foreground(t.Ice).Render(tk.ID) + " " + statusText(tk.Status, t)
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
			lines = append(lines, bar(share, 10, t)+" "+render.Dim(clock, t))
		}
	}
	spent := a.snap.Spent[m.Name]
	budget := a.cfg.Roles[m.Role].Budget
	switch {
	case budget > 0:
		lines = append(lines, bar(float64(spent)/float64(budget), 10, t)+" "+render.Dim(render.Tokens(spent)+" of "+render.Tokens(budget)+" tok", t))
	case spent > 0:
		lines = append(lines, render.Dim(render.Tokens(spent)+" tok, no budget set", t))
	}
	if live.tally.total() > 0 {
		lines = append(lines, tallyText(live.tally, t))
	}
	w := 0
	for _, l := range lines {
		w = max(w, ansi.StringWidth(l))
	}
	w = min(w+4, wb.w-2)
	h := len(lines) + 2
	x, y := sp.x+4, sp.top-h/2
	if x+w > wb.x+wb.w-1 {
		x = sp.x - w - 4
	}
	x = max(x, wb.x+1)
	y = min(max(y, wb.y+a.stripRows()), wb.y+wb.h-h-1)
	if y < wb.y || h > wb.h {
		return
	}
	p.panel(x, y, w, h, "", 0.86)
	p.lines(x+2, y+1, w-4, h-2, lines)
}

// Words on what an idle member is doing on the floor
func ambientText(a *actor) string {
	if a.duty.kind != dutyIdle && a.duty.kind != dutyGather || a.exit > 0 {
		return ""
	}
	switch a.ambient {
	case "inspect":
		return "idle, looking over a ticket"
	case "huddle":
		if a.partner != nil {
			return "idle, talking with " + a.partner.name
		}
	case "perch":
		return "idle, sitting on the edge"
	case "watch":
		return "idle, watching a builder work"
	case "gaze":
		return "idle, looking at the horizon"
	case "wander":
		return "idle, wandering the floor"
	}
	return "idle"
}

// Seconds a stage banner shows for after a stage change
func stageBanner(sc *scene, t theme.Theme) string {
	if sc.stageFlash <= 0 || sc.stageText == "" {
		return ""
	}
	return t.Glyphs.Mission + " " + sc.stageText
}
