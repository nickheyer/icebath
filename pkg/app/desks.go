package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Child run whose card may leave now that it finished
type agentDoneMsg struct{ run string }

// Builds the cards of an open session and gives it a workspace
func (m *Model) addDesk(live *harness.Live) (*desk, tea.Cmd) {
	d := &desk{live: live, agents: map[string]*cards.Agent{}, missions: map[string]*cards.Mission{}}
	d.session = cards.NewSession(m.env, live, m.configDir())
	d.session.Composer.SetCommands(m.slashCommands())
	d.session.Number = func() int { return m.wm.IndexOf(d.ws) + 1 }
	d.approval = cards.NewApproval(m.env, live.Session.ID(), func(run string) bool { return m.app.LiveOfRun(run) == live })
	ws, cmd := m.wm.AddWorkspace(live.Session.ID(), d.session)
	d.ws = ws
	m.desks = append(m.desks, d)
	return d, cmd
}

// Desk of the workspace on the stage
func (m *Model) current() *desk {
	ws := m.wm.Active()
	for _, d := range m.desks {
		if d.ws == ws {
			return d
		}
	}
	return nil
}

func (m *Model) deskOfLive(l *harness.Live) *desk {
	for _, d := range m.desks {
		if d.live == l {
			return d
		}
	}
	return nil
}

func (m *Model) deskOfSession(id string) *desk {
	for _, d := range m.desks {
		if d.live.Session.ID() == id {
			return d
		}
	}
	return nil
}

func (m *Model) deskOfRun(run string) *desk {
	return m.deskOfLive(m.app.LiveOfRun(run))
}

// Findings card of a desk, built on first use
func (m *Model) findingsOf(d *desk) *cards.Findings {
	if d.findings == nil {
		d.findings = cards.NewFindings(m.env, d.live.Facts, d.live.Session.ID())
	}
	return d.findings
}

// Mission card of a desk, built on first use
func (m *Model) missionOf(d *desk, id string) (*cards.Mission, bool) {
	if card, ok := d.missions[id]; ok {
		return card, true
	}
	ms, ok := d.live.Missions.Get(id)
	if !ok {
		return nil, false
	}
	card := cards.NewMission(m.env, d.live.Missions, d.live.Facts, d.live.Session.ID(), ms)
	d.missions[id] = card
	return card, true
}

// Opens the card of a child run in the workspace of its session
func (m *Model) openAgent(id string) tea.Cmd {
	r, ok := m.app.Control.Get(id)
	if !ok || r.Parent == "" {
		return nil
	}
	d := m.deskOfRun(id)
	if d == nil {
		return nil
	}
	card, ok := d.agents[id]
	if !ok {
		card = cards.NewAgent(m.env, r)
		d.agents[id] = card
	}
	return m.openFrom(d, card)
}

// Opens a card of the desk growing out of its session card, so what a session spawns comes from it
func (m *Model) openFrom(d *desk, c wm.Card) tea.Cmd {
	origin, _ := m.wm.FrameOf(d.session)
	return m.wm.OpenFrom(d.ws, c, origin)
}

// Closes the card of a finished child run unless it holds the focus
func (m *Model) retireAgent(id string) tea.Cmd {
	d := m.deskOfRun(id)
	if d == nil {
		return nil
	}
	card, ok := d.agents[id]
	if !ok || m.wm.Focused() == card {
		return nil
	}
	return m.wm.Close(card)
}

// Opens the transcript of a run, the session card for a root and an agent card for a child
func (m *Model) openRun(id string) tea.Cmd {
	r, ok := m.app.Control.Get(id)
	if !ok {
		return m.wm.Toast("no agent "+id, wm.Warn, 0)
	}
	if r.Parent != "" {
		return m.openAgent(id)
	}
	d := m.deskOfRun(id)
	if d == nil {
		return m.wm.Toast("that session is closed", wm.Warn, 0)
	}
	return m.wm.Focus(d.session)
}

// Sends session input, steering the root run when one is going and starting one otherwise
func (m *Model) submit(msg cards.SubmitMsg) tea.Cmd {
	d := m.deskOfSession(msg.Session)
	if d == nil {
		return m.wm.Toast("session closed", wm.Warn, 0)
	}
	if d.compacting {
		return m.wm.Toast("compacting", wm.Warn, 0)
	}
	if m.app.Busy(d.live) {
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" && r.Session == d.live.Session {
				if err := m.app.Control.Steer(r.ID, msg.Parts...); err != nil {
					return m.wm.Toast(err.Error(), wm.Error, 0)
				}
				return m.wm.Toast("queued", wm.Info, 0)
			}
		}
	}
	if d.live.Session.Title() == "" && msg.Text != "" {
		d.live.Session.SetTitle(render.Summary(msg.Text, 60, m.theme))
	}
	if _, err := m.app.RunOn(m.ctx, d.live, d.mission, msg.Parts); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.state = tea.ProgramStateWorking
	d.ws.SetBusy(true)
	return m.wm.Focus(d.session)
}

func (m *Model) startMission(d *desk, objective string) tea.Cmd {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(objective) == "" {
		return m.wm.Toast("no objective", wm.Warn, 0)
	}
	if d.compacting {
		return m.wm.Toast("compacting", wm.Warn, 0)
	}
	ms, err := d.live.Missions.Create(objective)
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	d.mission = ms.ID
	if d.live.Session.Title() == "" {
		d.live.Session.SetTitle(render.Summary(objective, 60, m.theme))
	}
	if _, err := m.app.RunOn(m.ctx, d.live, ms.ID, []protocol.Part{protocol.Text{Text: objective}}); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.state = tea.ProgramStateWorking
	d.ws.SetBusy(true)
	cmds := []tea.Cmd{m.wm.Toast("mission "+ms.ID+" started", wm.Success, 0)}
	if card, ok := m.missionOf(d, ms.ID); ok {
		cmds = append(cmds, m.openFrom(d, card))
	}
	return tea.Batch(cmds...)
}

// Starts a fresh session in the workspace of the desk, storing the one it had
func (m *Model) clearDesk(d *desk) tea.Cmd {
	if d == nil {
		return nil
	}
	if m.app.Busy(d.live) {
		return m.wm.Toast("agent still working", wm.Warn, 0)
	}
	if d.compacting {
		return m.wm.Toast("compacting", wm.Warn, 0)
	}
	live, err := m.app.Create()
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	old := d.live
	var cmds []tea.Cmd
	for _, c := range d.agents {
		cmds = append(cmds, m.wm.Close(c))
	}
	for _, c := range d.missions {
		cmds = append(cmds, m.wm.Close(c))
	}
	if d.findings != nil {
		cmds = append(cmds, m.wm.Close(d.findings))
	}
	cmds = append(cmds, m.wm.Close(d.approval))
	d.live = live
	d.ws.ID = live.Session.ID()
	d.session.Rebind(live)
	d.approval = cards.NewApproval(m.env, live.Session.ID(), func(run string) bool { return m.app.LiveOfRun(run) == live })
	d.agents, d.missions, d.findings, d.mission = map[string]*cards.Agent{}, map[string]*cards.Mission{}, nil, ""
	if err := m.app.Release(old); err != nil {
		cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
	}
	if d == m.current() {
		m.app.Select(live)
	}
	m.wm.Repaint()
	return tea.Batch(append(cmds, m.wm.Toast("cleared, "+old.Session.ID()+" stored", wm.Success, 0))...)
}

// Opens a fresh session in a new workspace and shows it
func (m *Model) newWorkspace() tea.Cmd {
	live, err := m.app.Create()
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	return m.showDesk(live)
}

// Gives an open session a workspace when it has none and switches to it
func (m *Model) showDesk(live *harness.Live) tea.Cmd {
	if d := m.deskOfLive(live); d != nil {
		return m.wm.Switch(m.wm.IndexOf(d.ws))
	}
	d, cmd := m.addDesk(live)
	return tea.Batch(cmd, m.wm.Switch(m.wm.IndexOf(d.ws)))
}

// Opens stored sessions each in a workspace, switching to one that already has a workspace, the last shown
func (m *Model) openStored(ids ...string) tea.Cmd {
	var cmds []tea.Cmd
	var titles []string
	for _, id := range ids {
		live, err := m.app.Open(m.ctx, id)
		if err != nil {
			cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
			continue
		}
		cmds = append(cmds, m.showDesk(live))
		titles = append(titles, sessionName(live, m.theme)+" on "+live.Endpoint.Model)
	}
	switch len(titles) {
	case 0:
	case 1:
		cmds = append(cmds, m.wm.Toast("resumed "+titles[0], wm.Success, 0))
	default:
		cmds = append(cmds, m.wm.Toast(fmt.Sprintf("resumed %d sessions", len(titles)), wm.Success, 0))
	}
	return tea.Batch(cmds...)
}

// Title of a session, its id while it has none
func sessionName(l *harness.Live, t theme.Theme) string {
	if title := l.Session.Title(); title != "" {
		return render.Summary(title, 40, t)
	}
	return l.Session.ID()
}

// Forks stored sessions each into a new workspace as a copy under a new id
func (m *Model) forkStored(ids ...string) tea.Cmd {
	var cmds []tea.Cmd
	var made []string
	for _, id := range ids {
		src, err := m.app.Open(m.ctx, id)
		if err != nil {
			cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
			continue
		}
		f, err := m.forkLive(m.deskOfLive(src), src, src.Session.Len())
		if err != nil {
			cmds = append(cmds, m.wm.Toast(err.Error(), wm.Warn, 0))
			continue
		}
		cmds = append(cmds, m.showDesk(f))
		made = append(made, sessionName(src, m.theme)+" into "+f.Session.ID())
	}
	switch len(made) {
	case 0:
	case 1:
		cmds = append(cmds, m.wm.Toast("forked "+made[0], wm.Success, 0))
	default:
		cmds = append(cmds, m.wm.Toast(fmt.Sprintf("forked %d sessions", len(made)), wm.Success, 0))
	}
	return tea.Batch(cmds...)
}

// Forks an open session cut at n entries into a new workspace
func (m *Model) forkAt(d *desk, src *harness.Live, n int) tea.Cmd {
	f, err := m.forkLive(d, src, n)
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Warn, 0)
	}
	return tea.Batch(m.showDesk(f), m.wm.Toast(fmt.Sprintf("forked into %s at %d entries", f.Session.ID(), n), wm.Success, 0))
}

// Fork of an open session cut at n entries, releasing a source that was opened only to be forked
func (m *Model) forkLive(d *desk, src *harness.Live, n int) (*harness.Live, error) {
	if d != nil && d.compacting {
		return nil, errors.New("compacting")
	}
	f, err := m.app.Fork(src, n)
	if err != nil {
		return nil, err
	}
	if d == nil {
		m.app.Release(src)
	}
	return f, nil
}

// Deletes stored sessions that have no workspace open, with the prompt drafts kept for them
func (m *Model) deleteStored(ids ...string) tea.Cmd {
	var cmds []tea.Cmd
	var gone []string
	for _, id := range ids {
		if m.app.LiveByID(id) != nil {
			cmds = append(cmds, m.wm.Toast(id+" is open", wm.Warn, 0))
			continue
		}
		if err := m.app.Store.Delete(m.ctx, id); err != nil {
			cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
			continue
		}
		if err := os.Remove(cards.DraftPath(m.configDir(), id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
		}
		gone = append(gone, id)
	}
	m.sessions.Reload()
	switch len(gone) {
	case 0:
	case 1:
		cmds = append(cmds, m.wm.Toast("deleted "+gone[0], wm.Success, 0))
	default:
		cmds = append(cmds, m.wm.Toast(fmt.Sprintf("deleted %d sessions", len(gone)), wm.Success, 0))
	}
	return tea.Batch(cmds...)
}

// Closes the workspace on the stage, writing its session to disk
func (m *Model) closeWorkspace() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	if len(m.desks) == 1 {
		return m.wm.Toast("last workspace", wm.Warn, 0)
	}
	if m.app.Busy(d.live) {
		return m.wm.Toast("agent still working", wm.Warn, 0)
	}
	if err := m.app.Release(d.live); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	i := m.wm.IndexOf(d.ws)
	kept := m.desks[:0]
	for _, have := range m.desks {
		if have != d {
			kept = append(kept, have)
		}
	}
	m.desks = kept
	return m.wm.RemoveWorkspace(i)
}
