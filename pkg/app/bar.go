package app

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Cells the context gauge on the bar spans
const gaugeCells = 6

// What one open session shows on the bar this frame
type deskState struct {
	ws      *wm.Workspace
	run     *harness.Run
	pending []*harness.Pending
	asked   *harness.Asking
	signal  wm.Signal
}

// Session waiting on the person, served from the field in queue order
type attention struct {
	desk   *desk
	signal wm.Signal
	at     time.Time
	text   string
}

// Lane of every workspace, the first session waiting on the person and the dials
func (m *Model) status() wm.Status {
	now := time.Now()
	states := m.deskStates()
	st := wm.Status{Lanes: map[*wm.Workspace]wm.Lane{}, Dials: m.dials()}
	for _, ws := range m.wm.Workspaces() {
		st.Lanes[ws] = m.lane(ws, states, now)
	}
	if q := m.queue(states); len(q) > 0 {
		st.Notice = wm.Notice{Text: q[0].text, Signal: q[0].signal, More: len(q) - 1, Action: "attention.next"}
	}
	return st
}

// State of every session seated in a workspace
func (m *Model) deskStates() map[*desk]deskState {
	runs := m.app.Control.Runs()
	asked := m.app.Approver.Pending()
	questions := m.app.Questions.Pending()
	active := m.wm.Active()
	out := make(map[*desk]deskState, len(m.desks))
	for _, d := range m.desks {
		ws := m.workspaceOf(d)
		if ws == nil {
			continue
		}
		s := deskState{ws: ws}
		for _, r := range runs {
			if r.Parent == "" && r.Session == d.live.Session {
				s.run = r
			}
		}
		for _, p := range asked {
			if m.app.LiveOfRun(p.Run) == d.live {
				s.pending = append(s.pending, p)
			}
		}
		for _, q := range questions {
			if q.Session == d.live.Session.ID() && s.asked == nil {
				s.asked = q
			}
		}
		s.signal = m.signalOf(d, s, ws == active)
		out[d] = s
	}
	return out
}

// Signal of a session, approvals and questions first, then work going on, then a run ending while out of view
func (m *Model) signalOf(d *desk, s deskState, shown bool) wm.Signal {
	r := s.run
	switch {
	case len(s.pending) > 0 || s.asked != nil:
		return wm.Blocked
	case d.compacting || (r != nil && r.Running()):
		return wm.Working
	case shown || r == nil || !r.Finished().After(d.seen):
		return wm.Idle
	case failed(r.Err()):
		return wm.Failed
	case r.Err() == nil:
		return wm.Fresh
	}
	return wm.Idle
}

// Reports whether a run ended in an error the person did not cause by cancelling it
func failed(err error) bool {
	return err != nil && !errors.Is(err, agent.ErrInterrupted) && !errors.Is(err, context.Canceled)
}

// Chip of a workspace named after its lead session, carrying the strongest signal of its sessions
func (m *Model) lane(ws *wm.Workspace, states map[*desk]deskState, now time.Time) wm.Lane {
	desks := m.desksIn(ws)
	if len(desks) == 0 {
		return wm.Lane{}
	}
	lead := desks[0]
	if d := m.recent[ws]; d != nil && slices.Contains(desks, d) {
		lead = d
	}
	l := wm.Lane{Name: lead.live.Session.Title()}
	for _, d := range desks {
		s := states[d]
		if s.signal > l.Signal || (s.signal == l.Signal && d == lead) {
			l.Signal = s.signal
			l.Detail = m.laneDetail(d, s, now)
		}
	}
	return l
}

// Words after a lane mark, the tool awaiting approval, the question waiting or the tool running and its time
func (m *Model) laneDetail(d *desk, s deskState, now time.Time) string {
	switch s.signal {
	case wm.Blocked:
		if len(s.pending) == 0 {
			return "question"
		}
		return s.pending[0].Request.Call.Name
	case wm.Working:
		if d.compacting {
			return "compacting"
		}
		tool, _ := s.run.LastTool()
		return strings.TrimSpace(tool + " " + render.Elapsed(s.run.Elapsed(now)))
	}
	return ""
}

// Sessions waiting on the person, approvals and questions off screen first, then failures, then finished runs
func (m *Model) queue(states map[*desk]deskState) []attention {
	focused := m.wm.Focused()
	sep := " " + m.theme.Glyphs.Sep + " "
	var out []attention
	for _, d := range m.desks {
		s, ok := states[d]
		if !ok {
			continue
		}
		where := sep + strconv.Itoa(m.wm.IndexOf(s.ws)+1) + " " + sessionName(d.live, m.theme)
		switch s.signal {
		case wm.Blocked:
			if len(s.pending) == 0 {
				if focused == wm.Card(d.session) {
					continue
				}
				out = append(out, attention{desk: d, signal: wm.Blocked, at: s.asked.Asked, text: "answer " + render.Summary(s.asked.Questions[0].Text, 60, m.theme) + where})
				continue
			}
			if focused == wm.Card(d.approval) {
				continue
			}
			p := s.pending[0]
			what := strings.TrimSpace(p.Request.Call.Name + " " + cards.CallSummary(p.Request.Call, "", m.theme))
			out = append(out, attention{desk: d, signal: wm.Blocked, at: p.Asked, text: "approve " + what + where})
		case wm.Failed:
			out = append(out, attention{desk: d, signal: wm.Failed, at: s.run.Finished(), text: "failed " + render.Summary(s.run.Err().Error(), 60, m.theme) + where})
		case wm.Fresh:
			text := "done in " + render.Elapsed(s.run.Elapsed(s.run.Finished()))
			if _, tools := s.run.LastTool(); tools > 0 {
				text += " with " + render.Count(tools, "tool")
			}
			out = append(out, attention{desk: d, signal: wm.Fresh, at: s.run.Finished(), text: text + where})
		}
	}
	for _, st := range m.app.Stuck() {
		out = append(out, attention{signal: wm.Failed, at: st.Since, text: st.Text() + ", every run on it fails until you fix it or switch profile"})
	}
	slices.SortStableFunc(out, func(a, b attention) int {
		if a.signal != b.signal {
			return int(b.signal) - int(a.signal)
		}
		return a.at.Compare(b.at)
	})
	return out
}

// Serves the first session in the queue, its approval, its session card for a question or a run, or the config panel
func (m *Model) dispatch() tea.Cmd {
	states := m.deskStates()
	q := m.queue(states)
	if len(q) == 0 {
		return nil
	}
	d := q[0].desk
	switch {
	case d == nil:
		return m.wm.OpenHere(m.panel("config"))
	case q[0].signal == wm.Blocked && len(states[d].pending) > 0:
		return m.openFrom(d, d.approval)
	}
	return m.wm.Focus(d.session)
}

// Controls at the right of the bar, least important first since the bar drops them from the left
func (m *Model) dials() []wm.Segment {
	g := m.theme.Glyphs
	var out []wm.Segment
	snap := m.app.Meter.Snapshot()
	if snap.Cost > 0 {
		out = append(out, wm.Segment{Text: render.Money(snap.Cost), Style: wm.Dim, Action: "open.metrics"})
	}
	agents := 0
	for _, r := range m.app.Control.Active() {
		if r.Parent != "" {
			agents++
		}
	}
	if agents > 0 {
		out = append(out, wm.Segment{Text: g.Agent + " " + strconv.Itoa(agents), Style: wm.Cool, Action: "open.agents"})
	}
	if jobs := m.runningJobs(); jobs > 0 {
		out = append(out, wm.Segment{Text: g.Jobs + " " + strconv.Itoa(jobs), Style: wm.Cool, Action: "open.jobs"})
	}
	if flows := len(m.app.Flows.Active()); flows > 0 {
		out = append(out, wm.Segment{Text: g.Flow + " " + strconv.Itoa(flows), Style: wm.Cool, Action: "workflow.stop"})
	}
	ep := m.endpoint()
	if snap.Requests > 0 {
		out = append(out, cacheDial(snap))
	}
	if d := m.current(); d != nil {
		out = append(out, m.contextDial(d.live.Session.Context(), ep.Catalog.Context))
	}
	if ep.Model != "" {
		out = append(out, wm.Segment{Text: ep.Model, Style: wm.Cool, Action: "model.switch"})
	}
	md := m.app.Mode.Mode()
	return append(out, wm.Segment{Text: md.Label(), Style: modeStyle(md), Action: "mode.set"})
}

// Gauge of the context against the model window, warming as it fills, the token count when the window is unknown
// Share of the last request read from cache, warm when a repeat request read nothing
func cacheDial(t harness.Telemetry) wm.Segment {
	st := wm.Dim
	if t.CacheRatio == 0 && t.Requests > 1 {
		st = wm.Warm
	}
	return wm.Segment{Text: "cache " + render.Percent(t.CacheRatio), Style: st, Action: "open.metrics"}
}

func (m *Model) contextDial(used, window int) wm.Segment {
	if window <= 0 {
		return wm.Segment{Text: "ctx " + render.Tokens(used), Style: wm.Dim, Action: "open.metrics"}
	}
	share := min(max(float64(used)/float64(window), 0), 1)
	st := wm.Dim
	switch {
	case share >= 0.9:
		st = wm.Bad
	case share >= 0.7:
		st = wm.Warm
	}
	g := m.theme.Glyphs
	filled := int(math.Round(share * gaugeCells))
	gauge := strings.Repeat(g.Full, filled) + strings.Repeat(g.Empty, gaugeCells-filled)
	return wm.Segment{Text: "ctx " + gauge + " " + render.Percent(share), Style: st, Action: "open.metrics"}
}

// Dial color of a permission mode, bypass painted as the hazard it is
func modeStyle(md policy.Mode) wm.SegmentStyle {
	switch md {
	case policy.ModeAcceptEdits:
		return wm.Warm
	case policy.ModePlan:
		return wm.Cool
	case policy.ModeBypass:
		return wm.Bad
	}
	return wm.Dim
}

// Shell jobs still running
func (m *Model) runningJobs() int {
	n := 0
	for _, j := range m.app.Shell.Jobs() {
		if j.Running {
			n++
		}
	}
	return n
}
