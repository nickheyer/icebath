package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
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

// Builds the cards of an open session without placing any of them
func (m *Model) addDesk(live *harness.Live) *desk {
	d := &desk{live: live, agents: map[string]*cards.Agent{}, missions: map[string]*cards.Mission{}}
	d.session = cards.NewSession(m.env, live, m.configDir())
	d.session.Composer.SetCommands(m.slashCommands())
	d.session.Composer.Volatile = func() bool { return d.live.Endpoint.Profile.Ghost }
	d.session.Locked = func() bool { return d.compacting }
	d.approval = cards.NewApproval(m.env, live.Session.ID(), func(run string) bool { return m.app.LiveOfRun(run) == live })
	m.desks = append(m.desks, d)
	return d
}

// Id for the next workspace
func (m *Model) workspaceID() string {
	m.spaces++
	return "w" + strconv.Itoa(m.spaces)
}

// Workspace holding the session card of the desk, nil while it is not open
func (m *Model) workspaceOf(d *desk) *wm.Workspace {
	if d == nil {
		return nil
	}
	return m.wm.WorkspaceOf(d.session)
}

// Desks whose session cards sit in the workspace
func (m *Model) desksIn(ws *wm.Workspace) []*desk {
	var out []*desk
	for _, d := range m.desks {
		if ws != nil && m.workspaceOf(d) == ws {
			out = append(out, d)
		}
	}
	return out
}

// Desk of the focused card, else the last focused in the workspace on the stage, else its first
func (m *Model) current() *desk {
	ws := m.wm.Active()
	if ws == nil {
		return nil
	}
	if d := m.deskOfCard(ws.Focused()); d != nil {
		m.recent[ws] = d
		return d
	}
	if d := m.recent[ws]; d != nil && m.workspaceOf(d) == ws {
		return d
	}
	for _, c := range ws.Cards() {
		if d := m.deskOfCard(c); d != nil {
			return d
		}
	}
	return nil
}

// Desk whose session card or one of the cards grown from it is c
func (m *Model) deskOfCard(c wm.Card) *desk {
	if c == nil {
		return nil
	}
	for _, d := range m.desks {
		if c == wm.Card(d.session) || c == wm.Card(d.approval) || (d.findings != nil && c == wm.Card(d.findings)) {
			return d
		}
		for _, a := range d.agents {
			if c == wm.Card(a) {
				return d
			}
		}
		for _, ms := range d.missions {
			if c == wm.Card(ms) {
				return d
			}
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

// Opens the card of a child run beside the card of its session
func (m *Model) openAgent(id string) tea.Cmd {
	if d, card := m.agentCard(id); card != nil {
		return m.openFrom(d, card)
	}
	return nil
}

// Desk and card of a child run, the card built on first use, nil for a root or unknown run
func (m *Model) agentCard(id string) (*desk, *cards.Agent) {
	r, ok := m.app.Control.Get(id)
	if !ok || r.Parent == "" {
		return nil, nil
	}
	d := m.deskOfRun(id)
	if d == nil {
		return nil, nil
	}
	card, ok := d.agents[id]
	if !ok {
		card = cards.NewAgent(m.env, r, d.live.ShowsThinking())
		d.agents[id] = card
	}
	return d, card
}

// Loads every transcript of the desk again as its session now shows or hides thinking
func (m *Model) rethink(d *desk) {
	d.session.Rethink()
	for _, a := range d.agents {
		a.Rethink(d.live.ShowsThinking())
	}
	m.wm.Repaint()
}

// Opens a card of the desk beside its session card and growing out of it, switching to its workspace
func (m *Model) openFrom(d *desk, c wm.Card) tea.Cmd {
	ws, origin := m.seat(d)
	return m.wm.OpenFrom(ws, c, origin)
}

// Opens a card of the desk like openFrom without leaving the stage, the bar raising it while out of view
func (m *Model) openAside(d *desk, c wm.Card) tea.Cmd {
	ws, origin := m.seat(d)
	return m.wm.OpenAside(ws, c, origin)
}

// Workspace a card of the desk opens in and the frame it grows out of
func (m *Model) seat(d *desk) (*wm.Workspace, wm.Box) {
	ws := m.workspaceOf(d)
	if ws == nil {
		ws = m.wm.Active()
	}
	origin, _ := m.wm.FrameOf(d.session)
	return ws, origin
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
	cmds := []tea.Cmd{m.wm.Toast("mission "+ms.ID+" started", wm.Success, 0)}
	if card, ok := m.missionOf(d, ms.ID); ok {
		cmds = append(cmds, m.openFrom(d, card))
	}
	return tea.Batch(cmds...)
}

// Starts a fresh session in the card of the desk, storing the one it had
func (m *Model) clearDesk(d *desk) tea.Cmd {
	if d == nil {
		return nil
	}
	if cmd := m.held(d); cmd != nil {
		return cmd
	}
	live, err := m.app.Create()
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	old := d.live
	cmds := m.closeGrown(d)
	d.live = live
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

// Opens a workspace holding a fresh session and shows it
func (m *Model) newWorkspace() tea.Cmd {
	live, err := m.app.Create()
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	d := m.addDesk(live)
	ws, cmd := m.wm.AddWorkspace(m.workspaceID(), d.session)
	return tea.Batch(cmd, m.wm.Switch(m.wm.IndexOf(ws)))
}

// Opens a fresh session in the workspace on the stage
func (m *Model) newSession() tea.Cmd {
	live, err := m.app.Create()
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	return m.openDesk(nil, live)
}

// Desk on the stage, a fresh session opened there when it holds none
func (m *Model) ensureDesk() (*desk, tea.Cmd) {
	if d := m.current(); d != nil {
		return d, nil
	}
	live, err := m.app.Create()
	if err != nil {
		return nil, m.wm.Toast(err.Error(), wm.Error, 0)
	}
	cmd := m.openDesk(nil, live)
	return m.deskOfLive(live), cmd
}

// Focuses the card of an open session, opening it in the workspace, the stage when nil, when it has none
func (m *Model) openDesk(ws *wm.Workspace, live *harness.Live) tea.Cmd {
	if d := m.deskOfLive(live); d != nil {
		return m.wm.Focus(d.session)
	}
	d := m.addDesk(live)
	if ws == nil {
		ws = m.wm.Active()
	}
	if ws == nil {
		ws, cmd := m.wm.AddWorkspace(m.workspaceID(), d.session)
		return tea.Batch(cmd, m.wm.Switch(m.wm.IndexOf(ws)))
	}
	return m.wm.Open(ws, d.session)
}

// Opens stored sessions in the workspace on the stage, focusing one that is open already, the last one shown
func (m *Model) openStored(ids ...string) tea.Cmd {
	var cmds []tea.Cmd
	var titles []string
	for _, id := range ids {
		live, err := m.app.Open(m.ctx, id)
		if err != nil {
			cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
			continue
		}
		cmds = append(cmds, m.openDesk(nil, live))
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

// Forks stored sessions each into the workspace on the stage as a copy under a new id
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
		cmds = append(cmds, m.openDesk(nil, f))
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

// Forks an open session cut at n entries into a card beside it
func (m *Model) forkAt(d *desk, src *harness.Live, n int) tea.Cmd {
	f, err := m.forkLive(d, src, n)
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Warn, 0)
	}
	return tea.Batch(m.openDesk(m.workspaceOf(d), f), m.wm.Toast(fmt.Sprintf("forked into %s at %d entries", f.Session.ID(), n), wm.Success, 0))
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

// Deletes stored sessions that have no card open, with the prompt drafts kept for them
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

// Toast for a session that cannot be let go of right now, nil when it can
func (m *Model) held(d *desk) tea.Cmd {
	switch {
	case m.app.Busy(d.live):
		return m.wm.Toast("agent still working", wm.Warn, 0)
	case d.compacting:
		return m.wm.Toast("compacting", wm.Warn, 0)
	}
	return nil
}

// Closes the workspace on the stage, storing every session in it
func (m *Model) closeWorkspace() tea.Cmd {
	ws := m.wm.Active()
	if ws == nil {
		return nil
	}
	if len(m.wm.Workspaces()) == 1 {
		return m.wm.Toast("last workspace", wm.Warn, 0)
	}
	seated := m.desksIn(ws)
	for _, d := range seated {
		if cmd := m.held(d); cmd != nil {
			return cmd
		}
	}
	var cmds []tea.Cmd
	var stored []string
	for _, d := range seated {
		cmds = append(cmds, m.releaseDesk(d)...)
		if d.live.Session.Len() > 0 && !m.app.Ghosted(d.live.Session) {
			stored = append(stored, sessionName(d.live, m.theme))
		}
	}
	delete(m.recent, ws)
	switch len(stored) {
	case 0:
	case 1:
		cmds = append(cmds, m.wm.Toast("stored "+stored[0], wm.Info, 0))
	default:
		cmds = append(cmds, m.wm.Toast(fmt.Sprintf("stored %d sessions", len(stored)), wm.Info, 0))
	}
	return tea.Batch(append(cmds, m.wm.RemoveWorkspace(m.wm.IndexOf(ws)))...)
}

// Closes the session card of the desk, storing the session
func (m *Model) closeSession(d *desk) tea.Cmd {
	if d == nil {
		return nil
	}
	if cmd := m.held(d); cmd != nil {
		return cmd
	}
	cmds := append(m.releaseDesk(d), m.wm.Close(d.session), m.storedToast(d))
	return tea.Batch(cmds...)
}

// Stores and forgets the session of a card the manager closed, putting the card back while a run holds it
func (m *Model) sessionClosed(id string) tea.Cmd {
	d := m.deskOfSession(id)
	if d == nil {
		return nil
	}
	if cmd := m.held(d); cmd != nil {
		return tea.Batch(cmd, m.wm.OpenHere(d.session))
	}
	return tea.Batch(append(m.releaseDesk(d), m.storedToast(d))...)
}

// Toast naming the stored session, nil for one holding nothing or ghost and so not written
func (m *Model) storedToast(d *desk) tea.Cmd {
	if d.live.Session.Len() == 0 || m.app.Ghosted(d.live.Session) {
		return nil
	}
	return m.wm.Toast("stored "+sessionName(d.live, m.theme), wm.Info, 0)
}

// Closes the cards grown from the session card of the desk
func (m *Model) closeGrown(d *desk) []tea.Cmd {
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
	return append(cmds, m.wm.Close(d.approval))
}

// Closes the cards grown from the desk, writes its session to disk and forgets it
func (m *Model) releaseDesk(d *desk) []tea.Cmd {
	cmds := m.closeGrown(d)
	if err := m.app.Release(d.live); err != nil {
		cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
	}
	m.desks = slices.DeleteFunc(m.desks, func(have *desk) bool { return have == d })
	for ws, r := range m.recent {
		if r == d {
			delete(m.recent, ws)
		}
	}
	return cmds
}
