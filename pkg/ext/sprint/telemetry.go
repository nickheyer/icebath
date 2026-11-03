package sprint

import (
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/ui/cards"

	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/protocol"
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

// Generated tokens, prompt tokens processed and the share of them served from cache, with the cost when there is one
func usageLine(u protocol.Usage, cost float64, t theme.Theme) string {
	prompt := u.Input + u.CacheRead + u.CacheWrite
	if u.Output == 0 && prompt == 0 {
		return ""
	}
	parts := []string{"out " + render.Tokens(u.Output)}
	if prompt > 0 {
		parts = append(parts, "cache "+render.Percent(Cached(u)))
	}
	if cost > 0 {
		parts = append(parts, render.Money(cost))
	}
	return render.Dim(strings.Join(parts, "  "), t)
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

// Card over the floor beside a member with what the transcript would take minutes to tell, every line whole
func (a *arena) paintTelemetry(p *cards.Painter, name string, mark actorMark, sp spot, live liveState) {
	t := a.env.Theme
	wb := a.lay.world
	m, ok := a.snap.Member(name)
	if !ok {
		return
	}
	w := min(44, wb.w-6)
	if w < 16 {
		return
	}
	var lines []string
	title := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(m.Name)
	if !strings.EqualFold(m.Name, string(m.Role)) && !strings.HasPrefix(strings.ToLower(m.Name), string(m.Role)) {
		title += " " + render.Dim(string(m.Role), t)
	}
	lines = append(lines, title)
	lines = append(lines, cards.WrapIndent(a.stateText(live, m), w, 2)...)
	if m.Task == "" && !live.running && mark.idle {
		lines = append(lines, render.Dim("idle", t))
	}
	lines = append(lines, a.memberDetail(m, live, w)...)
	h := len(lines) + 2
	x, y := sp.x+4, sp.top-h/2
	if x+w+4 > wb.x+wb.w-1 {
		x = sp.x - w - 8
	}
	x = max(x, wb.x+1)
	y = min(max(y, wb.y), wb.y+wb.h-h-1)
	if y < wb.y || h > wb.h {
		return
	}
	p.Panel(x, y, w+4, h, "", 0.86)
	p.Lines(x+2, y+1, w, h-2, lines)
}

// Words on what an idle member is doing on the floor
func ambientText(a *actor) string {
	if a.duty.kind != dutyIdle && a.duty.kind != dutyGather || a.exit > 0 {
		return ""
	}
	return "idle"
}
