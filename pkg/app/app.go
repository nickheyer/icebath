// Terminal application that composes the harness, the window manager and the cards, with a headless runner for scripts
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Layout file written beside the sessions
const layoutFile = "layout.json"

// What the app keeps between runs, the manager layout and the mission each session works on
type layoutState struct {
	wm.Layout
	// Mission the runs of a session attach to, by session id
	Missions map[string]string `json:"missions,omitempty"`
}

// Seconds a finished agent card stays open before it leaves on its own
const agentLinger = 4 * time.Second

// Window for a second interrupt to quit after one cancelled a run
const quitWindow = 1500 * time.Millisecond

// How often the keybind file is checked for edits made outside the app
const keysPoll = 2 * time.Second

// Options the shell passes beside the configuration
type Options struct {
	Config  harness.Config
	Resume  string
	Dir     string
	Version string
}

// Open session with the cards built for it, seated in whichever workspace holds its session card
type desk struct {
	live       *harness.Live
	session    *cards.Session
	approval   *cards.Approval
	agents     map[string]*cards.Agent
	missions   map[string]*cards.Mission
	findings   *cards.Findings
	mission    string
	compacting bool
	// Last moment its workspace was on the stage, runs ending later wait in the field
	seen time.Time
}

// Bubble Tea model running the harness behind the window manager
type Model struct {
	opts    Options
	app     *harness.App
	relay   *relay
	wm      *wm.Model
	env     *cards.Env
	keys    *keys.Map
	keysErr error
	theme   theme.Theme
	desks   []*desk
	// Desk last focused in each workspace, which session bound commands act on while a panel holds the focus
	recent map[*wm.Workspace]*desk
	// Workspaces opened so far, numbering the next id
	spaces   int
	metrics  *cards.Telemetry
	files    *cards.Files
	jobs     *cards.Jobs
	sessions *cards.Sessions
	config   *cards.Config
	roster   *cards.Agents
	ctx      context.Context
	cancel   context.CancelFunc
	started  time.Time
	state    tea.ProgramState
	cancelAt time.Time
	models   []profiles.Model
	listed   string
	quitting bool
	width    int
	height   int
	bg       sync.WaitGroup
	// Layout as last written, so only a change costs a write
	layoutSaved []byte
	// Commands the restored cards asked for, run when the program starts
	startup []tea.Cmd
	// Workspaces of the last run that could not come back, shown when the program starts
	notices []string
}

// Builds the application, opening providers and stores from the configuration
func New(ctx context.Context, opts Options) (*Model, error) {
	ctx, cancel := context.WithCancel(ctx)
	r := &relay{}
	app, err := harness.Build(ctx, opts.Config, r.Send, harness.BuildOptions{Resume: opts.Resume, Dir: opts.Dir})
	if err != nil {
		cancel()
		return nil, err
	}
	layout, raw, saved := loadLayout(layoutPath(app.Config))
	th, err := resolveTheme(opts.Config, layout.Theme)
	if err != nil {
		cancel()
		return nil, err
	}
	m := &Model{opts: opts, app: app, relay: r, theme: th, ctx: ctx, cancel: cancel, started: time.Now(), state: tea.ProgramStateIdle, layoutSaved: raw, recent: map[*wm.Workspace]*desk{}}
	m.keys, m.keysErr = keys.Open(filepath.Join(m.configDir(), keys.FileName))
	m.env = m.environment()
	m.buildManager(layout.Layout, saved)
	m.buildCards()
	m.restoreDesks(layout)
	m.wm.SetCommands(m.commands())
	m.wm.SetBar(m.status)
	m.retitle()
	return m, nil
}

// Theme the configuration names, else the one saved with the layout, else a default fit for the terminal
func resolveTheme(cfg harness.Config, saved string) (theme.Theme, error) {
	st := theme.Style{Palette: cfg.UI.Theme}
	if _, ok := theme.Named(saved); ok && st.Palette == "" {
		st.Palette = saved
	}
	th, err := st.Resolve()
	if err != nil {
		return theme.Theme{}, err
	}
	if st.Palette == "" {
		if term := strings.ToLower(os.Getenv("TERM")); term == "dumb" || term == "linux" || os.Getenv("NO_COLOR") != "" {
			th = theme.Plain()
		}
	}
	return th, nil
}

func (m *Model) environment() *cards.Env {
	a := m.app
	return &cards.Env{
		Theme: m.theme, Keys: m.keys, Control: a.Control, Meter: a.Meter, Work: a.Workspace, Approver: a.Approver, PreImage: a.PreImage, MCP: a.MCP, Shell: a.Shell, Tools: a.Tools, Mode: a.Mode, Store: a.Store, Files: a.Files,
		Journal: a.Journal, Editor: a, Ctx: m.ctx,
		Lives:   a.Lives,
		Primary: m.endpoint,
		Models: func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error) {
			prov, err := p.Open(ctx)
			if err != nil {
				return nil, err
			}
			return p.ListModels(ctx, prov)
		},
		Settings:   m.settings,
		ProjectDir: filepath.Join(a.Root, harness.ProjectDir),
		Now:        time.Now,
	}
}

// Endpoint of the session on the stage, the default while none is
func (m *Model) endpoint() harness.Endpoint {
	if d := m.current(); d != nil {
		return d.live.Endpoint
	}
	return m.app.Primary
}

func (m *Model) configDir() string {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return m.app.Root
	}
	return dir
}

// Setting values as they stand, empty for those left unset
func (m *Model) settings() map[string]string {
	cfg := m.app.Config
	out := map[string]string{"theme": m.theme.Name, "motion": fmt.Sprintf("%.2f", m.wm.Motion()), "cascade": fmt.Sprintf("%t", m.wm.Cascade())}
	if cfg.MaxTokens > 0 {
		out["max_tokens"] = fmt.Sprint(cfg.MaxTokens)
	}
	if cfg.MaxRounds > 0 {
		out["max_rounds"] = fmt.Sprint(cfg.MaxRounds)
	}
	if cfg.Compaction.Threshold > 0 {
		out["compact_threshold"] = fmt.Sprint(cfg.Compaction.Threshold)
	}
	if cfg.Temperature != nil {
		out["temperature"] = fmt.Sprintf("%.2f", *cfg.Temperature)
	}
	return out
}

func (m *Model) buildManager(layout wm.Layout, saved bool) {
	cfg := m.opts.Config
	opts := []wm.Option{wm.WithTheme(m.theme), wm.WithWindowTitle("icebath"), wm.WithKeys(m.keys)}
	if cfg.UI.Motion != nil {
		opts = append(opts, wm.WithMotion(*cfg.UI.Motion))
	} else if m.theme.Plain {
		opts = append(opts, wm.WithMotion(0))
	}
	if cfg.UI.Ratio > 0 {
		opts = append(opts, wm.WithRatio(cfg.UI.Ratio))
	}
	if cfg.UI.Cascade != nil {
		opts = append(opts, wm.WithCascade(*cfg.UI.Cascade))
	}
	m.wm = wm.New(opts...)
	m.wm.OnToast(func(text string, level wm.Level) {
		m.app.Journal.Add(journalLevel(level), "notice", text)
	})
	m.wm.SetEmptyHints("session.new", "open", "palette")
	if saved {
		m.wm.Restore(layout)
	}
}

// Severity in the log for a toast of the level
func journalLevel(l wm.Level) string {
	switch l {
	case wm.Warn:
		return harness.LevelWarn
	case wm.Error:
		return harness.LevelError
	}
	return harness.LevelInfo
}

func (m *Model) buildCards() {
	env := m.env
	m.metrics = cards.NewTelemetry(env)
	m.files = cards.NewFiles(env)
	m.jobs = cards.NewJobs(env)
	m.sessions = cards.NewSessions(env)
	m.config = cards.NewConfig(env)
	m.roster = cards.NewAgents(env)
}

// Panels shared by every workspace, by key
func (m *Model) panel(key string) wm.Card {
	switch key {
	case "metrics":
		return m.metrics
	case "files":
		return m.files
	case "jobs":
		return m.jobs
	case "sessions":
		return m.sessions
	case "config":
		return m.config
	case "agents":
		return m.roster
	}
	return nil
}

// Card a saved key names, nil for one of a closed session or a run of the last process
func (m *Model) cardFor(key string) wm.Card {
	if c := m.panel(key); c != nil {
		return c
	}
	kind, rest, _ := strings.Cut(key, ":")
	switch kind {
	case "session":
		if d := m.deskOfSession(rest); d != nil {
			return d.session
		}
	case "findings":
		if d := m.deskOfSession(rest); d != nil {
			return m.findingsOf(d)
		}
	case "mission":
		sess, id, ok := strings.Cut(rest, ":")
		if !ok {
			return nil
		}
		if d := m.deskOfSession(sess); d != nil {
			if card, ok := m.missionOf(d, id); ok {
				return card
			}
		}
	}
	return nil
}

// Ids of the sessions a saved workspace holds cards of, tiles before floats
func sessionKeys(wl wm.WorkspaceLayout) []string {
	var out []string
	add := func(key string) {
		if id, ok := strings.CutPrefix(key, "session:"); ok && id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, t := range wl.Tiles {
		add(t.Key)
	}
	for _, f := range wl.Floats {
		add(f.Key)
	}
	return out
}

// Reopens the workspaces of the last run with their sessions, the resumed or a fresh one standing in when none return
func (m *Model) restoreDesks(l layoutState) {
	first := m.app.Current()
	for _, wl := range l.Workspaces {
		ws, cmd := m.wm.AddWorkspace(m.workspaceID())
		m.startup = append(m.startup, cmd)
		for _, id := range sessionKeys(wl) {
			if m.deskOfSession(id) != nil {
				continue
			}
			live, err := m.app.Open(m.ctx, id)
			if errors.Is(err, session.ErrNotFound) {
				live, err = m.app.Revive(id)
			}
			if err != nil {
				m.notices = append(m.notices, "session "+id+": "+err.Error())
				continue
			}
			d := m.addDesk(live)
			if mid := l.Missions[id]; mid != "" {
				if ms, ok := live.Missions.Get(mid); ok && !ms.Ended() {
					d.mission = mid
				}
			}
		}
		m.startup = append(m.startup, m.wm.RestoreWorkspace(ws, wl, m.cardFor))
	}
	resumed := m.opts.Resume != ""
	switch {
	case len(m.wm.Workspaces()) == 0 || (resumed && m.deskOfLive(first) == nil):
		d := m.deskOfLive(first)
		if d == nil {
			d = m.addDesk(first)
		}
		_, cmd := m.wm.AddWorkspace(m.workspaceID(), d.session)
		m.startup = append(m.startup, cmd)
	case m.deskOfLive(first) == nil:
		if err := m.app.Release(first); err != nil {
			m.notices = append(m.notices, err.Error())
		}
	}
	active := 0
	if l.Active >= 0 && l.Active < len(m.wm.Workspaces()) {
		active = l.Active
	}
	if resumed {
		d := m.deskOfLive(first)
		active = m.wm.IndexOf(m.workspaceOf(d))
		m.startup = append(m.startup, m.wm.Focus(d.session))
	}
	m.startup = append(m.startup, m.wm.Switch(active))
	if d := m.current(); d != nil {
		m.app.Select(d.live)
	}
}

// Layout file beside the session directory, empty when the configuration cannot place one
func layoutPath(cfg harness.Config) string {
	dir, err := cfg.SessionsPath()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), layoutFile)
}

// Layout read from the path with its bytes, reporting whether a readable one was there
func loadLayout(path string) (layoutState, []byte, bool) {
	var l layoutState
	if path == "" {
		return l, nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || unmarshal(data, &l) != nil {
		return layoutState{}, nil, false
	}
	return l, data, true
}

// Layout as it stands with the mission of every desk
func (m *Model) layoutState() layoutState {
	l := layoutState{Layout: m.wm.Layout()}
	l.Theme = m.theme.Name
	for _, d := range m.desks {
		if d.mission != "" {
			if l.Missions == nil {
				l.Missions = map[string]string{}
			}
			l.Missions[d.live.Session.ID()] = d.mission
		}
	}
	return l
}

// Writes the layout when it differs from the one last written, logging a write that fails
func (m *Model) saveLayout() {
	path := layoutPath(m.app.Config)
	if path == "" {
		return
	}
	data, err := marshal(m.layoutState())
	if err != nil {
		m.app.Journal.Add(harness.LevelError, "layout", err.Error())
		return
	}
	if bytes.Equal(data, m.layoutSaved) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		m.app.Journal.Add(harness.LevelError, "layout", err.Error())
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		m.app.Journal.Add(harness.LevelError, "layout", err.Error())
		return
	}
	m.layoutSaved = data
}

// Starts the manager and restored cards, lists models, watches the keybind file and fires the session start hooks
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.wm.Init(), m.listModels(true), m.watchKeys(), func() tea.Msg {
		if err := m.app.SessionStart(m.ctx, "startup"); err != nil {
			return errMsg{err}
		}
		return nil
	}}
	cmds = append(cmds, m.startup...)
	m.startup = nil
	if m.keysErr != nil {
		cmds = append(cmds, m.wm.Toast("keybinds: "+m.keysErr.Error(), wm.Error, 8*time.Second))
	}
	for _, text := range m.notices {
		cmds = append(cmds, m.wm.Toast(text, wm.Warn, 8*time.Second))
	}
	m.notices = nil
	return tea.Batch(cmds...)
}

// Time to look at the keybind file again
type keysTickMsg struct{}

// Schedules the next look at the keybind file
func (m *Model) watchKeys() tea.Cmd {
	return tea.Tick(keysPoll, func(time.Time) tea.Msg { return keysTickMsg{} })
}

// Reads the keybind file again when it was edited outside the app, telling every card
func (m *Model) reloadKeys() tea.Cmd {
	if !m.keys.Stale() {
		return nil
	}
	changed := func() tea.Msg { return keys.ChangedMsg{} }
	if err := m.keys.Reload(); err != nil {
		return tea.Batch(changed, m.wm.Toast("keybinds: "+err.Error(), wm.Error, 8*time.Second))
	}
	return tea.Batch(changed, m.wm.Toast("keybinds reloaded", wm.Success, 0))
}

// Attaches the running program so harness messages reach it
func (m *Model) Attach(p *tea.Program) { m.relay.Attach(p) }

type errMsg struct{ err error }

// Handles the message, then writes the layout when anything but a frame changed it
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	if _, frame := msg.(anim.FrameMsg); !frame && !m.quitting {
		m.saveLayout()
	}
	return model, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case keysTickMsg:
		return m, tea.Batch(m.reloadKeys(), m.watchKeys())
	case errMsg:
		if msg.err != nil {
			return m, m.wm.Toast(msg.err.Error(), wm.Error, 0)
		}
		return m, nil
	case startMsg:
		d, opened := m.ensureDesk()
		if d == nil {
			return m, opened
		}
		return m, tea.Batch(opened, m.submit(cards.SubmitMsg{Session: d.live.Session.ID(), Parts: []protocol.Part{protocol.Text{Text: msg.text}}, Text: msg.text}))
	case startMissionMsg:
		d, opened := m.ensureDesk()
		return m, tea.Batch(opened, m.startMission(d, msg.objective))
	case compactedMsg:
		msg.desk.compacting = false
		if msg.err != nil {
			return m, m.wm.Toast(msg.err.Error(), wm.Error, 0)
		}
		msg.desk.session.Reload()
		m.wm.Repaint()
		return m, m.wm.Toast("compacted", wm.Success, 0)
	case profileMsg:
		return m, m.adoptProfile(msg)
	case jobKilledMsg:
		return m, m.wm.Toast("killed "+msg.job, wm.Warn, 0)
	case modelsMsg:
		if msg.err == nil {
			m.models = msg.list
		}
		if msg.quiet {
			return m, nil
		}
		return m, m.pickModel(msg)
	case agentDoneMsg:
		return m, m.retireAgent(msg.run)
	}
	var cmds []tea.Cmd
	if cmd, handled := m.harnessMsg(msg); handled {
		cmds = append(cmds, cmd)
	} else if cmd, handled := m.cardMsg(msg); handled {
		m.wm.Repaint()
		return m, cmd
	}
	_, cmd := m.wm.Update(msg)
	cmds = append(cmds, cmd, m.sync())
	return m, tea.Batch(cmds...)
}

// Keeps the harness selection, title, model list and what has been seen in step with the session on the stage
func (m *Model) sync() tea.Cmd {
	if m.wm.Active() == nil {
		return nil
	}
	now := time.Now()
	for _, d := range m.desksIn(m.wm.Active()) {
		d.seen = now
	}
	if d := m.current(); d != nil && m.app.Session != d.live.Session {
		m.app.Select(d.live)
	}
	m.retitle()
	if m.endpoint().Profile.Name != m.listed {
		return m.listModels(true)
	}
	return nil
}

// Program filter that runs the shutdown before any quit message, whichever card or signal raised it
func (m *Model) Filter(_ tea.Model, msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.QuitMsg); ok {
		m.shutdown()
	}
	return msg
}

func (m *Model) shutdown() tea.Cmd {
	if m.quitting {
		return tea.Quit
	}
	m.quitting = true
	m.saveLayout()
	m.cancel()
	m.relay.Close()
	m.bg.Wait()
	m.app.Close(context.Background())
	return tea.Quit
}

// Cancels the run of the session on the stage, quitting when nothing runs or when pressed again soon after
func (m *Model) interrupt() tea.Cmd {
	d := m.current()
	if d != nil && m.app.Busy(d.live) && !m.quitting && time.Since(m.cancelAt) > quitWindow {
		m.cancelAt = time.Now()
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" && r.Session == d.live.Session {
				m.app.Control.Cancel(r.ID)
			}
		}
		again := "interrupt"
		if k := m.keys.Display("interrupt"); k != "" {
			again = k
		}
		return m.wm.Toast("cancelled, "+again+" again quits", wm.Warn, 0)
	}
	return m.shutdown()
}

// Routes bridge messages to app state and reports whether the message was one
func (m *Model) harnessMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case harness.ApprovalMsg:
		m.state = tea.ProgramStateBlocked
		d := m.deskOfRun(msg.Pending.Run)
		if d == nil {
			return nil, true
		}
		return m.openAside(d, d.approval), true
	case harness.ApprovalDoneMsg:
		if m.app.Control.Busy() {
			m.state = tea.ProgramStateWorking
		}
		return nil, true
	case harness.RunDoneMsg:
		m.state = tea.ProgramStateDone
		if msg.Err != nil {
			m.state = tea.ProgramStateError
		}
		if !failed(msg.Err) {
			return nil, true
		}
		cmds := []tea.Cmd{m.wm.Toast(render.Truncate(msg.Err.Error(), 80, m.theme), wm.Error, 6*time.Second)}
		if d := m.deskOfRun(msg.Run); d != nil {
			cmds = append(cmds, m.wm.Attention(d.session, wm.Alarm))
		}
		return tea.Batch(cmds...), true
	case harness.EventMsg:
		switch msg.Event.Type {
		case agent.EventRunStart:
			m.state = tea.ProgramStateWorking
		case agent.EventSubagentStart:
			if d, card := m.agentCard(msg.Run + "/" + msg.Event.Call.Name); card != nil {
				return m.openAside(d, card), true
			}
			return nil, true
		case agent.EventSubagentEnd:
			id := msg.Run + "/" + msg.Event.Call.Name
			return tea.Tick(agentLinger, func(time.Time) tea.Msg { return agentDoneMsg{run: id} }), true
		}
		return nil, true
	case harness.SavedMsg:
		if msg.Err != nil {
			return m.wm.Toast("save failed: "+msg.Err.Error(), wm.Error, 0), true
		}
		return nil, true
	case harness.MissionMsg:
		d := m.deskOfSession(msg.Session)
		if d == nil {
			return nil, true
		}
		if msg.Mission.ID == d.mission && (msg.Mission.Ended() || msg.Op == mission.OpRemove) {
			d.mission = ""
		}
		if msg.Op == mission.OpRemove {
			card, ok := d.missions[msg.Mission.ID]
			delete(d.missions, msg.Mission.ID)
			if ok {
				return m.wm.Close(card), true
			}
		}
		return nil, true
	case harness.MCPMsg:
		if msg.Err != nil {
			return m.wm.Toast("mcp "+msg.Server+": "+render.Truncate(msg.Err.Error(), 60, m.theme), wm.Error, 0), true
		}
		if len(msg.Tools) > 0 {
			return m.wm.Toast(fmt.Sprintf("mcp %s: %d tools", msg.Server, len(msg.Tools)), wm.Success, 0), true
		}
		return nil, true
	case harness.HookOutputMsg:
		return m.wm.Toast(render.Truncate(msg.Text, 80, m.theme), wm.Info, 0), true
	case harness.FactMsg, harness.ProgressMsg, harness.JobMsg, harness.AttemptMsg, harness.FailoverMsg, harness.ServedMsg, harness.MCPLogMsg:
		return nil, true
	}
	return nil, false
}

// Routes requests the cards made and reports whether the message was one
func (m *Model) cardMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case cards.SubmitMsg:
		return m.submit(msg), true
	case cards.CommandMsg:
		return m.command(m.deskOfSession(msg.Session), msg.Name, msg.Args), true
	case cards.SessionClosedMsg:
		return m.sessionClosed(msg.Session), true
	case cards.AnswerMsg:
		if !m.app.Approver.Answer(msg.ID, msg.Verdict) {
			return m.wm.Toast("approval gone", wm.Warn, 0), true
		}
		return nil, true
	case cards.OpenRunMsg:
		return m.openRun(msg.Run), true
	case cards.KillRunMsg:
		if err := m.app.Control.Cancel(msg.Run); err != nil {
			return m.wm.Toast(err.Error(), wm.Error, 0), true
		}
		return m.wm.Toast("stopping "+msg.Run, wm.Warn, 0), true
	case cards.KillJobMsg:
		return m.killJob(msg.Job), true
	case cards.OpenSessionMsg:
		return m.openStored(msg.Sessions...), true
	case cards.ForkMsg:
		return m.forkStored(msg.Sessions...), true
	case cards.DeleteMsg:
		return m.deleteStored(msg.Sessions...), true
	case cards.SetModelMsg:
		return m.setModel(msg.Model), true
	case cards.SetModeMsg:
		return m.setSetting("mode", string(msg.Mode), false), true
	case cards.SetProfileMsg:
		return m.switchProfile(msg.Profile, msg.Model), true
	case cards.ReconnectMsg:
		return m.reconnect(msg.Server), true
	case cards.ExportMsg:
		if msg.Err != nil {
			return m.wm.Toast(msg.Err.Error(), wm.Error, 0), true
		}
		return m.wm.Toast("wrote "+msg.Path, wm.Success, 0), true
	case cards.SettingMsg:
		return m.setSetting(msg.Key, msg.Value, msg.Quiet), true
	case cards.MissionStateMsg:
		d := m.deskOfSession(msg.Session)
		if d == nil {
			return nil, true
		}
		var err error
		if msg.Status == mission.StatusActive {
			_, err = d.live.Missions.Resume(msg.ID)
		} else {
			_, err = d.live.Missions.Finish(msg.ID, msg.Status, "")
		}
		if err != nil {
			return m.wm.Toast(err.Error(), wm.Error, 0), true
		}
		if msg.Status == mission.StatusActive {
			d.mission = msg.ID
		}
		return nil, true
	case cards.MissionRemoveMsg:
		d := m.deskOfSession(msg.Session)
		if d == nil || !d.live.Missions.Remove(msg.ID) {
			return m.wm.Toast("no mission "+msg.ID, wm.Warn, 0), true
		}
		return m.wm.Toast("forgot mission "+msg.ID, wm.Success, 0), true
	}
	return nil, false
}

// Applies a setting, then saves it for the next start, an empty value unsetting it and quiet skipping the toast
func (m *Model) setSetting(key, value string, quiet bool) tea.Cmd {
	th, known := theme.Named(value)
	if key == "theme" && value != "" && !known {
		return m.wm.Toast("themes: "+strings.Join(theme.Names(), ", "), wm.Warn, 0)
	}
	cfg := m.app.Config
	if err := cfg.Set(key, value); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.app.Config = cfg
	switch key {
	case "theme":
		if value == "" {
			resolved, err := resolveTheme(cfg, "")
			if err != nil {
				return m.wm.Toast(err.Error(), wm.Error, 0)
			}
			th = resolved
		}
		m.setTheme(th)
	case "motion":
		motion := 1.0
		switch {
		case cfg.UI.Motion != nil:
			motion = *cfg.UI.Motion
		case m.theme.Plain:
			motion = 0
		}
		m.wm.SetMotion(motion)
	case "cascade":
		m.wm.SetCascade(cfg.UI.Cascade != nil && *cfg.UI.Cascade)
	case "compact_threshold":
		m.app.Meter.SetThreshold(cfg.Compaction.Threshold)
	case "mode":
		md := mode.Default
		if value != "" {
			md, _ = mode.Parse(value)
		}
		m.app.Mode.Set(md)
	}
	m.wm.Repaint()
	if err := cfg.SaveSetting(m.opts.Dir, key, value); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	if quiet {
		return nil
	}
	if value == "" {
		return m.wm.Toast(key+" unset", wm.Success, 0)
	}
	return m.wm.Toast(key+" = "+value, wm.Success, 0)
}

// Switches the model of the session on the stage, or the default, saving the default when it moved
func (m *Model) setModel(id string) tea.Cmd {
	before := m.app.Config.Model
	if d := m.current(); d != nil {
		m.app.SetModelOn(d.live, id)
	} else {
		m.app.SetDefaultModel(id)
	}
	m.retitle()
	m.wm.Repaint()
	if m.app.Config.Model != before {
		if err := harness.Save(m.opts.Dir, m.app.Config.Sources, harness.Field{Path: []string{"model"}, Value: id}); err != nil {
			return m.wm.Toast(err.Error(), wm.Error, 0)
		}
	}
	return m.wm.Toast("model "+id, wm.Success, 0)
}

func (m *Model) retitle() {
	m.wm.SetWindowTitle("icebath " + m.theme.Glyphs.Sep + " " + m.endpoint().Model)
}

// Endpoint a profile switch opened for a session, or the error that stopped it
type profileMsg struct {
	live     *harness.Live
	name     string
	endpoint harness.Endpoint
	err      error
}

// Opens the profile off the event loop and moves the session on the stage, or the default, to it
func (m *Model) switchProfile(name, model string) tea.Cmd {
	if strings.TrimSpace(name) == "" {
		return m.wm.Toast("no profile named", wm.Warn, 0)
	}
	var live *harness.Live
	if d := m.current(); d != nil {
		if m.app.Busy(d.live) {
			return m.wm.Toast("agent still working", wm.Warn, 0)
		}
		live = d.live
	}
	ctx := m.ctx
	return func() tea.Msg {
		ep, err := m.app.OpenEndpoint(ctx, name, model)
		return profileMsg{live: live, name: name, endpoint: ep, err: err}
	}
}

func (m *Model) adoptProfile(msg profileMsg) tea.Cmd {
	if msg.err != nil {
		return m.wm.Toast(msg.err.Error(), wm.Error, 0)
	}
	if msg.live == nil {
		m.app.SetDefaultEndpoint(msg.name, msg.endpoint)
	} else if err := m.app.SetEndpointOn(msg.live, msg.name, msg.endpoint); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.retitle()
	refresh := []tea.Cmd{m.config.FetchModels(), m.listModels(true)}
	m.wm.Repaint()
	if err := harness.Save(m.opts.Dir, m.app.Config.Sources, harness.Field{Path: []string{"profile"}, Value: msg.name}, harness.Field{Path: []string{"model"}, Value: msg.endpoint.Model}); err != nil {
		return tea.Batch(append(refresh, m.wm.Toast(err.Error(), wm.Error, 0))...)
	}
	return tea.Batch(append(refresh, m.wm.Toast("profile "+msg.name+" "+m.theme.Glyphs.Sep+" "+msg.endpoint.Model, wm.Success, 0))...)
}

// Background job that was stopped
type jobKilledMsg struct{ job string }

// Stops the job off the event loop, since the kill waits for the process to end
func (m *Model) killJob(id string) tea.Cmd {
	ctx := m.ctx
	return tea.Batch(m.wm.Toast("stopping "+id, wm.Warn, 0), func() tea.Msg {
		if err := m.app.Shell.KillJob(ctx, id); err != nil {
			return errMsg{err}
		}
		return jobKilledMsg{job: id}
	})
}

// Reconnects the server off the event loop, since closing it waits out a grace period, its state arriving as messages
func (m *Model) reconnect(server string) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		if err := m.app.MCP.Reconnect(ctx, server); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

// Repaints the manager and every card with the palette
func (m *Model) setTheme(th theme.Theme) {
	m.theme = th
	m.env.Theme = th
	m.wm.SetTheme(th)
	m.config.SetTheme(th)
	for _, d := range m.desks {
		d.session.SetTheme(th)
	}
}

// Runs compaction through a one turn agent that never calls the model for anything else
func (m *Model) compactNow(d *desk) tea.Cmd {
	if d == nil {
		return nil
	}
	if m.app.Busy(d.live) {
		return m.wm.Toast("agent still working", wm.Warn, 0)
	}
	if d.compacting {
		return m.wm.Toast("already compacting", wm.Warn, 0)
	}
	cfg := m.app.Config
	if cfg.Compaction.Threshold <= 0 {
		cfg.Compaction.Threshold = 1
	}
	saved := m.app.Config
	m.app.Config = cfg
	ag := m.app.NewAgent("", d.live.Session)
	m.app.Config = saved
	ag.MaxRounds = 1
	d.compacting = true
	sess := d.live.Session
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		req := protocol.Request{Messages: sess.Snapshot()}
		for _, h := range ag.Hooks {
			if err := h.BeforeTurn(m.ctx, &req); err != nil {
				m.relay.Send(compactedMsg{desk: d, err: err})
				return
			}
		}
		m.app.Saver.Flush(sess)
		m.relay.Send(compactedMsg{desk: d})
	}()
	return m.wm.Toast("compacting", wm.Info, 0)
}

// Compaction that ended, with the error that stopped it
type compactedMsg struct {
	desk *desk
	err  error
}

func (m *Model) exportTranscript(d *desk, path string) tea.Cmd {
	if d == nil {
		return nil
	}
	if path == "" {
		path = m.exportPath(d)
	}
	if err := os.WriteFile(path, []byte(d.live.Session.MarkdownWith(session.MarkdownOptions{Timestamps: true, Records: true})), 0o644); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	return m.wm.Toast("wrote "+path, wm.Success, 0)
}

func (m *Model) exportPath(d *desk) string {
	return filepath.Join(m.app.Root, "transcript-"+d.live.Session.ID()+".md")
}

func (m *Model) View() tea.View {
	title := "icebath"
	if model := m.endpoint().Model; model != "" {
		title += " " + m.theme.Glyphs.Sep + " " + model
	}
	if m.state == tea.ProgramStateBlocked {
		title += " " + m.theme.Glyphs.Sep + " " + render.Count(m.app.Approver.Len(), "approval")
	}
	status := &tea.ProgramStatus{State: m.state, App: "icebath", Title: title}
	if m.state == tea.ProgramStateBlocked {
		status.Kind = tea.ProgramStatusKindPermission
	}
	m.wm.SetStatus(status)
	d := m.current()
	switch {
	case d != nil && d.mission != "":
		if ms, ok := d.live.Missions.Get(d.mission); ok && !ms.Ended() {
			m.wm.SetProgress(tea.NewProgressBar(tea.ProgressBarDefault, int(ms.Progress()*100)))
		} else {
			m.wm.SetProgress(nil)
		}
	case m.app.Control.Busy():
		m.wm.SetProgress(tea.NewProgressBar(tea.ProgressBarIndeterminate, 0))
	default:
		m.wm.SetProgress(nil)
	}
	return m.wm.View()
}

// Prompt handed on the command line, run once the program is up
type startMsg struct{ text string }

// Mission objective handed on the command line, run once the program is up
type startMissionMsg struct{ objective string }

// Message that starts a prompt handed on the command line once the program is up
func StartPrompt(text string) tea.Msg { return startMsg{text: text} }

// Message that starts a mission handed on the command line once the program is up
func StartMission(objective string) tea.Msg { return startMissionMsg{objective: objective} }
