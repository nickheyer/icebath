// Terminal application that composes the harness, the window manager and the cards, with a headless runner for scripts
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Layout file written beside the sessions
const layoutFile = "layout.json"

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

// Workspace bound to one open session with the cards built for it
type desk struct {
	live       *harness.Live
	ws         *wm.Workspace
	session    *cards.Session
	approval   *cards.Approval
	agents     map[string]*cards.Agent
	missions   map[string]*cards.Mission
	findings   *cards.Findings
	mission    string
	compacting bool
}

// Bubble Tea model running the harness behind the window manager
type Model struct {
	opts     Options
	app      *harness.App
	relay    *relay
	wm       *wm.Model
	env      *cards.Env
	keys     *keys.Map
	keysErr  error
	theme    theme.Theme
	desks    []*desk
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
	quitting bool
	width    int
	height   int
	bg       sync.WaitGroup
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
	layout, saved := loadLayout(layoutPath(app.Config))
	th, err := resolveTheme(opts.Config, layout.Theme)
	if err != nil {
		cancel()
		return nil, err
	}
	m := &Model{opts: opts, app: app, relay: r, theme: th, ctx: ctx, cancel: cancel, started: time.Now(), state: tea.ProgramStateIdle}
	m.keys, m.keysErr = keys.Open(filepath.Join(m.configDir(), keys.FileName))
	m.env = m.environment()
	m.buildManager(layout, saved)
	m.buildCards()
	d, _ := m.addDesk(app.Current())
	if saved {
		m.reopen(layout.Open)
		m.wm.Focus(d.session)
	}
	m.wm.SetCommands(m.commands())
	m.wm.SetBar(m.bar)
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
		Primary: func() harness.Endpoint { return a.Primary },
		Models: func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error) {
			prov, err := p.Open(ctx)
			if err != nil {
				return nil, err
			}
			return p.ListModels(ctx, prov)
		},
		Knobs:      m.knobs,
		ProjectDir: filepath.Join(a.Root, harness.ProjectDir),
		Now:        time.Now,
	}
}

func (m *Model) configDir() string {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return m.app.Root
	}
	return dir
}

// Knob values as they stand, empty for those left unset
func (m *Model) knobs() map[string]string {
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

// Cards a saved layout can reopen, by key
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
	if d := m.current(); d != nil && key == "findings:"+d.live.Session.ID() {
		return m.findingsOf(d)
	}
	return nil
}

func (m *Model) reopen(keys []string) {
	for _, k := range keys {
		if c := m.panel(k); c != nil {
			m.wm.OpenHere(c)
		}
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

// Layout read from the path, reporting whether a readable one was there
func loadLayout(path string) (wm.Layout, bool) {
	var l wm.Layout
	if path == "" {
		return l, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return l, false
	}
	return l, unmarshal(data, &l) == nil
}

func (m *Model) saveLayout() {
	path := layoutPath(m.app.Config)
	if path == "" {
		return
	}
	l := m.wm.Layout()
	l.Theme = m.theme.Name
	data, err := marshal(l)
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, data, 0o644)
}

// Starts the manager, lists models for the prompt, watches the keybind file and fires the session start hooks
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.wm.Init(), m.listModels(true), m.watchKeys(), func() tea.Msg {
		if err := m.app.SessionStart(m.ctx, "startup"); err != nil {
			return errMsg{err}
		}
		return nil
	}}
	if m.keysErr != nil {
		cmds = append(cmds, m.wm.Toast("keybinds: "+m.keysErr.Error(), wm.Error, 8*time.Second))
	}
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
	return tea.Batch(changed, m.wm.Toast("keybinds reloaded from "+m.keys.Path(), wm.Success, 0))
}

// Attaches the running program so harness messages reach it
func (m *Model) Attach(p *tea.Program) { m.relay.Attach(p) }

type errMsg struct{ err error }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		return m, m.submit(cards.SubmitMsg{Session: m.desks[0].live.Session.ID(), Parts: []protocol.Part{protocol.Text{Text: msg.text}}, Text: msg.text})
	case startMissionMsg:
		return m, m.startMission(m.desks[0], msg.objective)
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

// Keeps the selected harness session and the busy dots in step with the workspace on the stage
func (m *Model) sync() tea.Cmd {
	ws := m.wm.Active()
	if ws == nil {
		return nil
	}
	for _, d := range m.desks {
		d.ws.SetBusy(m.app.Busy(d.live))
		if d.ws == ws && m.app.Session != d.live.Session {
			m.app.Select(d.live)
		}
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
		return m.wm.Toast("run cancelled, "+again+" again quits", wm.Warn, 0)
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
		cmds := []tea.Cmd{m.wm.Open(d.ws, d.approval)}
		if d.ws != m.wm.Active() {
			m.wm.SetUrgent(d.ws, true)
			cmds = append(cmds, m.wm.Toast(fmt.Sprintf("approval waiting in workspace %d", m.wm.IndexOf(d.ws)+1), wm.Warn, 0))
		}
		return tea.Batch(cmds...), true
	case harness.ApprovalDoneMsg:
		if m.app.Control.Busy() {
			m.state = tea.ProgramStateWorking
		}
		return nil, true
	case harness.RunDoneMsg:
		m.state = tea.ProgramStateDone
		d := m.deskOfRun(msg.Run)
		if msg.Err != nil {
			m.state = tea.ProgramStateError
			if !errors.Is(msg.Err, agent.ErrInterrupted) && !errors.Is(msg.Err, context.Canceled) {
				cmds := []tea.Cmd{m.wm.Toast(render.Truncate(msg.Err.Error(), 80), wm.Error, 6*time.Second)}
				if d != nil {
					cmds = append(cmds, m.wm.Attention(d.session, wm.Alarm))
				}
				return tea.Batch(cmds...), true
			}
			return nil, true
		}
		if d != nil && d.ws != m.wm.Active() {
			m.wm.SetUrgent(d.ws, true)
		}
		return nil, true
	case harness.EventMsg:
		switch msg.Event.Type {
		case agent.EventRunStart:
			m.state = tea.ProgramStateWorking
		case agent.EventSubagentStart:
			return m.openAgent(msg.Run + "/" + msg.Event.Call.Name), true
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
			return m.wm.Toast("mcp "+msg.Server+": "+render.Truncate(msg.Err.Error(), 60), wm.Error, 0), true
		}
		if len(msg.Tools) > 0 {
			return m.wm.Toast(fmt.Sprintf("mcp %s: %d tools", msg.Server, len(msg.Tools)), wm.Success, 0), true
		}
		return nil, true
	case harness.HookOutputMsg:
		return m.wm.Toast(render.Truncate(msg.Text, 80), wm.Info, 0), true
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
	case cards.AnswerMsg:
		if !m.app.Approver.Answer(msg.ID, msg.Verdict) {
			return m.wm.Toast("that approval is gone", wm.Warn, 0), true
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
		return m.setKnob("mode", string(msg.Mode), false), true
	case cards.SetProfileMsg:
		return m.switchProfile(msg.Profile, msg.Model), true
	case cards.ReconnectMsg:
		return m.reconnect(msg.Server), true
	case cards.ExportMsg:
		if msg.Err != nil {
			return m.wm.Toast(msg.Err.Error(), wm.Error, 0), true
		}
		return m.wm.Toast("wrote "+msg.Path, wm.Success, 0), true
	case cards.KnobMsg:
		return m.setKnob(msg.Key, msg.Value, msg.Quiet), true
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

// Applies a knob and saves it for the next start, an empty value unsetting it and quiet skipping the toast
func (m *Model) setKnob(key, value string, quiet bool) tea.Cmd {
	th, known := theme.Named(value)
	if key == "theme" && value != "" && !known {
		return m.wm.Toast("themes are "+strings.Join(theme.Names(), ", "), wm.Warn, 0)
	}
	cfg := m.app.Config
	if err := cfg.SaveKnob(m.opts.Dir, key, value); err != nil {
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
	if quiet {
		return nil
	}
	if value == "" {
		return m.wm.Toast(key+" unset", wm.Success, 0)
	}
	return m.wm.Toast(key+" = "+value, wm.Success, 0)
}

// Switches the model and saves it to the configuration the next start reads
func (m *Model) setModel(id string) tea.Cmd {
	if err := harness.Save(m.opts.Dir, m.app.Config.Sources, harness.Field{Path: []string{"model"}, Value: id}); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.app.SetModel(id)
	m.retitle()
	m.wm.Repaint()
	return m.wm.Toast("model "+id, wm.Success, 0)
}

func (m *Model) retitle() {
	m.wm.SetWindowTitle("icebath · " + m.app.Primary.Model)
}

// Endpoint a profile switch opened, or the error that stopped it
type profileMsg struct {
	name     string
	endpoint harness.Endpoint
	err      error
}

// Opens the profile off the event loop, since a key source may run a command, then adopts the endpoint
func (m *Model) switchProfile(name, model string) tea.Cmd {
	if strings.TrimSpace(name) == "" {
		return m.wm.Toast("give the profile a name", wm.Warn, 0)
	}
	if m.app.Control.Busy() {
		return m.wm.Toast("wait for the runs to finish", wm.Warn, 0)
	}
	ctx := m.ctx
	return func() tea.Msg {
		ep, err := m.app.OpenEndpoint(ctx, name, model)
		return profileMsg{name: name, endpoint: ep, err: err}
	}
}

func (m *Model) adoptProfile(msg profileMsg) tea.Cmd {
	if msg.err != nil {
		return m.wm.Toast(msg.err.Error(), wm.Error, 0)
	}
	if err := m.app.SetEndpoint(msg.name, msg.endpoint); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.retitle()
	refresh := []tea.Cmd{m.config.FetchModels(), m.listModels(true)}
	m.wm.Repaint()
	if err := harness.Save(m.opts.Dir, m.app.Config.Sources, harness.Field{Path: []string{"profile"}, Value: msg.name}, harness.Field{Path: []string{"model"}, Value: msg.endpoint.Model}); err != nil {
		return tea.Batch(append(refresh, m.wm.Toast(err.Error(), wm.Error, 0))...)
	}
	return tea.Batch(append(refresh, m.wm.Toast("profile "+msg.name+" · "+m.app.Primary.Model, wm.Success, 0))...)
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
		return m.wm.Toast("wait for the run to finish", wm.Warn, 0)
	}
	if d.compacting {
		return m.wm.Toast("compaction is already going", wm.Warn, 0)
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

// Segments the bar shows beside the workspace chips
func (m *Model) bar() (left, right []wm.Segment) {
	d := m.current()
	left = append(left, wm.Segment{Text: m.app.Primary.Model, Style: wm.Cool})
	if md := m.app.Mode.Mode(); md != mode.Default {
		left = append(left, wm.Segment{Text: md.Label(), Style: wm.Warm})
	}
	snap := m.app.Meter.Snapshot()
	if d != nil && snap.ContextLimit > 0 {
		share := float64(d.live.Session.Context()) / float64(snap.ContextLimit)
		st := wm.Dim
		switch {
		case share >= 0.9:
			st = wm.Bad
		case share >= 0.7:
			st = wm.Warm
		}
		left = append(left, wm.Segment{Text: "ctx " + render.Percent(share), Style: st})
	}
	if snap.Cost > 0 {
		left = append(left, wm.Segment{Text: render.Money(snap.Cost), Style: wm.Dim})
	}
	if d != nil {
		if n := len(d.approval.Pending()); n > 0 {
			left = append(left, wm.Segment{Text: fmt.Sprintf("⚑ %d awaiting approval", n), Style: wm.Hot})
		} else if m.app.Busy(d.live) {
			tools := 0
			var elapsed time.Duration
			for _, r := range m.app.Control.Active() {
				if r.Session == d.live.Session && r.Parent == "" {
					_, tools = r.LastTool()
					elapsed = r.Elapsed(time.Now())
				}
			}
			text := render.Spinner(int(time.Since(m.started)/(120*time.Millisecond)), m.theme) + " working"
			if tools > 0 {
				text += fmt.Sprintf(" · %d tools", tools)
			}
			left = append(left, wm.Segment{Text: text, Style: wm.Good})
			right = append(right, wm.Segment{Text: render.Elapsed(elapsed), Style: wm.Dim})
		}
	}
	return left, right
}

func (m *Model) View() tea.View {
	title := "icebath"
	if m.app.Primary.Model != "" {
		title += " · " + m.app.Primary.Model
	}
	if m.state == tea.ProgramStateBlocked {
		title += fmt.Sprintf(" · %d awaiting approval", m.app.Approver.Len())
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
