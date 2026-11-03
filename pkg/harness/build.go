package harness

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/plugin"
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

// Name of the plan tool, whose calls the task hooks watch
const planToolName = mission.ToolName

// Mission id the context carries
func missionIDFrom(ctx context.Context) string { return mission.IDFrom(ctx) }

// Plan kept on a session
func currentSessionPlan(sess *session.Session) (mission.SessionPlan, bool) {
	return mission.CurrentSessionPlan(sess)
}

// Configuration directory of the user
func configDirOf() (string, error) { return profiles.ConfigDir() }

func sortStrings(list []string) { sort.Strings(list) }

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
	expand   atomic.Bool
}

// Reports whether the thinking of its runs opens in the transcript as it arrives
func (l *Live) ExpandsThinking() bool { return l.expand.Load() }

// Opens the thinking of its runs in the transcript as it arrives, or leaves each block folded
func (l *Live) SetExpandThinking(on bool) { l.expand.Store(on) }

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
	Questions *Questioner
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
	kindMu   sync.RWMutex
	send     Sender
	lives    []*Live
	mu       sync.Mutex
	children *Endpoint
	summary  *Endpoint
	closed   sync.Once
	// Profiles whose endpoint refuses every call until the person acts, by name
	stuck   map[string]Stuck
	stuckMu sync.Mutex
	// First model each profile naming none listed, by profile name
	listed   map[string]string
	listedMu sync.Mutex
	// Plugins, skills, agents, styles and instructions with the state kept for them
	lib library
	// Directories added beside the project
	Added []string
	// Files and configuration watched for change events
	watch  *watcher
	pollMu sync.Mutex
	// Hook state shared by the agents of each session
	states  map[string]*agent.HookState
	stateMu sync.Mutex
	// Why the sessions end at close, as SessionEnd hooks hear it, other when empty
	EndReason string
	// Shell job ids of the plugin monitors started, by plugin id and monitor name
	monitors  map[string]string
	monitorMu sync.Mutex
	// Environment file the running session hooks may write, and the variables naming it
	envFile  string
	envMu    sync.Mutex
	extraEnv []string
}

// Profile whose endpoint refuses every call until the person acts, with the refusal and when it was first heard
type Stuck struct {
	Profile string
	Err     *protocol.Error
	Since   time.Time
}

// Why the profile is stuck and since when, for the person
func (s Stuck) Text() string {
	return fmt.Sprintf("%s %s since %s: %s", s.Profile, s.Err.Why(), s.Since.Local().Format("15:04"), s.Err.Message)
}

// Profiles stuck right now, in name order
func (a *App) Stuck() []Stuck {
	a.stuckMu.Lock()
	defer a.stuckMu.Unlock()
	out := make([]Stuck, 0, len(a.stuck))
	for _, s := range a.stuck {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// Stuck state of a profile, false while its endpoint answers
func (a *App) StuckOf(profile string) (Stuck, bool) {
	a.stuckMu.Lock()
	defer a.stuckMu.Unlock()
	s, ok := a.stuck[profile]
	return s, ok
}

// Marks the profile of a run stuck when it ended on a refusal of the account, and clears it when a run passed
func (a *App) heard(r *Run) {
	profile := a.endpointOf(r.Session).Profile.Name
	if profile == "" {
		return
	}
	err := r.Err()
	a.stuckMu.Lock()
	defer a.stuckMu.Unlock()
	if e := protocol.StuckError(err); e != nil {
		if _, already := a.stuck[profile]; already {
			return
		}
		if a.stuck == nil {
			a.stuck = map[string]Stuck{}
		}
		s := Stuck{Profile: profile, Err: e, Since: time.Now()}
		a.stuck[profile] = s
		a.send(NoticeMsg{Level: Error, Text: s.Text()})
		return
	}
	if err == nil {
		if _, was := a.stuck[profile]; was {
			delete(a.stuck, profile)
			a.send(NoticeMsg{Level: Success, Text: profile + " answers again"})
		}
	}
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
	app.Questions = NewQuestioner(send)
	name := cfg.Profile
	if name == "" {
		name = fileDefault
	}
	if name != "" {
		if app.Primary, err = app.open(ctx, Target{Profile: name, Model: cfg.Model}); err != nil {
			return nil, err
		}
	}
	app.track(app.Primary)
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
	app.watch = newWatcher()
	for _, d := range cfg.AddDirs {
		if abs, err := filepath.Abs(d); err == nil {
			app.lib.added = append(app.lib.added, abs)
		}
	}
	app.Added = app.AddedDirs()
	if err := app.buildTools(dir); err != nil {
		return nil, err
	}
	if err := app.loadLibrary(); err != nil {
		return nil, err
	}
	app.applyBins(app.Library())
	if err := app.buildPolicy(root); err != nil {
		return nil, err
	}
	app.Control = NewController(send, app.Pump)
	app.Control.Saver, app.Control.Meter, app.Control.Missions, app.Control.Subagents = app.Saver, app.Meter, app.Missions, app.Subagents
	app.Control.OnEnd = app.heard
	app.Control.MissionsOf = func(sess *session.Session) *mission.Store {
		if l := app.LiveOf(sess); l != nil {
			return l.Missions
		}
		return nil
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
	app.MCP.Options.Elicit = app.elicit
	for _, s := range cfg.MCP {
		if err := app.MCP.Add(ctx, s); err != nil {
			return nil, err
		}
	}
	app.connectPluginServers(ctx, app.Library())
	app.watch.cwd(app.Shell.Dir())
	app.watch.changedConfig(app.configFiles())
	app.watch.changedSkills(app.skillDirs())
	if app.lib.store != nil {
		app.lib.store.Sweep()
	}
	return app, nil
}

// Opens the profile and looks the model up, the app default model standing in on the default profile
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
	if model == "" && prof.Name == a.Primary.Profile.Name {
		model = a.Primary.Model
	}
	if model == "" {
		if model, err = a.firstModel(ctx, prof, prov); err != nil {
			return Endpoint{}, err
		}
	}
	return Endpoint{Profile: prof.Resolved(), Provider: prov, Model: model, Catalog: describe(model, prof), System: system}, nil
}

// Catalog entry of a model, a bare entry naming the vendor when the catalog does not know it, the profile window filling a missing one
func describe(model string, prof profiles.Profile) profiles.Model {
	known, ok := profiles.Describe(model)
	if !ok {
		known = profiles.Model{Vendor: prof.Vendor}
	}
	known.ID = model
	if known.Context == 0 {
		known.Context = prof.Window
	}
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
	return p.a.Primary.Provider.Complete(labelChild(ctx), req)
}

func (a *App) buildTools(dir string) error {
	cfg := a.Config
	a.Tools = &tool.Registry{}
	a.Files = files.Options{Dir: dir}
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
		fetch, err := web.Fetch(web.Options{Summarizer: primary{a}})
		if err != nil {
			return err
		}
		if err := a.Tools.Add(fetch); err != nil {
			return err
		}
	}
	a.Subagents = &agent.Subagents{MaxDepth: cfg.Subagents.MaxDepth, Parallel: cfg.Subagents.Parallel, Timeout: seconds(cfg.Subagents.Timeout), Factory: a.childWithIsolation, Kinds: a.agentKinds, Background: a.kindBackground, Deliver: a.deliverToRun}
	for _, name := range cfg.Tools.Disabled {
		a.Tools.SetEnabled(name, false)
	}
	return a.Tools.Add(facts.Tool{Resolve: a.FactsFor}, mission.Tool{Resolve: a.MissionsFor, Session: a.SessionFor}, a.Subagents, skillTool{app: a})
}

// Builds a child agent on a fresh session labeled with its parent, on the kind endpoint, the subagent one or the parent one
func (a *App) childFactory(ctx context.Context, kind, model string) (*agent.Agent, error) {
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
	if model != "" {
		var err error
		if ep, err = a.open(ctx, Target{Profile: ep.Profile.Name, Model: model}); err != nil {
			return nil, fmt.Errorf("model %s: %w", model, err)
		}
	}
	sess.Label(parentLabel, parent)
	sess.Label("agent", policy.AgentFrom(ctx))
	sess.Label(session.ProjectLabel, a.Root)
	ag, err := a.newAgent("", sess, ep)
	if err != nil {
		return nil, err
	}
	ag.Hooks = append([]agent.Hook{kindTools{kind: Kind{Name: "subagent", Deny: subagentDenied}}}, ag.Hooks...)
	a.trackOn(sess, ep)
	return ag, nil
}

// Child factory whose agents of an isolated kind work in a worktree of their own
func (a *App) childWithIsolation(ctx context.Context, kind, model string) (*agent.Agent, error) {
	ag, err := a.childFactory(ctx, kind, model)
	if err != nil {
		return nil, err
	}
	k, _ := a.KindOf(ag.Session)
	if len(k.Servers) > 0 {
		if err := a.connectKindServers(ctx, k); err != nil {
			return nil, err
		}
		ag.Hooks = append(ag.Hooks, &serversHook{app: a, kind: k})
	}
	if k.Isolation != "worktree" {
		return ag, nil
	}
	wt, err := a.createWorktree(ctx, ag.Session, kind)
	if err != nil {
		return nil, err
	}
	ag.Tools = wt.tools
	ag.Hooks = append(ag.Hooks, &worktreeHook{app: a, sess: ag.Session, wt: wt})
	return ag, nil
}

// Hook that ends the worktree of an isolated agent when its run stops, naming the kept path in its answer
type worktreeHook struct {
	agent.Hooks
	app  *App
	sess *session.Session
	wt   *worktree
}

func (h *worktreeHook) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	return append(input, protocol.Text{Text: "You work in a temporary git worktree at " + h.wt.Path + ", branched from " + h.wt.Branch + ". Every command and file path of yours runs there."}), nil
}

func (h *worktreeHook) OnStop(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
	if kept := h.app.finishWorktree(ctx, h.sess, h.wt); kept != "" {
		agent.Observe(ctx, agent.Event{Type: agent.EventNote, Text: "worktree with changes kept at " + kept})
	}
	return nil, nil
}

func (h *worktreeHook) OnFailure(ctx context.Context, err error) {
	h.app.finishWorktree(ctx, h.sess, h.wt)
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
	a.Gate = policy.Gate{Policy: a.Mode, Approver: hookApprover{app: a}, Tools: a.Tools, OnDeny: a.permissionDenied}
	return nil
}

// Permission mode a child of a kind runs under, the parent mode winning when it bypasses, accepts edits or plans
func childMode(parent policy.Mode, kind Kind) policy.Mode {
	own, err := policy.ParseMode(kind.Mode)
	if kind.Mode == "" || err != nil {
		return parent
	}
	switch parent {
	case policy.ModeBypass, policy.ModeAcceptEdits:
		return parent
	case policy.ModePlan:
		return policy.ModePlan
	}
	if own == policy.ModeBypass {
		return parent
	}
	return own
}

// Gate of an agent of a kind, the shared one unless the kind names a mode of its own
func (a *App) gateFor(kind Kind) policy.Gate {
	if kind.Mode == "" {
		return a.Gate
	}
	mode := childMode(a.Mode.Mode(), kind)
	if mode == a.Mode.Mode() {
		return a.Gate
	}
	sw := policy.Compose(mode, a.Rules, a.Tools)
	g := a.Gate
	g.Policy = sw
	return g
}

// Builds an agent on the session with every configured hook, the endpoint of the session and the shared gate
func (a *App) NewAgent(name string, sess *session.Session) (*agent.Agent, error) {
	return a.newAgent(name, sess, a.endpointOf(sess))
}

// Builds an agent on the session that runs on the endpoint, as the kind of the session when it has one
func (a *App) newAgent(name string, sess *session.Session, ep Endpoint) (*agent.Agent, error) {
	return a.buildAgent(name, sess, ep, buildOptions{})
}

// Choices of an agent build beside the configuration
type buildOptions struct {
	// Leaves the command hooks out, for agents that serve hooks
	noHooks bool
}

func (a *App) buildAgent(name string, sess *session.Session, ep Endpoint, opts buildOptions) (*agent.Agent, error) {
	if ep.Provider == nil {
		return nil, ErrNoProfile
	}
	if ep.Model == "" {
		return nil, fmt.Errorf("harness: profile %s has no model, pick one in config", ep.Profile.Name)
	}
	cfg := a.Config
	kind, _ := a.KindOf(sess)
	ag := &agent.Agent{Name: name, Model: labeled{named{Model: ep.Provider, profile: ep.Profile.Name}}, ModelID: ep.Model, Tools: a.Tools, Gate: a.gateFor(kind), Session: sess, MaxTokens: cfg.MaxTokens, Temperature: cfg.Temperature, Extra: cfg.Extra, MaxRounds: cfg.MaxRounds}
	ag.Effort = kind.Effort
	if ag.MaxTokens == 0 && protocol.RequiresMaxTokens(ep.Provider.Protocol) {
		ag.MaxTokens = ep.Catalog.MaxOutput
	}
	if kind.MaxRounds > 0 {
		ag.MaxRounds = kind.MaxRounds
	}
	if kind.MaxTokens > 0 {
		ag.MaxTokens = kind.MaxTokens
	}
	ag.Hooks = append(ag.Hooks, &systemHook{app: a, ep: ep, kind: kind})
	ag.Hooks = append(ag.Hooks, activationHook{app: a, sess: sess, ep: ep})
	var runner *agent.HookRunner
	if !opts.noHooks {
		r := a.hookRunner(name, sess, ep, kind, a.stateOf(sess))
		r.Host = a.hookHost(sess, ep, func() string { return name })
		r.Env = a.hookEnv()
		runner = &r
		ag.Hooks = append(ag.Hooks, r, watchHook{app: a, sess: sess}, &taskHook{app: a, sess: sess})
	}
	if kind.Name != "" {
		kt := kindTools{kind: kind}
		if len(kind.Grants) > 0 {
			grants, err := policy.NewRules(policy.RuleSet{Allow: kind.Grants})
			if err != nil {
				return nil, fmt.Errorf("kind %s: %w", kind.Name, err)
			}
			grants.Default, grants.Dir, grants.DirFunc = policy.Deny, a.Root, a.Shell.Dir
			kt.grants = grants
		}
		ag.Hooks = append(ag.Hooks, kt)
	}
	if kind.Memory != "" {
		for _, name := range []string{"Read", "Write", "Edit"} {
			if !kind.Offers(name) {
				kind.Tools = append(kind.Tools, name)
			}
		}
	}
	info := AgentInfo{Name: name, Session: sess, Endpoint: ep, Kind: kind, App: a}
	for _, build := range hooksRegistered() {
		if h := build(info); h != nil {
			ag.Hooks = append(ag.Hooks, h)
		}
	}
	ag.Hooks = append(ag.Hooks, kind.Hooks...)
	threshold := cfg.CompactThreshold(ep.Catalog.Context)
	if kind.CompactWindow > 0 {
		threshold = kind.CompactWindow
	}
	if threshold > 0 {
		s := &agent.Summarizer{Model: labeled{ep.Provider}, ModelID: ep.Model, Session: sess, Threshold: threshold, Keep: cfg.Compaction.Keep, Instructions: cfg.Compaction.Instructions}
		if a.summary != nil {
			s.Model, s.ModelID = labeled{a.summary.Provider}, a.summary.Model
		}
		if runner != nil {
			s.Before = func(ctx context.Context, trigger string) error {
				v, err := runner.Fire(ctx, agent.PreCompact, trigger, map[string]any{"trigger": trigger, "custom_instructions": cfg.Compaction.Instructions})
				if err != nil {
					return err
				}
				if v.Block {
					return &agent.HookBlocked{HookEvent: agent.PreCompact, Reason: v.Reason}
				}
				return nil
			}
			s.After = func(ctx context.Context, trigger string) error {
				a.forgetLoaded(sess)
				runner.Fire(ctx, agent.PostCompact, trigger, map[string]any{"trigger": trigger})
				for _, in := range plugin.Always(a.Instructions()) {
					in.Reason = plugin.LoadCompact
					a.instructionsLoaded(ctx, sess, in, "")
				}
				_, err := runner.Fire(ctx, agent.SessionStart, "compact", map[string]any{"source": "compact"})
				return err
			}
		}
		ag.Hooks = append(ag.Hooks, s)
	}
	after := cfg.PruneAfter()
	if after > 0 && kind.PruneAfter > 0 {
		after = kind.PruneAfter
	}
	if after > 0 {
		ag.Hooks = append(ag.Hooks, &agent.Pruner{Session: sess, After: after, Window: ep.Catalog.Context})
	}
	ag.Hooks = append(ag.Hooks, &agent.Dedup{Session: sess, Tools: ag.Tools})
	// Last so it strips and digests the request exactly as every other hook leaves it
	ag.Hooks = append(ag.Hooks, &agent.StripThinking{})
	return ag, nil
}

// First model the endpoint lists for a profile naming none, remembered until the profile is edited
func (a *App) firstModel(ctx context.Context, prof profiles.Profile, prov *provider.Provider) (string, error) {
	a.listedMu.Lock()
	model, ok := a.listed[prof.Name]
	a.listedMu.Unlock()
	if ok {
		return model, nil
	}
	list, err := prof.ListModels(ctx, prov)
	if err != nil {
		return "", fmt.Errorf("profile %s names no model and listing them failed: %w", prof.Name, err)
	}
	if len(list) == 0 {
		return "", fmt.Errorf("profile %s names no model and the endpoint lists none", prof.Name)
	}
	a.listedMu.Lock()
	if a.listed == nil {
		a.listed = map[string]string{}
	}
	a.listed[prof.Name] = list[0].ID
	a.listedMu.Unlock()
	return list[0].ID, nil
}

// Forgets the listed model of the named profiles, so an edited endpoint is listed again
func (a *App) forgetListed(names ...string) {
	a.listedMu.Lock()
	for _, name := range names {
		delete(a.listed, name)
	}
	a.listedMu.Unlock()
}

// Model whose failures name the profile they came from, so a transcript says which endpoint refused
type named struct {
	agent.Model
	profile string
}

func (n named) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		for ev, err := range n.Model.Stream(ctx, req) {
			if err != nil {
				yield(ev, fmt.Errorf("profile %s: %w", n.profile, err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Model whose requests a sub-agent sends carry its child run id, so the meter charges them to the child session
type labeled struct{ agent.Model }

func (l labeled) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return l.Model.Stream(labelChild(ctx), req)
}

// Context labeled with the child run id when a sub-agent sends from it, unchanged for the root agent
func labelChild(ctx context.Context) context.Context {
	run, name := RunFrom(ctx), policy.AgentFrom(ctx)
	if run == "" || name == "" || agent.LineageFrom(ctx).Depth == 0 {
		return ctx
	}
	return provider.WithLabel(ctx, childRun(run, name))
}

// Context whose requests and agent events the meter charges to the session, for work on it outside any run
func (a *App) Metered(ctx context.Context, sess *session.Session) context.Context {
	label := "session:" + sess.ID()
	a.Meter.Bind(label, sess.ID())
	ctx = provider.WithLabel(ctx, label)
	return agent.WithObserver(ctx, func(ev agent.Event) { a.Meter.Apply(label, ev) })
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

// Posts a message into the program for an extension that mirrors the live sessions
func (a *App) Announce(msg any) { a.send(msg) }

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

// Stops runs, closes servers and the shell, saves every open session and fires the SessionEnd hooks
func (a *App) Close(ctx context.Context) error {
	var err error
	a.closed.Do(func() {
		a.stopMonitors(ctx)
		a.Flows.Shutdown()
		a.Control.Shutdown()
		a.Approver.Close()
		a.Questions.Close()
		a.MCP.Close()
		a.Shell.Close()
		for _, l := range a.Lives() {
			a.Saver.Flush(l.Session)
		}
		a.Saver.Wait()
		a.Store.Close()
		reason := a.EndReason
		if reason == "" {
			reason = "other"
		}
		for _, l := range a.Lives() {
			if e := a.SessionEnded(ctx, l.Session, reason); e != nil && err == nil {
				err = e
			}
		}
	})
	return err
}

// Extra environment hooks run with, the environment file while one is open
func (a *App) hookEnv() []string {
	a.envMu.Lock()
	defer a.envMu.Unlock()
	return append([]string(nil), a.extraEnv...)
}

// Points the meter totals and the selected session at the endpoint, its window and the compaction threshold it gives
func (a *App) track(ep Endpoint) {
	a.Meter.SetCatalog(ep.Catalog, ep.Profile.Name)
	a.Meter.SetThreshold(a.Config.CompactThreshold(ep.Catalog.Context))
	if a.Session != nil {
		a.trackOn(a.Session, ep)
	}
}

// Points the meter at the endpoint one session runs on, its window and the compaction threshold that window gives
func (a *App) trackOn(sess *session.Session, ep Endpoint) {
	a.Meter.Track(sess.ID(), ep.Catalog, ep.Profile.Name, a.Config.CompactThreshold(ep.Catalog.Context))
}

// Points the meter at the endpoint of every open session and the selected one again, after the threshold changed
func (a *App) TrackAll() {
	for _, l := range a.Lives() {
		a.trackOn(l.Session, l.Endpoint)
	}
	a.track(a.endpointOf(a.Session))
}

// Hook that writes the system prompt built once per run into every turn, keeping an empty one while bare
type systemHook struct {
	agent.Hooks
	app    *App
	ep     Endpoint
	kind   Kind
	system string
}

func (h *systemHook) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	system, err := h.app.PromptFor(ctx, h.ep, h.kind)
	if err != nil {
		return nil, err
	}
	h.system = system
	return input, nil
}

func (h *systemHook) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	req.System = h.system
	req.KeepSystem = h.ep.Profile.Bare
	return nil
}

// Wraps a session in stores restored from its records, binds it to the endpoint and registers it, holding its lease so no
// other process works on it at the same time, and claiming it for this project when no project started it
func (a *App) live(sess *session.Session, ep Endpoint) (*Live, error) {
	if !ep.Profile.Ghost {
		if err := a.Store.Lease(sess.ID()); err != nil {
			return nil, err
		}
	}
	l := &Live{Session: sess, Facts: facts.New(), Missions: mission.New(), Endpoint: ep}
	l.SetExpandThinking(ep.Profile.ExpandsThinking())
	if err := l.Facts.Restore(sess); err != nil {
		a.Store.Unlease(sess.ID())
		return nil, err
	}
	if err := l.Missions.Restore(sess); err != nil {
		a.Store.Unlease(sess.ID())
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
	if sess.Project() == "" {
		sess.Label(session.ProjectLabel, a.Root)
	}
	a.trackOn(sess, ep)
	a.Meter.seed(sess.ID(), sess.Context())
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

// Endpoint of a profile by name with the model, the default for an empty or unknown name and the default with its
// model swapped for the name of the default profile
func (a *App) EndpointNamed(ctx context.Context, profile, model string) (Endpoint, error) {
	if profile == "" {
		return a.Primary, nil
	}
	if profile == a.Primary.Profile.Name {
		return withModel(a.Primary, model), nil
	}
	if _, ok := a.FindProfile(profile); !ok {
		return a.Primary, nil
	}
	return a.open(ctx, Target{Profile: profile, Model: model})
}

// Endpoint with the model swapped for another the same profile serves, looked up in the catalog
func withModel(ep Endpoint, id string) Endpoint {
	if id == "" {
		return ep
	}
	ep.Model = id
	ep.Catalog = describe(id, ep.Profile)
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

// Id of the newest stored session of the project that no other icebath has open, sub-agent sessions left out
func NewestSession(ctx context.Context, store *session.Store, root string) (string, error) {
	infos, err := store.ListProject(ctx, root)
	if err != nil {
		return "", err
	}
	for _, in := range infos {
		if !in.Busy && in.Labels[parentLabel] == "" {
			return in.ID, nil
		}
	}
	return "", fmt.Errorf("no session of %s to continue", root)
}

// Opens a fresh session on the default endpoint beside the ones already open, as the agent a plugin asks for when one does
func (a *App) Create() (*Live, error) {
	sess := session.New()
	if lib := a.Library(); lib != nil && lib.MainAgent != "" {
		if _, ok := a.Kind(lib.MainAgent); ok {
			sess.Label(kindLabel, lib.MainAgent)
		}
	}
	return a.live(sess, a.Primary)
}

// Opens a fork of an open session cut at n entries on the same endpoint and writes it to the store
func (a *App) Fork(l *Live, n int) (*Live, error) {
	if a.Busy(l) {
		return nil, errors.New("harness: an agent is still working")
	}
	return a.fork(l.Session, l.Endpoint, n)
}

// Opens a full fork of a stored session, the open one when it is open here, without taking the source from another process
func (a *App) ForkStored(ctx context.Context, id string) (*Live, error) {
	if l := a.LiveByID(id); l != nil {
		return a.Fork(l, l.Session.Len())
	}
	sess, err := a.Store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	ep, err := a.endpointFor(ctx, sess)
	if err != nil {
		return nil, err
	}
	return a.fork(sess, ep, sess.Len())
}

// Opens a copy of the session cut at n entries as a session of this project and writes it to the store
func (a *App) fork(sess *session.Session, ep Endpoint, n int) (*Live, error) {
	f := sess.Fork()
	f.Truncate(n)
	f.Label(session.ProjectLabel, a.Root)
	nl, err := a.live(f, ep)
	if err != nil {
		return nil, err
	}
	a.Saver.Flush(f)
	return nl, nil
}

// Changes the model of an open session, moving the default along when the session is on the default profile
func (a *App) SetModelOn(l *Live, id string) {
	a.SwitchModelOn(context.Background(), l, id)
}

// Changes the model of an open session after its PreModelSwitch hooks allow, PostModelSwitch hooks hearing of it
func (a *App) SwitchModelOn(ctx context.Context, l *Live, id string) error {
	from := l.Endpoint.Model
	if from == id {
		return nil
	}
	if err := a.modelSwitch(ctx, l.Session, from, id); err != nil {
		return err
	}
	l.Endpoint = withModel(l.Endpoint, id)
	l.Session.SetModel(id)
	a.trackOn(l.Session, l.Endpoint)
	if l.Endpoint.Profile.Name == a.Primary.Profile.Name {
		a.SetDefaultModel(id)
	}
	if l.Session == a.Session {
		a.track(l.Endpoint)
	}
	a.modelSwitched(ctx, l.Session, from, id)
	return nil
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
	from := l.Endpoint.Model
	if from != ep.Model {
		if err := a.modelSwitch(ctx, l.Session, from, ep.Model); err != nil {
			return err
		}
	}
	if was && !ep.Profile.Ghost {
		if err := a.Store.Lease(l.Session.ID()); err != nil {
			return err
		}
	}
	l.Endpoint = ep
	l.SetExpandThinking(ep.Profile.ExpandsThinking())
	l.Session.SetModel(ep.Model)
	l.Session.Label(profileLabel, ep.Profile.Name)
	a.trackOn(l.Session, ep)
	a.SetDefaultEndpoint(name, ep)
	if l.Session == a.Session {
		a.track(ep)
	}
	if from != ep.Model {
		a.modelSwitched(ctx, l.Session, from, ep.Model)
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
	a.Store.Unlease(s.ID())
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

// Moves new sessions to an opened endpoint, and every open session of that profile left without a model
func (a *App) SetDefaultEndpoint(name string, ep Endpoint) {
	a.Primary = ep
	a.Config.Profile, a.Config.Model = name, ep.Model
	for _, l := range a.Lives() {
		if l.Endpoint.Profile.Name == name && l.Endpoint.Model == "" {
			l.Endpoint = ep
			l.Session.SetModel(ep.Model)
			a.trackOn(l.Session, ep)
			a.Saver.Touch(l.Session)
		}
	}
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

// Writes an open session that holds anything, gives its lease back and forgets it, selecting another when it was the selected one
func (a *App) Release(l *Live) error {
	if a.Busy(l) {
		return errors.New("harness: an agent is still working")
	}
	a.Saver.Flush(l.Session)
	a.Store.Unlease(l.Session.ID())
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
	a.track(l.Endpoint)
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

// Session of the calling run, the selected one outside a run
func (a *App) SessionFor(ctx context.Context) *session.Session {
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		return l.Session
	}
	return a.Session
}

// Mission store the calling run plans into, the selected one outside a run
func (a *App) MissionsFor(ctx context.Context) *mission.Store {
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		return l.Missions
	}
	return a.Missions
}
