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
	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Layout file written beside the sessions
const layoutFile = "layout.json"

// Options the shell passes beside the configuration
type Options struct {
	Config  harness.Config
	Resume  string
	Dir     string
	Version string
}

// Bubble Tea model running the harness behind the deck
type Model struct {
	opts       Options
	app        *harness.App
	relay      *relay
	deck       *deck.Model
	history    *deck.Model
	env        *cards.Env
	theme      theme.Theme
	composer   *cards.Composer
	transcript *cards.Transcript
	approval   *cards.Approval
	daemons    *cards.Daemons
	ice        *cards.Telemetry
	findings   *cards.Findings
	workspace  *cards.Workspace
	jobs       *cards.Jobs
	sessions   *cards.Sessions
	config     *cards.Config
	missions   map[string]*cards.Mission
	children   map[string]*cards.Transcript
	ctx        context.Context
	cancel     context.CancelFunc
	started    time.Time
	state      tea.ProgramState
	inHistory  bool
	mission    string
	err        error
	quitting   bool
	width      int
	height     int
	bg         sync.WaitGroup
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
	m := &Model{opts: opts, app: app, relay: r, theme: th, ctx: ctx, cancel: cancel, started: time.Now(), state: tea.ProgramStateIdle, missions: map[string]*cards.Mission{}, children: map[string]*cards.Transcript{}}
	m.env = m.environment()
	m.buildCards()
	m.buildDeck(layout, saved)
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
		Theme: m.theme, Control: a.Control, Meter: a.Meter, Work: a.Workspace, Facts: a.Facts, Missions: a.Missions, Approver: a.Approver, PreImage: a.PreImage, MCP: a.MCP, Shell: a.Shell, Tools: a.Tools, Rules: a.Rules, Mode: a.Mode, Store: a.Store, Files: a.Files,
		Session:  func() *session.Session { return a.Session },
		Profiles: func() []profiles.Profile { return profiles.Merge(profiles.Defaults(), a.Profiles) },
		Primary:  func() harness.Endpoint { return a.Primary },
		Models: func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error) {
			prov, err := p.Open(ctx)
			if err != nil {
				return nil, err
			}
			return p.ListModels(ctx, prov)
		},
		Hooks:      func() hooks.Config { return a.Config.Hooks },
		Knobs:      m.knobs,
		ConfigDir:  m.configDir(),
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

func (m *Model) knobs() map[string]string {
	cfg := m.app.Config
	out := map[string]string{"max_tokens": fmt.Sprint(cfg.MaxTokens), "max_rounds": fmt.Sprint(cfg.MaxRounds), "compact_threshold": fmt.Sprint(cfg.Compaction.Threshold), "theme": m.theme.Name, "motion": fmt.Sprintf("%.2f", m.deckMotion())}
	if cfg.MaxTokens == 0 {
		out["max_tokens"] = fmt.Sprint(harness.DefaultMaxTokens)
	}
	if cfg.MaxRounds == 0 {
		out["max_rounds"] = fmt.Sprint(agent.DefaultMaxRounds)
	}
	if cfg.Temperature != nil {
		out["temperature"] = fmt.Sprintf("%.2f", *cfg.Temperature)
	} else {
		out["temperature"] = "model default"
	}
	return out
}

func (m *Model) deckMotion() float64 {
	if m.deck == nil {
		if m.opts.Config.UI.Motion != nil {
			return *m.opts.Config.UI.Motion
		}
		return 1
	}
	return m.deck.Motion()
}

func (m *Model) buildCards() {
	env := m.env
	m.composer = cards.NewComposer(env, m.configDir())
	m.composer.Busy = m.app.Control.Busy
	m.composer.Model = func() string { return m.app.Primary.Model }
	m.composer.ModeName = func() string { return m.app.Mode.Mode().Label() }
	m.transcript = cards.NewTranscript(env)
	m.transcript.Load(m.app.Session.Entries(), m.app.Session.Records())
	m.approval = cards.NewApproval(env)
	m.daemons = cards.NewDaemons(env)
	m.ice = cards.NewTelemetry(env)
	m.findings = cards.NewFindings(env)
	m.workspace = cards.NewWorkspace(env)
	m.jobs = cards.NewJobs(env)
	m.sessions = cards.NewSessions(env)
	m.config = cards.NewConfig(env)
	for _, ms := range m.app.Missions.List() {
		m.missions[ms.ID] = cards.NewMission(env, ms)
	}
}

func (m *Model) buildDeck(layout deck.Layout, saved bool) {
	cfg := m.opts.Config
	opts := []deck.Option{deck.WithTheme(m.theme), deck.WithWindowTitle("icebath")}
	if cfg.UI.Motion != nil {
		opts = append(opts, deck.WithMotion(*cfg.UI.Motion))
	} else if m.theme.Plain {
		opts = append(opts, deck.WithMotion(0))
	}
	if cfg.UI.Hints != nil {
		opts = append(opts, deck.WithHints(*cfg.UI.Hints))
	}
	if cfg.UI.Ratio > 0 {
		opts = append(opts, deck.WithRatio(cfg.UI.Ratio))
	}
	list := []deck.Card{m.transcript, m.composer, m.ice, m.daemons, m.workspace, m.findings}
	for _, ms := range m.missions {
		list = append(list, ms)
	}
	opts = append(opts, deck.WithCards(list...))
	m.deck = deck.New(opts...)
	if saved {
		m.deck.Restore(layout)
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

func (m *Model) layoutPath() string { return layoutPath(m.app.Config) }

// Layout read from the path, reporting whether a readable one was there
func loadLayout(path string) (deck.Layout, bool) {
	var l deck.Layout
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
	path := m.layoutPath()
	if path == "" {
		return
	}
	data, err := marshal(m.deck.Layout())
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, data, 0o644)
}

// Starts the deck, attaches the relay and fires the session start hooks
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.deck.Init(), func() tea.Msg {
		if err := m.app.SessionStart(m.ctx, "startup"); err != nil {
			return errMsg{err}
		}
		return nil
	})
}

// Attaches the running program so harness messages reach it
func (m *Model) Attach(p *tea.Program) { m.relay.Attach(p) }

type errMsg struct{ err error }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.history != nil {
			m.history.SetSize(msg.Width, msg.Height)
		}
	case tea.KeyPressMsg:
		if cmd, handled := m.globalKey(msg); handled {
			return m, cmd
		}
	case errMsg:
		if msg.err != nil {
			return m, m.active().Toast(msg.err.Error(), deck.Error, 0)
		}
		return m, nil
	case compactedMsg:
		m.reloadTranscript()
		return m, m.deck.Toast("compacted", deck.Success, 0)
	}
	if cmd, handled := m.harnessMsg(msg); handled {
		_, deckCmd := m.active().Update(msg)
		return m, tea.Batch(cmd, deckCmd)
	}
	if cmd, handled := m.cardMsg(msg); handled {
		return m, cmd
	}
	_, cmd := m.active().Update(msg)
	return m, cmd
}

// Program filter that runs the shutdown before any quit message, whichever card or signal raised it
func (m *Model) Filter(_ tea.Model, msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.QuitMsg); ok {
		m.shutdown()
	}
	return msg
}

// Deck receiving input, the history deck while it is open
func (m *Model) active() *deck.Model {
	if m.inHistory && m.history != nil {
		return m.history
	}
	return m.deck
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

// Keys the app answers before the deck
func (m *Model) globalKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	k := msg.String()
	if k == "ctrl+c" {
		if m.app.Control.Busy() && !m.quitting {
			for _, r := range m.app.Control.Active() {
				if r.Parent == "" {
					m.app.Control.Cancel(r.ID)
				}
			}
			return m.active().Toast("run cancelled, ctrl+c again quits", deck.Warn, 0), true
		}
		return m.shutdown(), true
	}
	if m.active().Capturing() {
		return nil, false
	}
	switch k {
	case "ctrl+k", ":":
		return m.deck.ShowOverlay(m.palette()), true
	case "f1", "ctrl+/":
		return m.deck.ShowOverlay(m.help()), true
	case "esc":
		if m.inHistory {
			return m.leaveHistory(), true
		}
	}
	return nil, false
}

// Routes bridge messages to app state and reports whether the message was one
func (m *Model) harnessMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case harness.ApprovalMsg:
		m.state = tea.ProgramStateBlocked
		var cmds []tea.Cmd
		if m.deck.Overlay() != m.approval {
			cmds = append(cmds, m.deck.ShowOverlay(m.approval))
		}
		cmds = append(cmds, m.deck.Attention(m.deck.IndexOf(m.transcript), deck.Pulse))
		return tea.Batch(cmds...), true
	case harness.ApprovalDoneMsg:
		if m.app.Control.Busy() {
			m.state = tea.ProgramStateWorking
		}
		return nil, true
	case harness.RunDoneMsg:
		m.state = tea.ProgramStateDone
		if msg.Err != nil {
			m.state = tea.ProgramStateError
			if !errors.Is(msg.Err, agent.ErrInterrupted) && !errors.Is(msg.Err, context.Canceled) {
				return tea.Batch(m.deck.Toast(render.Truncate(msg.Err.Error(), 80), deck.Error, 6*time.Second), m.deck.Attention(m.deck.IndexOf(m.transcript), deck.Alarm)), true
			}
			return m.deck.Toast("run stopped", deck.Warn, 0), true
		}
		return m.deck.Toast("run finished in "+render.Elapsed(msg.Duration), deck.Success, 0), true
	case harness.EventMsg:
		switch msg.Event.Type {
		case agent.EventRunStart:
			m.state = tea.ProgramStateWorking
		case agent.EventSubagentStart:
			return m.deck.Toast("spawned "+msg.Event.Call.Name, deck.Info, 0), true
		}
		return nil, true
	case harness.SavedMsg:
		if msg.Err != nil {
			return m.deck.Toast("save failed: "+msg.Err.Error(), deck.Error, 0), true
		}
		return nil, true
	case harness.FactMsg:
		if msg.Change.Op == facts.OpAdd {
			return m.deck.Toast("recorded "+msg.Change.Fact.Kind+": "+render.Truncate(msg.Change.Fact.Title, 50), deck.Info, 0), true
		}
		return nil, true
	case harness.MissionMsg:
		if msg.Mission.ID == m.mission && (msg.Mission.Ended() || msg.Op == mission.OpRemove) {
			m.mission = ""
		}
		if msg.Op == mission.OpRemove {
			card, ok := m.missions[msg.Mission.ID]
			delete(m.missions, msg.Mission.ID)
			if ok {
				return m.deck.Remove(m.deck.IndexOf(card)), true
			}
			return nil, true
		}
		if _, ok := m.missions[msg.Mission.ID]; !ok {
			card := cards.NewMission(m.env, msg.Mission)
			m.missions[msg.Mission.ID] = card
			return m.deck.AddAfter(m.deck.IndexOf(m.transcript), card), true
		}
		return nil, true
	case harness.MCPMsg:
		if msg.Err != nil {
			return m.deck.Toast("mcp "+msg.Server+": "+render.Truncate(msg.Err.Error(), 60), deck.Error, 0), true
		}
		if len(msg.Tools) > 0 {
			return m.deck.Toast(fmt.Sprintf("mcp %s: %d tools", msg.Server, len(msg.Tools)), deck.Success, 0), true
		}
		return m.deck.Toast("mcp "+msg.Server+" "+string(msg.State), deck.Info, 0), true
	case harness.HookOutputMsg:
		return m.deck.Toast(render.Truncate(msg.Text, 80), deck.Info, 0), true
	case harness.ProgressMsg, harness.JobMsg, harness.AttemptMsg, harness.FailoverMsg, harness.ServedMsg, harness.MCPLogMsg:
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
		return m.command(msg.Name, msg.Args), true
	case cards.PaletteMsg:
		name, args, _ := strings.Cut(msg.Command, " ")
		return m.command(name, args), true
	case cards.AnswerMsg:
		if !m.app.Approver.Answer(msg.ID, msg.Verdict) {
			return m.deck.Toast("that approval is gone", deck.Warn, 0), true
		}
		return nil, true
	case cards.OpenRunMsg:
		return m.openRun(msg.Run), true
	case cards.KillRunMsg:
		if err := m.app.Control.Cancel(msg.Run); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		return m.deck.Toast("stopping "+msg.Run, deck.Warn, 0), true
	case cards.KillJobMsg:
		if err := m.app.Shell.KillJob(m.ctx, msg.Job); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		return m.deck.Toast("killed "+msg.Job, deck.Warn, 0), true
	case cards.ResumeMsg:
		return m.switchSession(msg.Session), true
	case cards.ForkMsg:
		return m.forkSession(msg.Session), true
	case cards.DeleteMsg:
		if msg.Session == m.app.Session.ID() {
			return m.deck.Toast("the live session cannot be deleted", deck.Warn, 0), true
		}
		if err := m.app.Store.Delete(m.ctx, msg.Session); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		m.sessions.Reload()
		return m.deck.Toast("deleted "+msg.Session, deck.Success, 0), true
	case cards.SetModelMsg:
		m.app.SetModel(msg.Model)
		m.retitle()
		return m.deck.Toast("model "+msg.Model, deck.Success, 0), true
	case cards.SetModeMsg:
		m.app.Mode.Set(msg.Mode)
		return m.deck.Toast("mode "+msg.Mode.Label(), deck.Success, 0), true
	case cards.SetProfileMsg:
		if err := m.app.SetProfile(m.ctx, msg.Profile, msg.Model); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		m.retitle()
		return tea.Batch(m.deck.Toast("profile "+msg.Profile+" · "+m.app.Primary.Model, deck.Success, 0), m.config.FetchModels()), true
	case cards.RulesMsg:
		path := filepath.Join(m.app.Root, harness.ProjectDir, harness.RulesFile)
		if err := msg.Set.Save(path); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		return m.deck.Toast("rules saved to "+path, deck.Success, 0), true
	case cards.ReconnectMsg:
		if err := m.app.MCP.Reconnect(m.ctx, msg.Server); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		return nil, true
	case cards.ForkAtMsg:
		return m.forkAt(msg.Entries), true
	case cards.ExportMsg:
		if msg.Err != nil {
			return m.deck.Toast(msg.Err.Error(), deck.Error, 0), true
		}
		return m.deck.Toast("wrote "+msg.Path, deck.Success, 0), true
	case cards.KnobMsg:
		cfg := m.app.Config
		if err := cfg.SetKnob(msg.Key, msg.Value); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		m.app.Config = cfg
		var cmds []tea.Cmd
		switch msg.Key {
		case "theme":
			cmds = append(cmds, m.setTheme(msg.Value))
		case "motion":
			m.deck.SetMotion(*cfg.UI.Motion)
		case "compact_threshold":
			m.app.Meter.SetThreshold(cfg.Compaction.Threshold)
		case "mode":
			md, _ := mode.Parse(msg.Value)
			m.app.Mode.Set(md)
		}
		return tea.Batch(append(cmds, m.deck.Toast(msg.Key+" = "+msg.Value, deck.Success, 0))...), true
	case cards.MissionMsg:
		return m.startMission(msg.Objective), true
	case cards.MissionStateMsg:
		var err error
		if msg.Status == mission.StatusActive {
			_, err = m.app.Missions.Resume(msg.ID)
		} else {
			_, err = m.app.Missions.Finish(msg.ID, msg.Status, "")
		}
		if err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0), true
		}
		if msg.Status == mission.StatusActive {
			m.mission = msg.ID
		}
		return nil, true
	case cards.MissionRemoveMsg:
		if !m.app.Missions.Remove(msg.ID) {
			return m.deck.Toast("no mission "+msg.ID, deck.Warn, 0), true
		}
		return m.deck.Toast("forgot mission "+msg.ID, deck.Success, 0), true
	}
	return nil, false
}

// Sends composer input, steering the root run when one is going and starting one otherwise
func (m *Model) submit(msg cards.SubmitMsg) tea.Cmd {
	if m.app.Control.Busy() {
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" {
				if err := m.app.Control.Steer(r.ID, msg.Parts...); err != nil {
					return m.deck.Toast(err.Error(), deck.Error, 0)
				}
				return m.deck.Toast("queued for the next turn", deck.Info, 0)
			}
		}
	}
	if m.app.Session.Title() == "" && msg.Text != "" {
		m.app.Session.SetTitle(render.Summary(msg.Text, 60))
	}
	if _, err := m.app.Run(m.ctx, m.mission, msg.Parts); err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	m.state = tea.ProgramStateWorking
	return m.deck.Focus(m.deck.IndexOf(m.transcript))
}

func (m *Model) retitle() {
	m.deck.SetWindowTitle("icebath · " + m.app.Primary.Model)
}

func (m *Model) setTheme(name string) tea.Cmd {
	th, ok := theme.Named(name)
	if !ok {
		return m.deck.Toast("themes are "+strings.Join(theme.Names(), ", "), deck.Warn, 0)
	}
	m.theme = th
	m.env.Theme = th
	m.deck.SetTheme(th)
	if m.history != nil {
		m.history.SetTheme(th)
	}
	m.config.Group.SetTheme(th)
	return m.deck.Toast("theme "+name, deck.Success, 0)
}

// Opens the transcript of a run, child runs getting their own card
func (m *Model) openRun(id string) tea.Cmd {
	r, ok := m.app.Control.Get(id)
	if !ok {
		return m.deck.Toast("no run "+id, deck.Warn, 0)
	}
	if r.Parent == "" {
		return m.deck.Focus(m.deck.IndexOf(m.transcript))
	}
	if card, ok := m.children[id]; ok {
		if i := m.deck.IndexOf(card); i >= 0 {
			return m.deck.Focus(i)
		}
	}
	card := cards.NewChildTranscript(m.env, r.Parent, r.Name)
	card.Load(r.Session.Entries(), r.Session.Records())
	m.children[id] = card
	return m.deck.AddAfter(m.deck.IndexOf(m.transcript), card)
}

func (m *Model) reloadTranscript() {
	m.transcript.Load(m.app.Session.Entries(), m.app.Session.Records())
	m.findings.Reload()
	m.sessions.Reload()
}

// Points the cards at the stores of the live session and rebuilds everything read from it
func (m *Model) reloadSession() tea.Cmd {
	m.env.Facts, m.env.Missions = m.app.Facts, m.app.Missions
	m.mission = ""
	m.reloadTranscript()
	m.ice.Reload()
	return m.reloadMissions()
}

// Replaces the mission cards with one per mission of the live session
func (m *Model) reloadMissions() tea.Cmd {
	var cmds []tea.Cmd
	for _, card := range m.missions {
		if i := m.deck.IndexOf(card); i >= 0 {
			cmds = append(cmds, m.deck.Remove(i))
		}
	}
	m.missions = map[string]*cards.Mission{}
	at := m.deck.IndexOf(m.transcript)
	for _, ms := range m.app.Missions.List() {
		card := cards.NewMission(m.env, ms)
		m.missions[ms.ID] = card
		cmds = append(cmds, m.deck.AddAfter(at, card))
		at++
	}
	return tea.Batch(cmds...)
}

// Switches the live session to a stored one
func (m *Model) switchSession(id string) tea.Cmd {
	if err := m.app.Switch(m.ctx, id); err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	return tea.Batch(m.reloadSession(), m.deck.Toast("resumed "+id, deck.Success, 0), m.deck.Focus(m.deck.IndexOf(m.transcript)))
}

// Forks a stored session into a new one and switches to it
func (m *Model) forkSession(id string) tea.Cmd {
	src, err := m.app.Store.Load(m.ctx, id)
	if err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	f := src.Fork()
	if err := m.app.Store.Save(m.ctx, f); err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	return m.switchSession(f.ID())
}

// Forks the live session at an entry count, leaving the history deck
func (m *Model) forkAt(n int) tea.Cmd {
	f, err := m.app.ForkAt(n)
	if err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	return tea.Batch(m.leaveHistory(), m.reloadSession(), m.deck.Toast("forked at "+fmt.Sprint(n)+" entries into "+f.ID(), deck.Success, 0))
}

// Replaces the live deck with moments of the session fanned along a timeline
func (m *Model) enterHistory() tea.Cmd {
	moments := cards.Moments(m.app.Session)
	if len(moments) == 0 {
		return m.deck.Toast("nothing to fly through yet", deck.Info, 0)
	}
	list := make([]deck.Card, 0, len(moments))
	for i, mo := range moments {
		list = append(list, cards.NewMomentCard(m.env, m.app.Session, mo, i))
	}
	opts := []deck.Option{deck.WithTheme(m.theme), deck.WithMode(deck.Timeline), deck.WithCards(list...), deck.WithMotion(m.deck.Motion()), deck.WithWindowTitle("icebath · history")}
	m.history = deck.New(opts...)
	m.inHistory = true
	cmds := []tea.Cmd{m.history.Init()}
	if m.width > 0 {
		m.history.SetSize(m.width, m.height)
	}
	cmds = append(cmds, m.history.Focus(len(list)-1), m.history.Toast("enter forks here, c marks two moments to compare, esc returns", deck.Info, 6*time.Second))
	return tea.Batch(cmds...)
}

func (m *Model) leaveHistory() tea.Cmd {
	if !m.inHistory {
		return nil
	}
	m.inHistory = false
	m.history = nil
	return m.deck.Init()
}

// Shows marked moments side by side in the history deck
func (m *Model) compareMoments() tea.Cmd {
	if m.history == nil {
		return nil
	}
	var marked []int
	for i, c := range m.history.Cards() {
		if mc, ok := c.(*cards.MomentCard); ok && mc.Marked() {
			marked = append(marked, i)
		}
	}
	if len(marked) < 2 {
		return m.history.Toast("mark two moments with c first", deck.Warn, 0)
	}
	first, second := marked[0], marked[1]
	cmds := []tea.Cmd{m.history.SetMode(deck.Split)}
	if first != 0 {
		cmds = append(cmds, m.history.Promote(first))
		if second == 0 {
			second = first
		}
	}
	cmds = append(cmds, m.history.Focus(second))
	return tea.Batch(cmds...)
}

func (m *Model) startMission(objective string) tea.Cmd {
	ms, err := m.app.Missions.Create(objective)
	if err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	m.mission = ms.ID
	if _, err := m.app.Run(m.ctx, ms.ID, []protocol.Part{protocol.Text{Text: objective}}); err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	m.state = tea.ProgramStateWorking
	return m.deck.Toast("mission "+ms.ID+" started", deck.Success, 0)
}

// Commands reachable from the composer and the palette
func (m *Model) actions() []cards.Action {
	out := []cards.Action{
		{Name: "help", Desc: "show the keymap", Keys: "F1"},
		{Name: "palette", Desc: "this list", Keys: "ctrl+k"},
		{Name: "mode", Desc: "cycle the permission mode"},
		{Name: "compact", Desc: "summarize the transcript now"},
		{Name: "clear", Desc: "start a fresh session"},
		{Name: "fork", Desc: "fork the session here"},
		{Name: "sessions", Desc: "open the session list"},
		{Name: "history", Desc: "fly through the session as moments"},
		{Name: "compare", Desc: "compare marked moments"},
		{Name: "findings", Desc: "open the findings"},
		{Name: "config", Desc: "open the configuration"},
		{Name: "jobs", Desc: "open the background jobs"},
		{Name: "workspace", Desc: "open the files touched"},
		{Name: "daemons", Desc: "open the roster"},
		{Name: "ice", Desc: "open the telemetry"},
		{Name: "cancel", Desc: "stop the run"},
		{Name: "interrupt", Desc: "stop after the current tool"},
		{Name: "export", Desc: "write the transcript as markdown"},
		{Name: "timeline", Desc: "toggle the timeline layout", Keys: "t"},
		{Name: "quit", Desc: "leave icebath", Keys: "ctrl+c"},
	}
	for _, n := range theme.Names() {
		out = append(out, cards.Action{Name: "theme " + n, Desc: "switch the palette"})
	}
	for _, md := range mode.All() {
		out = append(out, cards.Action{Name: "mode " + string(md), Desc: md.Label()})
	}
	return out
}

func (m *Model) palette() deck.Card { return cards.NewPalette(m.env, m.actions()) }

func (m *Model) help() deck.Card {
	return cards.NewHelp(m.env, []cards.HelpSection{
		{Title: "deck", Keys: [][2]string{{"tab / j k", "next card"}, {"h l", "main / stack"}, {"enter", "promote or solo"}, {"esc", "back"}, {"1-9", "jump"}, {"t", "timeline"}, {"x", "close"}, {"L H", "resize"}, {"J K", "move"}, {"?", "hints"}, {"drag", "reorder"}}},
		{Title: "app", Keys: [][2]string{{"ctrl+k :", "palette"}, {"F1", "help"}, {"ctrl+c", "cancel run, then quit"}, {"i", "jack into the uplink"}}},
		{Title: "transcript", Keys: [][2]string{{"j k", "scroll"}, {"[ ]", "turns"}, {". ,", "block cursor"}, {"o", "expand"}, {"e E", "expand all / none"}, {"T", "thinking"}, {"/ n N", "search"}, {"f", "follow"}, {"drag", "copy text"}}},
		{Title: "approval", Keys: [][2]string{{"y", "allow"}, {"n", "deny"}, {"a", "always"}, {"v", "never"}}},
		{Title: "slash", Keys: [][2]string{{"/model id", "switch model"}, {"/mode name", "permission mode"}, {"/profile name", "switch endpoint"}, {"/attach path", "attach image"}, {"/mission text", "start a mission"}, {"/history", "moments"}, {"/theme name", "palette"}}},
	})
}

// Carries out a slash or palette command
func (m *Model) command(name, args string) tea.Cmd {
	focus := func(c deck.Card) tea.Cmd {
		if i := m.deck.IndexOf(c); i >= 0 {
			return m.deck.Focus(i)
		}
		return m.deck.AddAfter(m.deck.Len()-1, c)
	}
	switch name {
	case "help":
		return m.deck.ShowOverlay(m.help())
	case "palette":
		return m.deck.ShowOverlay(m.palette())
	case "model":
		if args == "" {
			return tea.Batch(focus(m.config), m.config.FetchModels())
		}
		m.app.SetModel(args)
		m.retitle()
		return m.deck.Toast("model "+args, deck.Success, 0)
	case "mode":
		if args == "" {
			next := m.app.Mode.Cycle()
			return m.deck.Toast("mode "+next.Label(), deck.Success, 0)
		}
		md, err := mode.Parse(args)
		if err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0)
		}
		m.app.Mode.Set(md)
		return m.deck.Toast("mode "+md.Label(), deck.Success, 0)
	case "profile":
		if args == "" {
			return focus(m.config)
		}
		prof, model, _ := strings.Cut(args, " ")
		if err := m.app.SetProfile(m.ctx, prof, strings.TrimSpace(model)); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0)
		}
		m.retitle()
		return m.deck.Toast("profile "+prof+" · "+m.app.Primary.Model, deck.Success, 0)
	case "mission":
		if strings.TrimSpace(args) == "" {
			return m.deck.Toast("give the mission an objective", deck.Warn, 0)
		}
		return m.startMission(args)
	case "compact":
		return m.compactNow()
	case "clear":
		if err := m.app.Fresh(); err != nil {
			return m.deck.Toast(err.Error(), deck.Error, 0)
		}
		return tea.Batch(m.reloadSession(), m.deck.Toast("fresh session "+m.app.Session.ID(), deck.Success, 0))
	case "fork":
		return m.forkAt(m.app.Session.Len())
	case "sessions":
		return focus(m.sessions)
	case "history":
		return m.enterHistory()
	case "compare":
		return m.compareMoments()
	case "findings":
		return focus(m.findings)
	case "config":
		return focus(m.config)
	case "jobs":
		return focus(m.jobs)
	case "workspace":
		return focus(m.workspace)
	case "daemons":
		return focus(m.daemons)
	case "ice":
		return focus(m.ice)
	case "theme":
		return m.setTheme(args)
	case "cancel":
		n := 0
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" && m.app.Control.Cancel(r.ID) == nil {
				n++
			}
		}
		if n == 0 {
			return m.deck.Toast("nothing is running", deck.Info, 0)
		}
		return m.deck.Toast("cancelling", deck.Warn, 0)
	case "interrupt":
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" {
				m.app.Control.Interrupt(r.ID)
			}
		}
		return m.deck.Toast("stopping after the current tool", deck.Warn, 0)
	case "export":
		return m.exportTranscript(args)
	case "timeline":
		return m.deck.SetMode(deck.Timeline)
	case "quit":
		return m.shutdown()
	}
	return m.deck.Toast("unknown command /"+name, deck.Warn, 0)
}

// Runs compaction through a one turn agent that never calls the model for anything else
func (m *Model) compactNow() tea.Cmd {
	if m.app.Control.Busy() {
		return m.deck.Toast("wait for the run to finish", deck.Warn, 0)
	}
	cfg := m.app.Config
	if cfg.Compaction.Threshold <= 0 {
		cfg.Compaction.Threshold = 1
	}
	saved := m.app.Config
	m.app.Config = cfg
	ag := m.app.NewAgent("", m.app.Session)
	m.app.Config = saved
	ag.MaxRounds = 1
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		req := protocol.Request{Messages: m.app.Session.Snapshot()}
		for _, h := range ag.Hooks {
			if err := h.BeforeTurn(m.ctx, &req); err != nil {
				m.relay.Send(errMsg{err})
				return
			}
		}
		m.app.Saver.Flush(m.app.Session)
		m.relay.Send(compactedMsg{})
	}()
	return m.deck.Toast("compacting", deck.Info, 0)
}

type compactedMsg struct{}

func (m *Model) exportTranscript(path string) tea.Cmd {
	if path == "" {
		path = filepath.Join(m.app.Root, "transcript-"+m.app.Session.ID()+".md")
	}
	if err := os.WriteFile(path, []byte(m.app.Session.MarkdownWith(session.MarkdownOptions{Timestamps: true, Records: true})), 0o644); err != nil {
		return m.deck.Toast(err.Error(), deck.Error, 0)
	}
	return m.deck.Toast("wrote "+path, deck.Success, 0)
}

func (m *Model) View() tea.View {
	d := m.active()
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
	d.SetStatus(status)
	if m.mission != "" {
		if ms, ok := m.app.Missions.Get(m.mission); ok && !ms.Ended() {
			d.SetProgress(tea.NewProgressBar(tea.ProgressBarDefault, int(ms.Progress()*100)))
		} else {
			d.SetProgress(nil)
		}
	} else if m.app.Control.Busy() {
		d.SetProgress(tea.NewProgressBar(tea.ProgressBarIndeterminate, 0))
	} else {
		d.SetProgress(nil)
	}
	return d.View()
}

// Message that starts a prompt handed on the command line once the program is up
func StartPrompt(text string) tea.Msg {
	return cards.SubmitMsg{Parts: []protocol.Part{protocol.Text{Text: text}}, Text: text}
}

// Message that starts a mission handed on the command line once the program is up
func StartMission(objective string) tea.Msg {
	return cards.MissionMsg{Objective: objective}
}
