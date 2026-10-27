package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/tool/web"
)

// Error of a run or an open while nothing names a profile
var ErrNoProfile = errors.New("harness: no profile is open, pick one in config or pass -profile")

const (
	// Session label naming the profile the session runs on
	profileLabel = "profile"
	// Session label naming the session a sub-agent serves
	parentLabel = "parent"
)

// One endpoint the app opened, with the model it serves
type Endpoint struct {
	Profile  profiles.Profile
	Provider *provider.Provider
	Model    string
	Catalog  profiles.Model
	// Base prompt the profile names, read as the endpoint opened, empty while it names none or the built in one
	System string
}

// Session open in the shell with the stores that log into it and the endpoint its runs go to
type Live struct {
	Session  *session.Session
	Facts    *facts.Store
	Missions *mission.Store
	Endpoint Endpoint
	thinking atomic.Bool
}

// Reports whether the thinking of its runs reaches the transcript
func (l *Live) ShowsThinking() bool { return l.thinking.Load() }

// Hands the thinking of its runs to the transcript or keeps every thinking event out of it
func (l *Live) SetThinking(on bool) { l.thinking.Store(on) }

// Everything built from a configuration, shared by the terminal app and the headless runner
type App struct {
	Config    Config
	Root      string
	Primary   Endpoint
	Tools     *tool.Registry
	Shell     *shell.Shell
	Files     files.Options
	Rules     *policy.Rules
	Mode      *policy.ModeSwitch
	Memory    *policy.Memory
	Gate      policy.Gate
	Approver  *Approver
	Pump      *Pump
	Meter     *Meter
	MCP       *MCP
	Facts     *facts.Store
	Missions  *mission.Store
	Subagents *agent.Subagents
	Store     *session.Store
	Session   *session.Session
	Control   *Controller
	Saver     *Saver
	Flows     *Flows
	System    string
	// Profiles the profile files hold, in file order
	Profiles []Instance
	// Working directory the configuration was discovered under
	Dir      string
	kinds    []Kind
	send     Sender
	lives    []*Live
	mu       sync.Mutex
	children *Endpoint
	summary  *Endpoint
	closed   sync.Once
}

// Options a caller passes beside the configuration
type BuildOptions struct {
	// Session to resume, a new one when empty
	Resume string
	// Working directory of the shell and base of relative paths, the process directory when empty
	Dir string
}

// Opens providers, tools, policy, stores and the bridge from the configuration
func Build(ctx context.Context, cfg Config, send Sender, opts BuildOptions) (*App, error) {
	if send == nil {
		return nil, errors.New("harness: a sender is required")
	}
	dir := opts.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root := cfg.Root
	if root == "" {
		root = dir
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	app := &App{Config: cfg, Root: root, Dir: dir, send: send}
	for _, path := range cfg.CatalogFiles {
		if _, err := profiles.LoadCatalogFile(path); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", path, err)
		}
	}
	var fileDefault string
	if app.Profiles, fileDefault, err = loadInstances(cfg); err != nil {
		return nil, err
	}
	if app.kinds, err = loadKinds(dir); err != nil {
		return nil, err
	}
	app.Pump = NewPump(send)
	app.Meter = NewMeter(send)
	app.Approver = NewApprover(send)
	name := cfg.Profile
	if name == "" {
		name = fileDefault
	}
	if name != "" {
		if app.Primary, err = app.open(ctx, Target{Profile: name, Model: cfg.Model}); err != nil {
			return nil, err
		}
	}
	app.Meter.SetCatalog(app.Primary.Catalog, app.Primary.Profile.Name)
	app.Meter.SetThreshold(cfg.Compaction.Threshold)
	if cfg.Subagents.Model != nil {
		ep, err := app.open(ctx, *cfg.Subagents.Model)
		if err != nil {
			return nil, fmt.Errorf("subagents: %w", err)
		}
		app.children = &ep
	}
	if cfg.Compaction.Model != nil {
		ep, err := app.open(ctx, *cfg.Compaction.Model)
		if err != nil {
			return nil, fmt.Errorf("compaction: %w", err)
		}
		app.summary = &ep
	}
	if app.System, err = cfg.SystemPrompt(); err != nil {
		return nil, err
	}
	sessions, err := cfg.SessionsPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return nil, err
	}
	app.Store = &session.Store{Dir: sessions}
	sess, ep := session.New(), app.Primary
	if opts.Resume != "" {
		if sess, err = app.Store.Load(ctx, opts.Resume); err != nil {
			return nil, fmt.Errorf("resume %s: %w", opts.Resume, err)
		}
		if ep, err = app.endpointFor(ctx, sess); err != nil {
			return nil, fmt.Errorf("resume %s: %w", opts.Resume, err)
		}
	}
	first, err := app.live(sess, ep)
	if err != nil {
		return nil, err
	}
	app.Session, app.Facts, app.Missions = first.Session, first.Facts, first.Missions
	app.Saver = NewSaver(app.Store, send)
	app.Saver.Off = app.Ghosted
	if err := app.buildTools(dir, root); err != nil {
		return nil, err
	}
	if err := app.buildPolicy(root); err != nil {
		return nil, err
	}
	app.Control = NewController(send, app.Pump)
	app.Control.Saver, app.Control.Meter, app.Control.Missions, app.Control.Subagents = app.Saver, app.Meter, app.Missions, app.Subagents
	app.Control.MissionsOf = func(sess *session.Session) *mission.Store {
		if l := app.LiveOf(sess); l != nil {
			return l.Missions
		}
		return nil
	}
	app.Control.Thinking = func(sess *session.Session) bool {
		if l := app.LiveOf(sess); l != nil {
			return l.ShowsThinking()
		}
		return true
	}
	app.Flows = NewFlows(app, send)
	for _, build := range toolsRegistered() {
		tools, err := build(app)
		if err != nil {
			return nil, err
		}
		if err := app.Tools.Add(tools...); err != nil {
			return nil, err
		}
	}
	app.MCP = NewMCP(send, app.Tools)
	app.MCP.Options.Roots = []mcp.Root{{URI: "file://" + filepath.ToSlash(root), Name: filepath.Base(root)}}
	for _, s := range cfg.MCP {
		if err := app.MCP.Add(ctx, s); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// Resolves the profile, opens it and looks the model up in the catalog
func (a *App) open(ctx context.Context, t Target) (Endpoint, error) {
	if t.Profile == "" {
		t.Profile = a.Primary.Profile.Name
		if t.Profile == "" {
			return Endpoint{}, ErrNoProfile
		}
	}
	prof, ok := a.FindProfile(t.Profile)
	if !ok {
		return Endpoint{}, fmt.Errorf("profile %q is not known, built in ones are %s", t.Profile, strings.Join(profileNames(), ", "))
	}
	if prof.Disabled {
		return Endpoint{}, fmt.Errorf("profile %s is turned off", prof.Name)
	}
	system, err := prof.System.Resolve(ctx)
	if err != nil {
		return Endpoint{}, fmt.Errorf("profile %s: system: %w", prof.Name, err)
	}
	prov, err := prof.Open(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	prov.Observer = a.Meter
	model := t.Model
	if model == "" {
		model = prof.Model()
	}
	if model == "" {
		return Endpoint{}, fmt.Errorf("profile %s names no model, pass one", prof.Name)
	}
	return Endpoint{Profile: prof.Resolved(), Provider: prov, Model: model, Catalog: describe(model, prof.Vendor), System: system}, nil
}

// Catalog entry of a model, a bare entry naming the vendor when the catalog does not know it
func describe(model, vendor string) profiles.Model {
	known, ok := profiles.Describe(model)
	if !ok {
		known = profiles.Model{Vendor: vendor}
	}
	known.ID = model
	return known
}

func profileNames() []string {
	var out []string
	for _, p := range profiles.Defaults() {
		out = append(out, p.Name)
	}
	return out
}

// Completer that follows the primary endpoint as it changes after build
type primary struct{ a *App }

func (p primary) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	if p.a.Primary.Provider == nil {
		return protocol.Response{}, ErrNoProfile
	}
	req.Model = p.a.Primary.Model
	return p.a.Primary.Provider.Complete(ctx, req)
}

func (a *App) buildTools(dir, root string) error {
	cfg := a.Config
	a.Tools = &tool.Registry{}
	a.Files = files.Options{Dir: dir, Root: root}
	sh, err := shell.New(shell.Options{Dir: dir, Env: cfg.Shell.Env, Timeout: seconds(cfg.Shell.Timeout), MaxTimeout: seconds(cfg.Shell.MaxTimeout), OnJob: func(ev shell.JobEvent) { a.send(JobMsg{Event: ev}) }})
	if err != nil {
		return err
	}
	a.Shell = sh
	if err := a.Tools.Add(files.All(a.Files)...); err != nil {
		return err
	}
	if err := a.Tools.Add(sh.Tools()...); err != nil {
		return err
	}
	if !cfg.Web.Disabled {
		fetch, err := web.Fetch(web.Options{AllowPrivate: cfg.Web.AllowPrivate, Summarizer: primary{a}})
		if err != nil {
			return err
		}
		if err := a.Tools.Add(fetch); err != nil {
			return err
		}
	}
	a.Subagents = &agent.Subagents{MaxDepth: cfg.Subagents.MaxDepth, Parallel: cfg.Subagents.Parallel, Timeout: seconds(cfg.Subagents.Timeout), Factory: a.childFactory, Kinds: a.agentKinds}
	for _, name := range cfg.Tools.Disabled {
		a.Tools.SetEnabled(name, false)
	}
	return a.Tools.Add(facts.Tool{Resolve: a.FactsFor}, mission.Tool{Resolve: a.MissionsFor}, a.Subagents)
}

// Builds a child agent on a fresh session labeled with its parent, on the kind endpoint, the subagent one or the parent one
func (a *App) childFactory(ctx context.Context, kind string) (*agent.Agent, error) {
	sess := session.New()
	parent, ep := a.Session.ID(), a.endpointOf(a.Session)
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		parent, ep = l.Session.ID(), l.Endpoint
	}
	if a.children != nil {
		ep = *a.children
	}
	if kind != "" {
		k, ok := a.Kind(kind)
		if !ok {
			return nil, fmt.Errorf("no kind %q", kind)
		}
		if k.Model != nil {
			var err error
			if ep, err = a.open(ctx, *k.Model); err != nil {
				return nil, fmt.Errorf("kind %s: %w", kind, err)
			}
		}
		sess.Label(kindLabel, kind)
	}
	sess.Label(parentLabel, parent)
	sess.Label("agent", policy.AgentFrom(ctx))
	return a.newAgent("", sess, ep)
}

func (a *App) buildPolicy(root string) error {
	cfg := a.Config
	pol, err := policy.NewRules(cfg.Permissions)
	if err != nil {
		return err
	}
	pol.Dir = root
	pol.DirFunc = a.Shell.Dir
	a.Rules = pol
	start := policy.ModeDefault
	if cfg.Mode != "" {
		if start, err = policy.ParseMode(cfg.Mode); err != nil {
			return err
		}
	}
	a.Mode = policy.Compose(start, pol, a.Tools)
	a.Memory = &policy.Memory{Ask: a.Approver, Key: pol.Key}
	a.Gate = policy.Gate{Policy: a.Mode, Approver: a.Memory, Tools: a.Tools}
	return nil
}

// Builds an agent on the session with every configured hook, the endpoint of the session and the shared gate
func (a *App) NewAgent(name string, sess *session.Session) (*agent.Agent, error) {
	return a.newAgent(name, sess, a.endpointOf(sess))
}

// Builds an agent on the session that runs on the endpoint, as the kind of the session when it has one
func (a *App) newAgent(name string, sess *session.Session, ep Endpoint) (*agent.Agent, error) {
	if ep.Provider == nil {
		return nil, ErrNoProfile
	}
	cfg := a.Config
	kind, _ := a.KindOf(sess)
	ag := &agent.Agent{Name: name, Model: ep.Provider, ModelID: ep.Model, Tools: a.Tools, Gate: a.Gate, Session: sess, MaxTokens: cfg.MaxTokens, Temperature: cfg.Temperature, Extra: cfg.Extra, MaxRounds: cfg.MaxRounds}
	if ag.MaxTokens == 0 && protocol.RequiresMaxTokens(ep.Provider.Protocol) {
		ag.MaxTokens = ep.Catalog.MaxOutput
	}
	if kind.MaxRounds > 0 {
		ag.MaxRounds = kind.MaxRounds
	}
	ag.Hooks = append(ag.Hooks, systemHook{app: a, ep: ep, kind: kind})
	var runner *agent.HookRunner
	if len(cfg.Hooks) > 0 {
		transcript, _ := a.Store.Path(sess.ID())
		r := agent.HookRunner{Config: cfg.Hooks, Dir: a.Root, SessionID: sess.ID(), Transcript: transcript, Log: hookLog{send: a.send, run: name}}
		runner = &r
		ag.Hooks = append(ag.Hooks, r)
	}
	ag.Hooks = append(ag.Hooks, agent.StripThinking{}, missionTools{})
	if kind.Name != "" {
		ag.Hooks = append(ag.Hooks, kindTools{kind: kind})
	}
	info := AgentInfo{Name: name, Session: sess, Endpoint: ep, Kind: kind, App: a}
	for _, build := range hooksRegistered() {
		if h := build(info); h != nil {
			ag.Hooks = append(ag.Hooks, h)
		}
	}
	ag.Hooks = append(ag.Hooks, kind.Hooks...)
	if cfg.Compaction.Threshold > 0 {
		s := &agent.Summarizer{Model: ep.Provider, ModelID: ep.Model, Session: sess, Threshold: cfg.Compaction.Threshold, Keep: cfg.Compaction.Keep, Instructions: cfg.Compaction.Instructions}
		if a.summary != nil {
			s.Model, s.ModelID = a.summary.Provider, a.summary.Model
		}
		if runner != nil {
			s.Before = func(ctx context.Context) error {
				_, err := runner.Fire(ctx, agent.PreCompact, map[string]any{"trigger": "auto"})
				return err
			}
		}
		ag.Hooks = append(ag.Hooks, s)
	}
	return ag, nil
}

// Writer that turns hook output into messages
type hookLog struct {
	send Sender
	run  string
}

func (h hookLog) Write(p []byte) (int, error) {
	h.send(HookOutputMsg{Run: h.run, Text: strings.TrimRight(string(p), "\n")})
	return len(p), nil
}

// Starts a run of a fresh root agent on the app session
func (a *App) Run(ctx context.Context, missionID string, input []protocol.Part) (*Run, error) {
	return a.start(ctx, a.Session, missionID, input)
}

// Starts a run on an open session
func (a *App) RunOn(ctx context.Context, l *Live, missionID string, input []protocol.Part) (*Run, error) {
	return a.start(ctx, l.Session, missionID, input)
}

func (a *App) start(ctx context.Context, sess *session.Session, missionID string, input []protocol.Part) (*Run, error) {
	ag, err := a.NewAgent("", sess)
	if err != nil {
		return nil, err
	}
	return a.Control.Start(ctx, ag, missionID, input)
}

// Fires the SessionStart hooks
func (a *App) SessionStart(ctx context.Context, source string) error {
	if len(a.Config.Hooks) == 0 {
		return nil
	}
	r := agent.HookRunner{Config: a.Config.Hooks, Dir: a.Root, SessionID: a.Session.ID(), Log: hookLog{send: a.send}}
	_, err := r.Fire(ctx, agent.SessionStart, map[string]any{"source": source})
	return err
}

// Stops runs, closes servers and the shell, saves every open session and fires the SessionEnd hooks
func (a *App) Close(ctx context.Context) error {
	var err error
	a.closed.Do(func() {
		a.Flows.Shutdown()
		a.Control.Shutdown()
		a.Approver.Close()
		a.MCP.Close()
		a.Shell.Close()
		for _, l := range a.Lives() {
			a.Saver.Flush(l.Session)
		}
		a.Saver.Wait()
		if len(a.Config.Hooks) > 0 {
			r := agent.HookRunner{Config: a.Config.Hooks, Dir: a.Root, SessionID: a.Session.ID(), Log: hookLog{send: a.send}}
			_, err = r.Fire(ctx, agent.SessionEnd, map[string]any{"reason": "exit"})
		}
	})
	return err
}

// Hook that writes the system prompt of the endpoint and kind into every turn, keeping an empty one while bare
type systemHook struct {
	agent.Hooks
	app  *App
	ep   Endpoint
	kind Kind
}

func (h systemHook) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	req.System = h.app.PromptFor(ctx, h.ep, h.kind)
	req.KeepSystem = h.ep.Profile.Bare
	return nil
}

// Hook that withholds the Plan tool from runs with no mission attached
type missionTools struct {
	agent.Hooks
}

func (missionTools) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	if mission.IDFrom(ctx) != "" {
		return nil
	}
	kept := req.Tools[:0]
	for _, t := range req.Tools {
		if t.Name != mission.ToolName {
			kept = append(kept, t)
		}
	}
	req.Tools = kept
	return nil
}

// Wraps a session in stores restored from its records, binds it to the endpoint and registers it
func (a *App) live(sess *session.Session, ep Endpoint) (*Live, error) {
	l := &Live{Session: sess, Facts: facts.New(), Missions: mission.New(), Endpoint: ep}
	l.SetThinking(ep.Profile.ShowsThinking())
	if err := l.Facts.Restore(sess); err != nil {
		return nil, err
	}
	if err := l.Missions.Restore(sess); err != nil {
		return nil, err
	}
	l.Facts.Attach(sess)
	l.Missions.Attach(sess)
	if sess.Model() != ep.Model {
		sess.SetModel(ep.Model)
	}
	if sess.Labels()[profileLabel] != ep.Profile.Name {
		sess.Label(profileLabel, ep.Profile.Name)
	}
	id := sess.ID()
	l.Facts.OnChange(func(c facts.Change) { a.send(FactMsg{Session: id, Change: c}) })
	l.Missions.OnChange(func(c mission.Change) { a.send(MissionMsg{Session: id, Mission: c.Mission, Op: c.Op}) })
	a.mu.Lock()
	a.lives = append(a.lives, l)
	a.mu.Unlock()
	return l, nil
}

// Endpoint a stored session resumes on, its labeled profile and model, else the default
func (a *App) endpointFor(ctx context.Context, sess *session.Session) (Endpoint, error) {
	name := sess.Labels()[profileLabel]
	if name == "" {
		return a.Primary, nil
	}
	if name == a.Primary.Profile.Name {
		return withModel(a.Primary, sess.Model()), nil
	}
	if _, ok := a.FindProfile(name); !ok {
		return a.Primary, nil
	}
	return a.open(ctx, Target{Profile: name, Model: sess.Model()})
}

// Endpoint with the model swapped for another the same profile serves, looked up in the catalog
func withModel(ep Endpoint, id string) Endpoint {
	if id == "" {
		return ep
	}
	ep.Model = id
	ep.Catalog = describe(id, ep.Profile.Vendor)
	return ep
}

// Endpoint the runs of a session go to, the default for a session that is not open
func (a *App) endpointOf(sess *session.Session) Endpoint {
	if l := a.LiveOf(sess); l != nil {
		return l.Endpoint
	}
	return a.Primary
}

// Opens a stored session beside the ones already open, returning the open one when it already is
func (a *App) Open(ctx context.Context, id string) (*Live, error) {
	if l := a.LiveByID(id); l != nil {
		return l, nil
	}
	sess, err := a.Store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	ep, err := a.endpointFor(ctx, sess)
	if err != nil {
		return nil, err
	}
	return a.live(sess, ep)
}

// Opens a fresh session on the default endpoint beside the ones already open
func (a *App) Create() (*Live, error) {
	return a.live(session.New(), a.Primary)
}

// Opens a fork of an open session cut at n entries on the same endpoint and writes it to the store
func (a *App) Fork(l *Live, n int) (*Live, error) {
	if a.Busy(l) {
		return nil, errors.New("harness: an agent is still working")
	}
	f := l.Session.Fork()
	f.Truncate(n)
	nl, err := a.live(f, l.Endpoint)
	if err != nil {
		return nil, err
	}
	a.Saver.Flush(f)
	return nl, nil
}

// Changes the model of an open session, moving the default along when the session is on the default profile
func (a *App) SetModelOn(l *Live, id string) {
	l.Endpoint = withModel(l.Endpoint, id)
	l.Session.SetModel(id)
	if l.Endpoint.Profile.Name == a.Primary.Profile.Name {
		a.SetDefaultModel(id)
	}
	if l.Session == a.Session {
		a.Meter.SetCatalog(l.Endpoint.Catalog, l.Endpoint.Profile.Name)
	}
}

// Changes the model new sessions start on
func (a *App) SetDefaultModel(id string) {
	a.Primary = withModel(a.Primary, id)
	a.Config.Model = id
}

// Moves an open session to an opened endpoint, now the default, forgetting its stored copy when the profile is ghost
func (a *App) SetEndpointOn(ctx context.Context, l *Live, name string, ep Endpoint) error {
	if a.Busy(l) {
		return errors.New("harness: an agent is still working")
	}
	was := l.Endpoint.Profile.Ghost
	l.Endpoint = ep
	l.SetThinking(ep.Profile.ShowsThinking())
	l.Session.SetModel(ep.Model)
	l.Session.Label(profileLabel, ep.Profile.Name)
	a.SetDefaultEndpoint(name, ep)
	if l.Session == a.Session {
		a.Meter.SetCatalog(ep.Catalog, ep.Profile.Name)
	}
	switch {
	case ep.Profile.Ghost && !was:
		return a.forget(ctx, l.Session)
	case was && !ep.Profile.Ghost:
		a.Saver.Touch(l.Session)
	}
	return nil
}

// Deletes the stored copies of the session and of every sub-agent session that served it
func (a *App) forget(ctx context.Context, s *session.Session) error {
	infos, err := a.Store.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range infos {
		if in.ID == s.ID() || in.Labels[parentLabel] == s.ID() {
			errs = append(errs, a.Store.Delete(ctx, in.ID))
		}
	}
	return errors.Join(errs...)
}

// Reports whether the session stays off disk, by the ghost of its profile or of the session it serves
func (a *App) Ghosted(s *session.Session) bool {
	if l := a.LiveOf(s); l != nil {
		return l.Endpoint.Profile.Ghost
	}
	if id := s.Labels()[parentLabel]; id != "" {
		if a.children != nil && a.children.Profile.Ghost {
			return true
		}
		if l := a.LiveByID(id); l != nil {
			return l.Endpoint.Profile.Ghost
		}
	}
	return false
}

// Moves new sessions to an opened endpoint
func (a *App) SetDefaultEndpoint(name string, ep Endpoint) {
	a.Primary = ep
	a.Config.Profile, a.Config.Model = name, ep.Model
}

// Opens a profile as an endpoint without changing the app, usable off the event loop
func (a *App) OpenEndpoint(ctx context.Context, name, model string) (Endpoint, error) {
	if name == "" {
		return Endpoint{}, errors.New("harness: a profile name is required")
	}
	return a.open(ctx, Target{Profile: name, Model: model})
}

// Reports whether the profile is the default or any open session runs on it
func (a *App) ProfileInUse(name string) bool {
	if a.Primary.Profile.Name == name {
		return true
	}
	for _, l := range a.Lives() {
		if l.Endpoint.Profile.Name == name {
			return true
		}
	}
	return false
}

// Writes an open session that holds anything and forgets it, selecting another when it was the selected one
func (a *App) Release(l *Live) error {
	if a.Busy(l) {
		return errors.New("harness: an agent is still working")
	}
	a.Saver.Flush(l.Session)
	a.mu.Lock()
	kept := a.lives[:0]
	for _, have := range a.lives {
		if have != l {
			kept = append(kept, have)
		}
	}
	a.lives = kept
	var first *Live
	if len(kept) > 0 && a.Session == l.Session {
		first = kept[0]
	}
	a.mu.Unlock()
	if first != nil {
		a.Select(first)
	}
	return nil
}

// Makes an open session the one hooks, the meter and the headless runner work on
func (a *App) Select(l *Live) {
	a.Session, a.Facts, a.Missions = l.Session, l.Facts, l.Missions
	a.Control.Missions = l.Missions
	a.Meter.SetCatalog(l.Endpoint.Catalog, l.Endpoint.Profile.Name)
}

// Open sessions in open order
func (a *App) Lives() []*Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Live(nil), a.lives...)
}

// Open session the app works on
func (a *App) Current() *Live {
	return a.LiveOf(a.Session)
}

// Open session wrapping sess, nil when it is not open
func (a *App) LiveOf(sess *session.Session) *Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lives {
		if l.Session == sess {
			return l
		}
	}
	return nil
}

// Open session with the id, nil when it is not open
func (a *App) LiveByID(id string) *Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lives {
		if l.Session.ID() == id {
			return l
		}
	}
	return nil
}

// Reports whether a root run goes on the open session
func (a *App) Busy(l *Live) bool {
	for _, r := range a.Control.Runs() {
		if r.Running() && r.Parent == "" && r.Session == l.Session {
			return true
		}
	}
	return false
}

// Open session a run serves, found through the root of a child run
func (a *App) LiveOfRun(id string) *Live {
	for id != "" {
		r, ok := a.Control.Get(id)
		if !ok {
			return nil
		}
		if l := a.LiveOf(r.Session); l != nil {
			return l
		}
		id = r.Parent
	}
	return nil
}

// Facts store the calling run records into, the selected one outside a run
func (a *App) FactsFor(ctx context.Context) *facts.Store {
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		return l.Facts
	}
	return a.Facts
}

// Mission store the calling run plans into, the selected one outside a run
func (a *App) MissionsFor(ctx context.Context) *mission.Store {
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		return l.Missions
	}
	return a.Missions
}

// Opens an empty session under a stored id whose file is gone, on the default endpoint
func (a *App) Revive(id string) (*Live, error) {
	if !session.ValidID(id) {
		return nil, fmt.Errorf("harness: %q is not a session id", id)
	}
	if l := a.LiveByID(id); l != nil {
		return l, nil
	}
	now := time.Now().UTC()
	return a.live(session.FromHeader(session.Header{ID: id, Created: now, Updated: now}, nil, nil), a.Primary)
}
