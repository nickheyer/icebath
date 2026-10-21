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
	System    string
	// Profiles known after loading every list
	Profiles []profiles.Profile
	send     Sender
	children Endpoint
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
	app := &App{Config: cfg, Root: root, send: send}
	for _, path := range cfg.CatalogFiles {
		if _, err := profiles.LoadCatalogFile(path); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", path, err)
		}
	}
	var fileDefault string
	if app.Profiles, fileDefault, err = loadProfiles(cfg); err != nil {
		return nil, err
	}
	app.Pump = NewPump(send)
	app.Meter = NewMeter(send)
	app.Approver = NewApprover(send)
	app.Workspace = NewWorkspace(dir)
	app.Facts = facts.New()
	app.Missions = mission.New()
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
	app.children = app.Primary
	if cfg.Subagents.Model != nil {
		if app.children, err = app.open(ctx, *cfg.Subagents.Model); err != nil {
			return nil, fmt.Errorf("subagents: %w", err)
		}
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
	if opts.Resume != "" {
		if app.Session, err = app.Store.Load(ctx, opts.Resume); err != nil {
			return nil, fmt.Errorf("resume %s: %w", opts.Resume, err)
		}
		if err := app.Facts.Restore(app.Session); err != nil {
			return nil, err
		}
		if err := app.Missions.Restore(app.Session); err != nil {
			return nil, err
		}
	} else {
		app.Session = session.New()
		app.Session.SetModel(app.Primary.Model)
	}
	app.Facts.Attach(app.Session)
	app.Missions.Attach(app.Session)
	app.Facts.OnChange(func(c facts.Change) { send(FactMsg{Change: c}) })
	app.Missions.OnChange(func(c mission.Change) { send(MissionMsg{Mission: c.Mission, Op: c.Op}) })
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
	prof, ok := profiles.Find(t.Profile, a.Profiles)
	if !ok {
		return Endpoint{}, fmt.Errorf("profile %q is not known, built in ones are %s", t.Profile, strings.Join(profileNames(), ", "))
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

// Loads extra profile lists and the user list over the built in ones, the last default any file names winning
func loadProfiles(cfg Config) ([]profiles.Profile, string, error) {
	var out []profiles.Profile
	var def string
	for _, path := range cfg.ProfileFiles {
		f, err := profiles.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		out = profiles.Merge(out, f.Profiles)
		if f.Default != "" {
			def = f.Default
		}
	}
	user, err := profiles.ReadUser()
	if err != nil {
		return nil, "", err
	}
	if user.Default != "" {
		def = user.Default
	}
	return profiles.Merge(out, user.Profiles), def, nil
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
	return a.Tools.Add(facts.Tool{Store: a.Facts}, mission.Tool{Store: a.Missions}, a.Subagents)
}

// Builds a child agent on a fresh session labeled with its parent, on the subagent endpoint ahead of the fallbacks
func (a *App) childFactory(ctx context.Context) (*agent.Agent, error) {
	sess := session.New()
	sess.Label("parent", a.Session.ID())
	sess.Label("agent", policy.AgentFrom(ctx))
	child := a.NewAgent("", sess)
	child.Model = a.chain(a.children)
	child.ModelID = a.children.Model
	return child, nil
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

// Builds an agent on the session with every configured hook, the primary model and the shared gate
func (a *App) NewAgent(name string, sess *session.Session) *agent.Agent {
	cfg := a.Config
	ag := &agent.Agent{Name: name, Model: a.Model, ModelID: a.Primary.Model, System: a.System, Tools: a.Tools, Gate: a.Gate, Session: sess, MaxTokens: cfg.MaxTokens, Temperature: cfg.Temperature, Extra: cfg.Extra, MaxRounds: cfg.MaxRounds}
	if ag.MaxTokens <= 0 {
		ag.MaxTokens = DefaultMaxTokens
	}
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
	ag.Hooks = append(ag.Hooks, a.PreImage, compact.StripThinking{})
	if cfg.Compaction.Threshold > 0 {
		s := &compact.Summarizer{Model: a.Model, ModelID: a.Primary.Model, Session: sess, Threshold: cfg.Compaction.Threshold, Keep: cfg.Compaction.Keep, Instructions: cfg.Compaction.Instructions}
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
		a.Saver.Flush(a.Session)
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

// Makes sess the live session, rebuilding facts, missions, their tools and the meter from its records
func (a *App) adopt(sess *session.Session) error {
	a.Saver.Flush(a.Session)
	a.Session = sess
	a.Facts = facts.New()
	a.Missions = mission.New()
	if err := a.Facts.Restore(sess); err != nil {
		return err
	}
	if err := a.Missions.Restore(sess); err != nil {
		return err
	}
	a.Facts.Attach(sess)
	a.Missions.Attach(sess)
	a.Facts.OnChange(func(c facts.Change) { a.send(FactMsg{Change: c}) })
	a.Missions.OnChange(func(c mission.Change) { a.send(MissionMsg{Mission: c.Mission, Op: c.Op}) })
	a.Control.Missions = a.Missions
	a.Meter.Reset()
	a.Tools.Remove(facts.ToolName)
	a.Tools.Remove(mission.ToolName)
	return a.Tools.Add(facts.Tool{Store: a.Facts}, mission.Tool{Store: a.Missions})
}

// Switches the session to a stored one, restoring its facts and missions
func (a *App) Switch(ctx context.Context, id string) error {
	if a.Control.Busy() {
		return errors.New("harness: a run is in progress")
	}
	sess, err := a.Store.Load(ctx, id)
	if err != nil {
		return err
	}
	return a.adopt(sess)
}

// Starts a new empty session, keeping the stores of the old one on disk
func (a *App) Fresh() error {
	if a.Control.Busy() {
		return errors.New("harness: a run is in progress")
	}
	sess := session.New()
	sess.SetModel(a.Primary.Model)
	return a.adopt(sess)
}

// Forks the current session at an entry count and switches to the fork with the facts and missions it kept
func (a *App) ForkAt(n int) (*session.Session, error) {
	if a.Control.Busy() {
		return nil, errors.New("harness: a run is in progress")
	}
	f := a.Session.Fork()
	f.Truncate(n)
	if err := a.adopt(f); err != nil {
		return nil, err
	}
	a.Saver.Flush(f)
	return f, nil
}

// Changes the model every later agent uses, looking the catalog up again
func (a *App) SetModel(id string) {
	a.Primary.Model = id
	if known, ok := profiles.Describe(id); ok {
		a.Primary.Catalog = known
	} else {
		a.Primary.Catalog = profiles.Model{ID: id, Vendor: a.Primary.Profile.Vendor}
	}
	a.Primary.Catalog.ID = id
	a.Meter.SetCatalog(a.Primary.Catalog, a.Primary.Profile.Name)
	a.Session.SetModel(id)
	a.Config.Model = id
}

// Switches the primary endpoint to another profile, keeping fallbacks, the session and the tools
func (a *App) SetProfile(ctx context.Context, name, model string) error {
	if a.Control.Busy() {
		return errors.New("harness: a run is in progress")
	}
	ep, err := a.open(ctx, Target{Profile: name, Model: model})
	if err != nil {
		return err
	}
	a.Primary = ep
	a.Model = a.chain(ep)
	if a.Config.Subagents.Model == nil {
		a.children = ep
	}
	a.Meter.SetCatalog(ep.Catalog, ep.Profile.Name)
	a.Session.SetModel(ep.Model)
	a.Config.Profile, a.Config.Model = name, ep.Model
	return nil
}
