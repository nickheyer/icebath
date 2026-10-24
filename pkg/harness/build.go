package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agent/compact"
	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/agent/subagent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/judge"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/failover"
	"github.com/nickheyer/icebath/pkg/provider/record"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/session/jsonl"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/tool/web"
)

// Profile used when neither the configuration nor a profile file names one
const DefaultProfile = "anthropic"

// Output budget when the configuration names none
const DefaultMaxTokens = 16000

func parseMode(s string) (mode.Mode, error) { return mode.Parse(s) }

// One endpoint the app opened, with the model it serves
type Endpoint struct {
	Profile  profiles.Profile
	Provider *provider.Provider
	Model    string
	Catalog  profiles.Model
}

// Everything built from a configuration, shared by the terminal app and the headless runner
type App struct {
	Config    Config
	Root      string
	Primary   Endpoint
	Fallbacks []Endpoint
	Model     agent.Model
	Tools     *tool.Registry
	Shell     *shell.Shell
	Files     files.Options
	Rules     *rules.Policy
	Mode      *mode.Switch
	Memory    *policy.Memory
	Gate      policy.Gate
	Approver  *Approver
	Pump      *Pump
	Meter     *Meter
	Workspace *Workspace
	MCP       *MCP
	Facts     *facts.Store
	Missions  *mission.Store
	Subagents *subagent.Tool
	Store     session.Store
	Session   *session.Session
	Control   *Controller
	Saver     *Saver
	PreImage  *PreImage
	Cassette  *record.Cassette
	Journal   *Journal
	System    string
	// Profiles the profile files hold, in file order
	Profiles []Instance
	// Working directory the configuration was discovered under
	Dir      string
	send     Sender
	lives    []*Live
	mu       sync.Mutex
	children *Endpoint
	judge    *Endpoint
	summary  *Endpoint
	closed   sync.Once
}

// Options a caller passes beside the configuration
type BuildOptions struct {
	// Session to resume, a new one when empty
	Resume string
	// Working directory of the shell and base of relative paths, the process directory when empty
	Dir string
	// Receives hook output and other diagnostics
	Log io.Writer
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
	journal := NewJournal()
	send = journal.Tap(send)
	app := &App{Config: cfg, Root: root, Dir: dir, send: send, Journal: journal}
	for _, path := range cfg.CatalogFiles {
		if _, err := profiles.LoadCatalogFile(path); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", path, err)
		}
	}
	var fileDefault string
	if app.Profiles, fileDefault, err = loadInstances(cfg); err != nil {
		return nil, err
	}
	app.Pump = NewPump(send)
	app.Meter = NewMeter(send)
	app.Approver = NewApprover(send)
	app.Workspace = NewWorkspace(dir)
	if cfg.Record != nil && cfg.Record.Path != "" {
		m, err := recordMode(cfg.Record.Mode)
		if err != nil {
			return nil, err
		}
		if app.Cassette, err = record.Open(cfg.Record.Path, m); err != nil {
			return nil, err
		}
	}
	name := cfg.Profile
	if name == "" {
		name = fileDefault
	}
	if name == "" {
		name = DefaultProfile
	}
	if app.Primary, err = app.open(ctx, Target{Profile: name, Model: cfg.Model}); err != nil {
		return nil, err
	}
	app.Meter.SetCatalog(app.Primary.Catalog, app.Primary.Profile.Name)
	app.Meter.SetThreshold(cfg.Compaction.Threshold)
	for _, t := range cfg.Fallbacks {
		ep, err := app.open(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("fallback: %w", err)
		}
		app.Fallbacks = append(app.Fallbacks, ep)
	}
	app.Model = app.chain(app.Primary)
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
	if cfg.Judge != nil {
		ep, err := app.open(ctx, cfg.Judge.Target)
		if err != nil {
			return nil, fmt.Errorf("judge: %w", err)
		}
		app.judge = &ep
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
	app.Store = &jsonl.Store{Dir: sessions}
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
	if err := app.buildTools(ctx, dir, root); err != nil {
		return nil, err
	}
	if err := app.buildPolicy(root); err != nil {
		return nil, err
	}
	app.PreImage = &PreImage{Files: app.Files}
	app.Control = NewController(send, app.Pump)
	app.Control.Saver, app.Control.Meter, app.Control.Workspace, app.Control.Missions, app.Control.Subagents = app.Saver, app.Meter, app.Workspace, app.Missions, app.Subagents
	app.Control.MissionsOf = func(sess *session.Session) *mission.Store {
		if l := app.LiveOf(sess); l != nil {
			return l.Missions
		}
		return nil
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
			t.Profile = DefaultProfile
		}
	}
	prof, ok := a.FindProfile(t.Profile)
	if !ok {
		return Endpoint{}, fmt.Errorf("profile %q is not known, built in ones are %s", t.Profile, strings.Join(profileNames(), ", "))
	}
	if prof.Disabled {
		return Endpoint{}, fmt.Errorf("profile %s is turned off", prof.Name)
	}
	prov, err := prof.Open(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	prov.Observer = a.Meter
	if a.Cassette != nil {
		var next http.RoundTripper
		if prov.Client != nil {
			next = prov.Client.Transport
		}
		prov.Client = a.Cassette.Client(next)
	}
	model := t.Model
	if model == "" {
		model = prof.Model()
	}
	if model == "" {
		return Endpoint{}, fmt.Errorf("profile %s names no model, pass one", prof.Name)
	}
	ep := Endpoint{Profile: prof.Resolved(), Provider: prov, Model: model}
	if known, ok := profiles.Describe(model); ok {
		ep.Catalog = known
	} else {
		ep.Catalog = profiles.Model{ID: model, Vendor: prof.Vendor}
	}
	ep.Catalog.ID = model
	return ep, nil
}

func profileNames() []string {
	var out []string
	for _, p := range profiles.Defaults() {
		out = append(out, p.Name)
	}
	return out
}

func recordMode(s string) (record.Mode, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return record.Auto, nil
	case "record":
		return record.Record, nil
	case "replay":
		return record.Replay, nil
	}
	return 0, fmt.Errorf("record mode %q is not auto, record or replay", s)
}

// Failover chain that asks the endpoint first and then every fallback under its own model id
func (a *App) chain(first Endpoint) failover.Chain {
	c := failover.Chain{OnFail: a.Meter.Failover, OnServe: a.Meter.Served}
	c.Models = append(c.Models, first.Provider)
	for _, f := range a.Fallbacks {
		c.Models = append(c.Models, remodel{Model: f.Provider, id: f.Model})
	}
	return c
}

// Completer that follows the primary endpoint and its fallbacks as they change after build
type primary struct{ a *App }

func (p primary) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	req.Model = p.a.Primary.Model
	return p.a.chain(p.a.Primary).Complete(ctx, req)
}

// Model that sends every request under its own model id
type remodel struct {
	failover.Model
	id string
}

func (r remodel) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	req.Model = r.id
	return r.Model.Complete(ctx, req)
}

func (r remodel) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	req.Model = r.id
	return r.Model.Stream(ctx, req)
}

func (a *App) buildTools(_ context.Context, dir, root string) error {
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
		fetch, err := web.Fetch(web.Options{AllowPrivate: cfg.Web.AllowPrivate, Summarizer: primary{a}, Model: a.Primary.Model})
		if err != nil {
			return err
		}
		if err := a.Tools.Add(fetch); err != nil {
			return err
		}
	}
	a.Subagents = &subagent.Tool{MaxDepth: cfg.Subagents.MaxDepth, Parallel: cfg.Subagents.Parallel, Timeout: seconds(cfg.Subagents.Timeout), Factory: a.childFactory}
	for _, name := range cfg.Tools.Disabled {
		a.Tools.SetEnabled(name, false)
	}
	return a.Tools.Add(facts.Tool{Resolve: a.factsFor}, mission.Tool{Resolve: a.missionsFor}, a.Subagents)
}

// Builds a child agent on a fresh session labeled with its parent, on the subagent endpoint or the parent one
func (a *App) childFactory(ctx context.Context) (*agent.Agent, error) {
	sess := session.New()
	parent, ep := a.Session.ID(), a.endpointOf(a.Session)
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		parent, ep = l.Session.ID(), l.Endpoint
	}
	if a.children != nil {
		ep = *a.children
	}
	sess.Label("parent", parent)
	sess.Label("agent", policy.AgentFrom(ctx))
	return a.newAgent("", sess, ep), nil
}

func (a *App) buildPolicy(root string) error {
	cfg := a.Config
	pol, err := rules.New(cfg.Permissions)
	if err != nil {
		return err
	}
	pol.Dir = root
	pol.DirFunc = a.Shell.Dir
	a.Rules = pol
	start := mode.Default
	if cfg.Mode != "" {
		if start, err = mode.Parse(cfg.Mode); err != nil {
			return err
		}
	}
	a.Mode = mode.Compose(start, pol, a.Tools)
	var ask policy.Asker = a.Approver
	if a.judge != nil {
		j := judge.Approver{Model: a.judge.Provider, ModelID: a.judge.Model, Instructions: cfg.Judge.Instructions, Escalate: policy.FromAsker(a.Approver), Timeout: seconds(cfg.Judge.Timeout)}
		j.Context = func(ctx context.Context, req policy.Request) string {
			return "Working directory: " + a.Shell.Dir()
		}
		ask = policy.ToAsker(j)
	}
	a.Memory = &policy.Memory{Ask: ask, Key: pol.Key}
	a.Gate = policy.Gate{Policy: a.Mode, Approver: a.Memory, Tools: a.Tools}
	return nil
}

// Builds an agent on the session with every configured hook, the endpoint of the session and the shared gate
func (a *App) NewAgent(name string, sess *session.Session) *agent.Agent {
	return a.newAgent(name, sess, a.endpointOf(sess))
}

// Builds an agent on the session that runs on the endpoint ahead of the fallbacks
func (a *App) newAgent(name string, sess *session.Session, ep Endpoint) *agent.Agent {
	cfg := a.Config
	ag := &agent.Agent{Name: name, Model: a.chain(ep), ModelID: ep.Model, Tools: a.Tools, Gate: a.Gate, Session: sess, MaxTokens: cfg.MaxTokens, Temperature: cfg.Temperature, Extra: cfg.Extra, MaxRounds: cfg.MaxRounds}
	if ag.MaxTokens <= 0 {
		ag.MaxTokens = DefaultMaxTokens
	}
	ag.Hooks = append(ag.Hooks, systemHook{app: a, model: ep.Model})
	var runner *hooks.Runner
	if len(cfg.Hooks) > 0 {
		transcript := ""
		if p, ok := a.Store.(session.Pather); ok {
			transcript, _ = p.Path(sess.ID())
		}
		r := hooks.Runner{Config: cfg.Hooks, Dir: a.Root, SessionID: sess.ID(), Transcript: transcript, Log: hookLog{send: a.send, run: name}}
		runner = &r
		ag.Hooks = append(ag.Hooks, r)
	}
	ag.Hooks = append(ag.Hooks, a.PreImage, compact.StripThinking{}, missionTools{})
	if cfg.Compaction.Threshold > 0 {
		s := &compact.Summarizer{Model: ag.Model, ModelID: ep.Model, Session: sess, Threshold: cfg.Compaction.Threshold, Keep: cfg.Compaction.Keep, Instructions: cfg.Compaction.Instructions}
		if a.summary != nil {
			s.Model, s.ModelID = a.summary.Provider, a.summary.Model
		}
		if runner != nil {
			s.Before = func(ctx context.Context) error {
				_, err := runner.Fire(ctx, hooks.PreCompact, map[string]any{"trigger": "auto"})
				return err
			}
		}
		ag.Hooks = append(ag.Hooks, s)
	}
	return ag
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
	ag := a.NewAgent("", a.Session)
	return a.Control.Start(ctx, ag, missionID, input)
}

// Fires the SessionStart hooks
func (a *App) SessionStart(ctx context.Context, source string) error {
	if len(a.Config.Hooks) == 0 {
		return nil
	}
	r := hooks.Runner{Config: a.Config.Hooks, Dir: a.Root, SessionID: a.Session.ID(), Log: hookLog{send: a.send}}
	_, err := r.Fire(ctx, hooks.SessionStart, map[string]any{"source": source})
	return err
}

// Stops runs, closes servers and the shell, saves the session and fires the SessionEnd hooks
func (a *App) Close(ctx context.Context) error {
	var err error
	a.closed.Do(func() {
		a.Control.Shutdown()
		a.Approver.Close()
		a.MCP.Close()
		a.Shell.Close()
		for _, l := range a.Lives() {
			a.Saver.Flush(l.Session)
		}
		a.Saver.Wait()
		if a.Cassette != nil {
			err = a.Cassette.Save()
		}
		if len(a.Config.Hooks) > 0 {
			r := hooks.Runner{Config: a.Config.Hooks, Dir: a.Root, SessionID: a.Session.ID(), Log: hookLog{send: a.send}}
			if _, herr := r.Fire(ctx, hooks.SessionEnd, map[string]any{"reason": "exit"}); herr != nil {
				err = errors.Join(err, herr)
			}
		}
	})
	return err
}

// Replaces the selected session with sess, releasing the old one
func (a *App) adopt(sess *session.Session, ep Endpoint) error {
	old := a.Current()
	l, err := a.live(sess, ep)
	if err != nil {
		return err
	}
	a.Select(l)
	a.Meter.Reset()
	if old != nil {
		return a.Release(old)
	}
	return nil
}

// Switches the selected session to a stored one, restoring its facts and missions
func (a *App) Switch(ctx context.Context, id string) error {
	if cur := a.Current(); cur != nil && a.Busy(cur) {
		return errors.New("harness: an agent is still working")
	}
	sess, err := a.Store.Load(ctx, id)
	if err != nil {
		return err
	}
	ep, err := a.endpointFor(ctx, sess)
	if err != nil {
		return err
	}
	return a.adopt(sess, ep)
}

// Starts a new empty session on the default endpoint in place of the selected one, which stays on disk
func (a *App) Fresh() error {
	if cur := a.Current(); cur != nil && a.Busy(cur) {
		return errors.New("harness: an agent is still working")
	}
	return a.adopt(session.New(), a.Primary)
}

// Forks the selected session at an entry count and selects the fork with its facts, missions and endpoint
func (a *App) ForkAt(n int) (*session.Session, error) {
	cur := a.Current()
	if cur != nil && a.Busy(cur) {
		return nil, errors.New("harness: an agent is still working")
	}
	f := a.Session.Fork()
	f.Truncate(n)
	if err := a.adopt(f, a.endpointOf(a.Session)); err != nil {
		return nil, err
	}
	a.Saver.Flush(f)
	return f, nil
}

// Changes the model of the selected session, and the default along with it
func (a *App) SetModel(id string) {
	if cur := a.Current(); cur != nil {
		a.SetModelOn(cur, id)
		return
	}
	a.SetDefaultModel(id)
	a.Meter.SetCatalog(a.Primary.Catalog, a.Primary.Profile.Name)
}

// Opens a profile as an endpoint without changing the app, usable off the event loop
func (a *App) OpenEndpoint(ctx context.Context, name, model string) (Endpoint, error) {
	if name == "" {
		return Endpoint{}, errors.New("harness: a profile name is required")
	}
	return a.open(ctx, Target{Profile: name, Model: model})
}

// Moves the selected session to an opened endpoint, which becomes the default, keeping fallbacks and the tools
func (a *App) SetEndpoint(name string, ep Endpoint) error {
	if cur := a.Current(); cur != nil {
		return a.SetEndpointOn(cur, name, ep)
	}
	a.SetDefaultEndpoint(name, ep)
	a.Meter.SetCatalog(ep.Catalog, ep.Profile.Name)
	return nil
}

// System prompt for a run as the machine, directory, model and mission stand right now
func (a *App) SystemPrompt(ctx context.Context, model string) string {
	return Prompt(GatherFacts(a.Shell.Dir(), a.Root, model, mission.IDFrom(ctx) != ""), a.System)
}

// Hook that writes the live system prompt into every turn, naming the model the agent runs on
type systemHook struct {
	agent.Hooks
	app   *App
	model string
}

func (h systemHook) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	req.System = h.app.SystemPrompt(ctx, h.model)
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
