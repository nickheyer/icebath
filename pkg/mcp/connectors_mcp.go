package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One wait for every authorization URL of a call, the flows start together
var connPrepareWait = 30 * time.Second

const connectorNote = "Settled once; do not re-ask on your own for any target the user skipped or that timed out, but " +
	"a later request from the USER for that same app is not a re-ask — run it. Connected " +
	"targets' tools are available now through tool_describe/tool_call and are named under " +
	"tools_listing. A target with discovery_error is authorized but its tools are unavailable; " +
	"retry discovery with manage_connections using that target's authorize or install action " +
	"without asking for consent again."

const connectorNoCardNote = "No connection card is drawn for this turn. Show any connect_url to the user so they " +
	"open it in a browser, then ask them to say when they are done. Connected targets' tools are " +
	"available now through tool_describe/tool_call and are named under tools_listing. A target with " +
	"discovery_error is authorized but its tools are unavailable; retry discovery with " +
	"manage_connections using that target's authorize or install action without asking for consent " +
	"again. Do not re-ask on your own for skipped or timed-out targets, but a later request from " +
	"the USER for that same app is not a re-ask — run it."

const authorizationKept = "the authorization had already completed when this was canceled, so it was " +
	"kept; run the same action again to list the tools"

func init() {
	tools.MCPOperationRunner = RunMCPOperation
	tools.MCPCatalogNames = connCatalogNamesList
}

// Sorted catalog entry names
func connCatalogNamesList() ([]string, error) {
	entries, err := ListCatalog()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	sort.Strings(out)
	return out, nil
}

// Catalog and configured name readers, swapped by tests
var (
	connCatalogNames    = func(ctx context.Context) ([]string, error) { return connCatalogNamesList() }
	connConfiguredNames = func(ctx context.Context) ([]string, error) {
		servers, err := getMCPServers(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(servers))
		for k := range servers {
			out = append(out, k)
		}
		sort.Strings(out)
		return out, nil
	}
)

// The reason names do not fit action, install taking catalog entries and the rest configured servers
func ValidateMCPNames(ctx context.Context, env *tools.Env, action string, names []string) string {
	catalog, err := connCatalogNames(ctx)
	var configured []string
	if err == nil {
		configured, err = connConfiguredNames(ctx)
	}
	if err != nil {
		return fmt.Sprintf("could not read the MCP catalog: %s", excStr(err))
	}
	allowed := configured
	if action == "install" {
		allowed = catalog
	}
	var unknown, foreign []string
	for _, n := range names {
		if !slices.Contains(allowed, n) {
			unknown = append(unknown, n)
			if !slices.Contains(catalog, n) && !slices.Contains(configured, n) {
				foreign = append(foreign, n)
			}
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	if len(foreign) > 0 {
		hosted, _ := tools.HostedNames(ctx, env)
		var parts []string
		for _, n := range foreign {
			if hosted[n] {
				parts = append(parts, tools.MisroutedToMCPError(action, n))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if action == "install" {
		out := fmt.Sprintf("unknown MCP server(s) for install: %s. Install works for catalog entries only: %s.",
			strings.Join(unknown, ", "), orText(strings.Join(catalog, ", "), "(empty catalog)"))
		if len(configured) > 0 {
			out += fmt.Sprintf(" Already configured (use enable/authorize): %s.", strings.Join(configured, ", "))
		}
		return out
	}
	out := fmt.Sprintf("unknown MCP server(s) for %s: %s. %s works for servers already in mcp_servers: %s.",
		action, strings.Join(unknown, ", "), action, orText(strings.Join(configured, ", "), "(none configured)"))
	if len(catalog) > 0 {
		out += fmt.Sprintf(" Catalog entries you can install: %s.", strings.Join(catalog, ", "))
	}
	return out
}

func orText(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// The catalog entry or a config_invalid error naming the miss
func connCatalogEntry(name string) (*CatalogEntry, error) {
	entry, err := GetEntry(name)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &ProbeError{Err: fmt.Errorf("no catalog entry '%s'", name), Class: "config_invalid"}
	}
	return entry, nil
}

// ConnectorBackend is the work behind an MCP target, the catalog, the installer and the OAuth flow
type ConnectorBackend interface {
	RequiredEnv(ctx context.Context, name string) ([]*pyjson.Object, error)
	StartOAuth(ctx context.Context, name string) (ConnectorAttempt, error)
	InstallsWithOAuth(ctx context.Context, name string) (bool, error)
	StartInstallOAuth(ctx context.Context, name string, env *pyjson.Object) (ConnectorAttempt, error)
	Install(ctx context.Context, name string, env *pyjson.Object) ([]string, error)
	Enable(ctx context.Context, name string) error
}

// ConnectorAttempt is one OAuth flow in flight as the watcher reads it
type ConnectorAttempt interface {
	AuthURL() string
	Detail() string
	Poll() AttemptPoll
	Flow() *DashboardOAuthFlow
}

// AttemptPoll is an attempt's status with its tools and discovery error once approved
type AttemptPoll struct {
	Status         string
	Error          string
	Tools          []string
	DiscoveryError string
}

type catalogBackend struct{ env *tools.Env }

// The declared credentials of the entry that have no value yet
func (b catalogBackend) RequiredEnv(ctx context.Context, name string) ([]*pyjson.Object, error) {
	entry, err := connCatalogEntry(name)
	if err != nil {
		return nil, err
	}
	out := []*pyjson.Object{}
	for _, spec := range entry.Auth.Env {
		if v, _, _ := config.GetEnvValue(ctx, home.Dir(ctx), spec.Name); v != "" {
			continue
		}
		o := pyjson.NewObject()
		o.Set("name", spec.Name)
		o.Set("prompt", spec.Prompt)
		o.Set("required", spec.Required)
		o.Set("secret", spec.Secret)
		if spec.Secret {
			o.Set("default", "")
		} else {
			o.Set("default", spec.Default)
		}
		out = append(out, o)
	}
	return out, nil
}

func (b catalogBackend) StartOAuth(ctx context.Context, name string) (ConnectorAttempt, error) {
	return StartCardOAuth(ctx, b.env, name, CardOAuthOptions{})
}

// Whether the card must run the entry's own OAuth, provider mediated OAuth being a plain probe
func (b catalogBackend) InstallsWithOAuth(ctx context.Context, name string) (bool, error) {
	entry, err := connCatalogEntry(name)
	if err != nil {
		return false, err
	}
	return entry.Auth.Type == "oauth" && !connTruthy(entry.Auth.Provider), nil
}

// Installs an OAuth entry through the card flow, the config and setup values landing when the token is accepted
func (b catalogBackend) StartInstallOAuth(ctx context.Context, name string, env *pyjson.Object) (ConnectorAttempt, error) {
	installed, err := IsInstalled(ctx, name)
	if err != nil {
		return nil, err
	}
	fresh := !installed
	commit := func(cctx context.Context) error {
		if err := connSaveEnv(cctx, env, nil); err != nil {
			return err
		}
		if fresh {
			RecordMCPInstall(cctx, "catalog", name, "success", "", nil)
		}
		return nil
	}
	attempt, err := func() (ConnectorAttempt, error) {
		entry, err := connCatalogEntry(name)
		if err != nil {
			return nil, err
		}
		if err := connCheckDeclared(name, entry, env); err != nil {
			return nil, err
		}
		cfg, err := CardInstallConfig(ctx, headlessUI{}, entry)
		if err != nil {
			return nil, err
		}
		return StartCardOAuth(ctx, b.env, name, CardOAuthOptions{Cfg: cfg, Env: env, OnCommit: commit})
	}()
	if err != nil && fresh {
		RecordMCPInstall(ctx, "catalog", name, "failed", "", err)
	}
	return attempt, err
}

// Probes the in memory config with ephemeral credentials and saves both only after the server answered
func (b catalogBackend) Install(ctx context.Context, name string, env *pyjson.Object) ([]string, error) {
	var out []string
	err := recordedCatalogInstall(ctx, name, func() error {
		var err error
		out, err = b.install(ctx, name, env)
		return err
	})
	return out, err
}

func (b catalogBackend) install(ctx context.Context, name string, env *pyjson.Object) ([]string, error) {
	entry, err := connCatalogEntry(name)
	if err != nil {
		return nil, err
	}
	if err := connCheckDeclared(name, entry, env); err != nil {
		return nil, err
	}
	cfg, err := CardInstallConfig(ctx, headlessUI{}, entry)
	if err != nil {
		return nil, err
	}
	secret := map[string]bool{}
	for _, spec := range entry.Auth.Env {
		if spec.Secret {
			secret[spec.Name] = true
		}
	}
	for _, k := range env.Keys {
		if v, _ := env.Vals[k].(string); !secret[k] && v != "" {
			cfg, _ = inlineNonSecretValue(cfg, k, v).(map[string]any)
		}
	}
	merged := map[string]string{}
	for k, v := range provider.CurrentSecretScope(ctx) {
		merged[k] = v
	}
	for _, k := range env.Keys {
		merged[k], _ = env.Vals[k].(string)
	}
	sctx := provider.WithSecretScope(ctx, merged, provider.CurrentSecretScopeHome(ctx))
	summaries, err := probeSingleServer(sctx, b.env, name, cfg)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(summaries))
	for i, s := range summaries {
		names[i] = s.Name
	}
	ok, err := SaveServer(ctx, headlessUI{}, name, cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &ProbeError{Err: fmt.Errorf("'%s' was rejected: suspicious command/args configuration", name), Class: "config_rejected"}
	}
	if err := connSaveEnv(ctx, env, secret); err != nil {
		return nil, err
	}
	return names, nil
}

// Flips enabled under the dashboard's config write scope so the two read modify write paths serialise
func (b catalogBackend) Enable(ctx context.Context, name string) error {
	wctx, release := ConfigWriteScope(ctx, "")
	defer release()
	h := home.Dir(wctx)
	doc, err := config.ReadUser(h)
	if err != nil {
		return err
	}
	servers, ok := doc["mcp_servers"].(map[string]any)
	var entry map[string]any
	if ok {
		entry, ok = servers[name].(map[string]any)
	}
	if !ok {
		return fmt.Errorf("'%s' is not a configured MCP server", name)
	}
	entry["enabled"] = true
	return config.SaveUser(h, doc)
}

var configMutationMu sync.Mutex

// ConfigWriteScope enters the profile scope then the process config mutation lock, the dashboard write nesting
var ConfigWriteScope = func(ctx context.Context, profile string) (context.Context, func()) {
	if profile != "" {
		ctx = home.WithOverride(ctx, profile)
	}
	configMutationMu.Lock()
	return ctx, configMutationMu.Unlock
}

// Refuses the whole env map before the first write when any key is undeclared or unwritable
func connCheckDeclared(name string, entry *CatalogEntry, env *pyjson.Object) error {
	declared := map[string]bool{}
	for _, spec := range entry.Auth.Env {
		declared[spec.Name] = true
	}
	for _, k := range env.Keys {
		if !declared[k] {
			return &ProbeError{Err: fmt.Errorf("'%s' does not declare the environment variable %s", name, k), Class: "config_invalid"}
		}
		if err := config.ValidateEnvNameForWrite(k); err != nil {
			return err
		}
	}
	return nil
}

// Writes each non empty value to the profile env file, only the keys in only when it is given
func connSaveEnv(ctx context.Context, env *pyjson.Object, only map[string]bool) error {
	for _, k := range env.Keys {
		v, _ := env.Vals[k].(string)
		if v == "" || (only != nil && !only[k]) {
			continue
		}
		if err := config.SaveEnvValue(home.Dir(ctx), k, v); err != nil {
			return err
		}
	}
	return nil
}

// Default backend, swapped by tests
var connDefaultBackend = func(env *tools.Env) ConnectorBackend { return catalogBackend{env: env} }

// headlessUI writes to stdout with colour only on a terminal and never prompts
type headlessUI struct{}

func (headlessUI) Out() io.Writer { return os.Stdout }
func (headlessUI) Color(text string, codes ...string) string {
	if len(codes) == 0 || !isatty.IsTerminal(os.Stdout.Fd()) {
		return text
	}
	return strings.Join(codes, "") + text + "\033[0m"
}
func (headlessUI) Prompt(question, def string, password bool) string { return def }
func (headlessUI) YesNo(question string, def bool) bool              { return def }
func (headlessUI) Checklist(title string, items []string, selected, cancelReturns map[int]bool) map[int]bool {
	return cancelReturns
}
func (headlessUI) SingleSelect(title string, items []string) (int, bool) { return 0, false }
func (headlessUI) Interactive() bool                                     { return false }

type connWork struct {
	attempt ConnectorAttempt
	done    *tools.ConnEvent
	tools   []string
	err     string
}

// connRunner holds the backend and the work for one operation's targets
type connRunner struct {
	action  string
	backend ConnectorBackend
	env     *tools.Env

	mu          sync.Mutex
	opID        string
	operation   *tools.ConnectionOperation
	work        map[string]*connWork
	approvedEnv map[string]*pyjson.Object
}

var (
	connRunnersMu sync.Mutex
	connRunners   = map[string]*connRunner{}
)

// The runner for one MCP operation, bound to it by prepare
func openRunner(env *tools.Env, action string, backend ConnectorBackend) *connRunner {
	if backend == nil {
		backend = connDefaultBackend(env)
	}
	return &connRunner{action: action, backend: backend, env: env, work: map[string]*connWork{}, approvedEnv: map[string]*pyjson.Object{}}
}

func (r *connRunner) getWork(name string) *connWork {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.work[name]
}

func (r *connRunner) setWork(name string, w *connWork) {
	r.mu.Lock()
	r.work[name] = w
	r.mu.Unlock()
}

func (r *connRunner) popWork(name string) *connWork {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.work[name]
	delete(r.work, name)
	return w
}

func (r *connRunner) approved(name string) *pyjson.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.approvedEnv[name]
}

type connStep int

const (
	stepPrepare connStep = iota
	stepApprove
	stepRetry
	stepNoCard
)

// Runs the action's step of the table on one target
func (r *connRunner) run(ctx context.Context, step connStep, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	if env == nil {
		env = pyjson.NewObject()
	}
	type fn = func(context.Context, *connRunner, *tools.ConnectionOperation, *tools.ConnTarget, *pyjson.Object)
	tables := map[connStep]map[string]fn{
		stepPrepare: {"authorize": connStartOAuth, "install": connDeclareEnv, "enable": connNothing},
		stepApprove: {"authorize": connNothing, "install": connStartInstall, "enable": connDoEnable},
		stepRetry:   {"authorize": connStartOAuth, "install": connStartInstall, "enable": connDoEnable},
		stepNoCard:  {"authorize": connStartOAuth, "install": connInstallNow, "enable": connDoEnable},
	}
	tables[step][r.action](ctx, r, op, t, env)
}

// Runs one blocking backend call on a goroutine in the caller's context, observe reports its outcome
func (r *connRunner) spawn(ctx context.Context, op *tools.ConnectionOperation, t *tools.ConnTarget, call func(context.Context) ([]string, error)) {
	if op.Settled() {
		mcpLogger().Debug(fmt.Sprintf("mcp %s %s: not started, the operation settled first", r.action, t.Name))
		return
	}
	work := &connWork{done: tools.NewConnEvent()}
	r.setWork(t.Name, work)
	wctx := context.WithoutCancel(ctx)
	go func() {
		names := []string{}
		errText := ""
		got, err := call(wctx)
		if err != nil {
			errText = connDetail(r, t, err)
		} else {
			names = append(names, got...)
		}
		if op.Settled() {
			mcpLogger().Debug(fmt.Sprintf("mcp %s %s: outcome dropped, the operation settled first", r.action, t.Name))
			return
		}
		r.mu.Lock()
		work.tools, work.err = names, errText
		r.mu.Unlock()
		work.done.Set()
		op.Wake.Set()
	}()
}

// Binds the runner to the operation and runs each target's prepare step, authorize flows together
func (r *connRunner) prepare(ctx context.Context, op *tools.ConnectionOperation) {
	connRunnersMu.Lock()
	connRunners[op.OpID] = r
	connRunnersMu.Unlock()
	r.mu.Lock()
	r.opID = op.OpID
	r.operation = op
	r.mu.Unlock()
	if r.action == "authorize" && len(op.Targets) > 1 {
		r.prepareTogether(ctx, op)
		return
	}
	for _, t := range op.Targets {
		r.run(ctx, stepPrepare, op, t, nil)
	}
}

// Starts every OAuth flow at once and waits for the URLs once, a late row left to its own goroutine
func (r *connRunner) prepareTogether(ctx context.Context, op *tools.ConnectionOperation) {
	dones := make([]chan struct{}, len(op.Targets))
	for i, t := range op.Targets {
		dones[i] = make(chan struct{})
		go func(t *tools.ConnTarget, done chan struct{}) {
			defer close(done)
			r.run(ctx, stepPrepare, op, t, nil)
		}(t, dones[i])
	}
	deadline := time.Now().Add(connPrepareWait)
	for _, done := range dones {
		timer := time.NewTimer(max(0, time.Until(deadline)))
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Advances every live target by one read
func (r *connRunner) observe(ctx context.Context, op *tools.ConnectionOperation) {
	for _, t := range op.Targets {
		state := op.StateOf(t)
		if op.Settled() || (state != tools.TargetPending && state != tools.TargetInitiated) {
			continue
		}
		switch r.action {
		case "authorize":
			connObserveOAuth(ctx, r, op, t)
		case "install":
			connObserveInstall(ctx, r, op, t)
		case "enable":
			connObserveWorker(ctx, r, op, t)
		}
	}
}

// Ends the operation, cancelling attempts on interrupt and parking the rest for the next turn
func (r *connRunner) close(ctx context.Context) {
	r.mu.Lock()
	opID, op := r.opID, r.operation
	work := r.work
	r.work = map[string]*connWork{}
	r.mu.Unlock()
	if opID != "" {
		connRunnersMu.Lock()
		delete(connRunners, opID)
		connRunnersMu.Unlock()
	}
	names := make([]string, 0, len(work))
	for name := range work {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w := work[name]
		if w.attempt == nil || op == nil {
			continue
		}
		if op.SettledBy() == tools.SettleInterrupt {
			if flow := w.attempt.Flow(); flow != nil {
				CancelAttempt(flow)
			}
			continue
		}
		key := connLateKey(ctx, op)
		lateMu.Lock()
		if lateAttempts[key] == nil {
			lateAttempts[key] = map[string]ConnectorAttempt{}
		}
		lateAttempts[key][name] = w.attempt
		lateMu.Unlock()
	}
}

type connLateKeyT struct{ profile, session string }

// The profile and session pairing live.open keys by, the calling home when the operation never opened
func connLateKey(ctx context.Context, op *tools.ConnectionOperation) connLateKeyT {
	pk := op.ProfileKey
	if pk == "" {
		pk = home.Key(home.Dir(ctx))
	}
	return connLateKeyT{pk, op.SessionKey}
}

var (
	lateMu       sync.Mutex
	lateAttempts = map[connLateKeyT]map[string]ConnectorAttempt{}
)

// LateConnectionAgent is the agent view adopt late connections reads and updates
type LateConnectionAgent interface {
	SessionID() string
	EnabledToolsets() []string
	DisabledToolsets() []string
	SetToolsets(enabled, disabled []string)
	ToolEnv() *tools.Env
}

// Server registration seam, swapped by tests
var connRegisterServers = Connect

// Registers servers whose authorization committed after their card closed and adds them to the agent toolsets
func AdoptLateConnections(ctx context.Context, agent LateConnectionAgent) []string {
	env := agent.ToolEnv()
	key := connLateKeyT{home.Key(home.Dir(ctx)), tools.OperationSessionKey(env, agent.SessionID())}
	lateMu.Lock()
	attempts := lateAttempts[key]
	pending := make([]string, 0, len(attempts))
	for name := range attempts {
		pending = append(pending, name)
	}
	lateMu.Unlock()
	if len(pending) == 0 {
		return []string{}
	}
	sort.Strings(pending)
	adopted := []string{}
	for _, name := range pending {
		lateMu.Lock()
		attempt := attempts[name]
		lateMu.Unlock()
		snap := attempt.Poll()
		if snap.Status == "pending" {
			continue
		}
		lateMu.Lock()
		delete(attempts, name)
		lateMu.Unlock()
		if snap.Status != "approved" || snap.DiscoveryError != "" {
			continue
		}
		if cfg := LoadServerConfigs(ctx, env)[name]; cfg != nil {
			if _, err := connRegisterServers(ctx, env, map[string]*Rendered{name: cfg}); err != nil {
				mcpLogger().Debug(fmt.Sprintf("late MCP connection %s was not adopted: %s", name, excStr(err)))
				continue
			}
			adopted = append(adopted, name)
		}
	}
	lateMu.Lock()
	if len(attempts) == 0 {
		delete(lateAttempts, key)
	}
	lateMu.Unlock()
	enabled := agent.EnabledToolsets()
	if len(adopted) > 0 && enabled != nil && !slices.Contains(enabled, "no_mcp") {
		next := append([]string{}, enabled...)
		for _, n := range adopted {
			if !slices.Contains(enabled, n) {
				next = append(next, n)
			}
		}
		agent.SetToolsets(next, agent.DisabledToolsets())
	}
	return adopted
}

// Message-less errors reach the user as what happened instead of their class name
var connBareErrors = map[string]string{
	"CancelledError": "tool discovery was interrupted; run the same action again to list the tools",
	"TimeoutError":   "the server did not answer in time",
}

// User facing failure text with every value the card submitted for the target replaced before the redactor runs
func connDetail(r *connRunner, t *tools.ConnTarget, cause any) string {
	text := "error"
	switch c := cause.(type) {
	case string:
		if c != "" {
			text = c
		}
	case error:
		text = c.Error()
		if text == "" {
			var te *TimeoutError
			switch {
			case errors.As(c, &te) || errors.Is(c, context.DeadlineExceeded):
				text = connBareErrors["TimeoutError"]
			case errors.Is(c, context.Canceled):
				text = connBareErrors["CancelledError"]
			default:
				text = fmt.Sprintf("%T", c)
			}
		}
	}
	if bare, ok := connBareErrors[text]; ok {
		text = bare
	}
	if approved := r.approved(t.Name); approved != nil {
		for _, k := range approved.Keys {
			if v, _ := approved.Vals[k].(string); v != "" {
				text = strings.ReplaceAll(text, v, "[REDACTED]")
			}
		}
	}
	return orText(tools.RedactSensitiveTextWith(r.env, text, tools.RedactOptions{Force: true}), "error")
}

// The manifest's post_install text for a catalog name, empty for a custom server
func connCatalogInstructions(name string) string {
	entry, err := GetEntry(name)
	if err != nil || entry == nil {
		return ""
	}
	return entry.PostInstall
}

// Moves one target, dropping the move when Continue settled the operation first
func connMove(op *tools.ConnectionOperation, t *tools.ConnTarget, to tools.TargetState, actor tools.ConnActor, f tools.TransitionFields) bool {
	if _, err := op.Transition(t.Name, to, actor, f); err != nil {
		var it *tools.IllegalTransition
		if !errors.As(err, &it) || !op.Settled() {
			panic(err)
		}
		mcpLogger().Debug(fmt.Sprintf("mcp target %s: %s dropped, the operation settled first", t.Name, to))
		return false
	}
	return true
}

// Reports a failure whatever the row was doing, a repeated failure refreshing only the text
func connFail(op *tools.ConnectionOperation, t *tools.ConnTarget, detail string) {
	if op.Settled() {
		mcpLogger().Debug(fmt.Sprintf("mcp target %s: failure dropped, the operation settled first", t.Name))
		return
	}
	if op.StateOf(t) == tools.TargetFailed {
		if err := op.Refresh(t.Name, "", detail, tools.ActorBackendWatcher, ""); err != nil {
			panic(err)
		}
		return
	}
	op.UpdateTarget(t, func(t *tools.ConnTarget) { t.ConnectURL = "" })
	connMove(op, t, tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{Detail: &detail})
}

// Registers one committed server in the current profile scope and reports its callable names, swapped by tests
var connRegisterConnected = func(ctx context.Context, r *connRunner, t *tools.ConnTarget, name string) ([]string, string) {
	cfg := LoadServerConfigs(ctx, r.env)[name]
	if cfg == nil {
		return []string{}, connDetail(r, t, fmt.Errorf("no committed MCP configuration for '%s'", name))
	}
	if _, err := connRegisterServers(ctx, r.env, map[string]*Rendered{name: cfg}); err != nil {
		return []string{}, connDetail(r, t, err)
	}
	return connRegisteredToolNames(ctx, name, 30*time.Second), ""
}

// The server's callable names once its registration finished, waking a parked server once
func connRegisteredToolNames(ctx context.Context, name string, wait time.Duration) []string {
	key := resolveServerKey(ctx, name, "", true)
	scope := mcpRegistryScope(ctx)
	deadline := time.Now().Add(wait)
	woken := false
	for {
		names := tools.Default.ToolNamesForToolset(ctx, scope, "mcp-"+name)
		if len(names) > 0 || !time.Now().Before(deadline) {
			return names
		}
		mcpMu.Lock()
		_, connecting := serverConnecting[key]
		srv := mcpServers[key]
		mcpMu.Unlock()
		if !connecting {
			if srv != nil && srv.registrationFinished() {
				return names
			}
			if woken || srv == nil || !ReconnectServer(ctx, name) {
				return names
			}
			woken = true
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func connConnect(op *tools.ConnectionOperation, t *tools.ConnTarget, names []string, discoveryError string) {
	list := make([]any, len(names))
	for i, n := range names {
		list[i] = n
	}
	extra := pyjson.NewObject()
	extra.Set("tools", list)
	if discoveryError != "" {
		extra.Set("discovery_error", discoveryError)
	}
	connMove(op, t, tools.TargetConnected, tools.ActorBackendWatcher, tools.TransitionFields{Extra: extra})
}

// Try again is the user's move and a first attempt the backend's
func connActorFor(op *tools.ConnectionOperation, t *tools.ConnTarget) tools.ConnActor {
	if op.StateOf(t) == tools.TargetFailed {
		return tools.ActorUser
	}
	return tools.ActorBackendWatcher
}

func connStartOAuth(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	actor := connActorFor(op, t)
	instructions := connCatalogInstructions(t.Name)
	op.UpdateTarget(t, func(t *tools.ConnTarget) { t.Instructions = instructions })
	attempt, err := r.backend.StartOAuth(ctx, t.Name)
	if err != nil {
		connFail(op, t, connDetail(r, t, err))
		return
	}
	r.setWork(t.Name, &connWork{attempt: attempt})
	url, detail := attempt.AuthURL(), attempt.Detail()
	connMove(op, t, tools.TargetInitiated, actor, tools.TransitionFields{ConnectURL: &url, Detail: &detail})
}

// The install row waits pending while the card draws a field per credential it still needs
func connDeclareEnv(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	instructions := connCatalogInstructions(t.Name)
	op.UpdateTarget(t, func(t *tools.ConnTarget) { t.Instructions = instructions })
	required, err := r.backend.RequiredEnv(ctx, t.Name)
	if err != nil {
		connFail(op, t, connDetail(r, t, err))
		return
	}
	op.UpdateTarget(t, func(t *tools.ConnTarget) { t.RequiredEnv = required })
}

// The declared required credentials that still have no value
func connMissingRequired(ctx context.Context, r *connRunner, t *tools.ConnTarget, env *pyjson.Object) ([]*pyjson.Object, error) {
	declared, err := r.backend.RequiredEnv(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	var missing []*pyjson.Object
	for _, spec := range declared {
		if !specRequired(spec) {
			continue
		}
		name, _ := spec.Get("name")
		if v, _ := env.Get(connStr(name)); connTruthy(v) {
			continue
		}
		missing = append(missing, spec)
	}
	return missing, nil
}

func specRequired(spec *pyjson.Object) bool {
	v, ok := spec.Get("required")
	return !ok || connTruthy(v)
}

func connStr(v any) string {
	if !connTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

func connTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

func connStartInstall(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	approved := pyjson.NewObject()
	if prev := r.approved(t.Name); prev != nil {
		for _, k := range prev.Keys {
			approved.Set(k, prev.Vals[k])
		}
	}
	for _, k := range env.Keys {
		approved.Set(k, env.Vals[k])
	}
	missing, err := connMissingRequired(ctx, r, t, approved)
	if err != nil {
		connFail(op, t, connDetail(r, t, err))
		return
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		for i, spec := range missing {
			n, _ := spec.Get("name")
			names[i] = pyrepr.Str(n)
		}
		var url string
		op.UpdateTarget(t, func(t *tools.ConnTarget) { t.RequiredEnv = missing; url = t.ConnectURL })
		if err := op.Refresh(t.Name, url, "waiting for "+strings.Join(names, ", "), tools.ActorBackendWatcher, ""); err != nil {
			panic(err)
		}
		return
	}
	r.mu.Lock()
	r.approvedEnv[t.Name] = approved
	r.mu.Unlock()
	actor := connActorFor(op, t)
	op.UpdateTarget(t, func(t *tools.ConnTarget) { t.RequiredEnv = nil })
	empty := ""
	if !connMove(op, t, tools.TargetInitiated, actor, tools.TransitionFields{Detail: &empty}) {
		return
	}
	if connInstallsWithOAuth(ctx, r, t) {
		connStartInstallOAuth(ctx, r, op, t, approved)
		return
	}
	r.spawn(ctx, op, t, func(wctx context.Context) ([]string, error) { return r.backend.Install(wctx, t.Name, approved) })
}

// Whether the entry installs through its own OAuth, a bad entry answering false for the install to report
func connInstallsWithOAuth(ctx context.Context, r *connRunner, t *tools.ConnTarget) bool {
	ok, err := r.backend.InstallsWithOAuth(ctx, t.Name)
	return err == nil && ok
}

// Publishes the authorization link onto the already initiated row
func connStartInstallOAuth(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	attempt, err := r.backend.StartInstallOAuth(ctx, t.Name, env)
	if err != nil {
		connFailInstall(ctx, r, op, t, err)
		return
	}
	r.setWork(t.Name, &connWork{attempt: attempt})
	if op.Settled() {
		return
	}
	if err := op.Refresh(t.Name, attempt.AuthURL(), attempt.Detail(), tools.ActorBackendWatcher, ""); err != nil {
		panic(err)
	}
}

// A failed install asks for every declared field again so the card can reopen its form
func connFailInstall(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, cause any) {
	if required, err := r.backend.RequiredEnv(ctx, t.Name); err == nil {
		op.UpdateTarget(t, func(t *tools.ConnTarget) { t.RequiredEnv = required })
	}
	connFail(op, t, connDetail(r, t, cause))
}

func connDoEnable(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	actor := connActorFor(op, t)
	empty := ""
	if !connMove(op, t, tools.TargetInitiated, actor, tools.TransitionFields{Detail: &empty}) {
		return
	}
	if err := r.backend.Enable(ctx, t.Name); err != nil {
		connFail(op, t, connDetail(r, t, err))
		return
	}
	names, discoveryError := connRegisterConnected(ctx, r, t, t.Name)
	connConnect(op, t, names, discoveryError)
}

// Installs at once off the desktop where nobody can fill a credential in
func connInstallNow(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	required, err := r.backend.RequiredEnv(ctx, t.Name)
	if err != nil {
		connFail(op, t, connDetail(r, t, err))
		return
	}
	var missing []string
	for _, spec := range required {
		if specRequired(spec) {
			n, _ := spec.Get("name")
			missing = append(missing, pyrepr.Str(n))
		}
	}
	if len(missing) > 0 {
		connFail(op, t, fmt.Sprintf("set %s in the environment or %s/.env, then install again",
			strings.Join(missing, ", "), home.Display(home.Dir(ctx))))
		return
	}
	actor := connActorFor(op, t)
	if connInstallsWithOAuth(ctx, r, t) {
		attempt, err := r.backend.StartInstallOAuth(ctx, t.Name, pyjson.NewObject())
		if err != nil {
			connFail(op, t, connDetail(r, t, err))
			return
		}
		r.setWork(t.Name, &connWork{attempt: attempt})
		url, detail := attempt.AuthURL(), attempt.Detail()
		connMove(op, t, tools.TargetInitiated, actor, tools.TransitionFields{ConnectURL: &url, Detail: &detail})
		return
	}
	connMove(op, t, tools.TargetInitiated, actor, tools.TransitionFields{})
	if _, err := r.backend.Install(ctx, t.Name, pyjson.NewObject()); err != nil {
		connFail(op, t, connDetail(r, t, err))
		return
	}
	names, discoveryError := connRegisterConnected(ctx, r, t, t.Name)
	connConnect(op, t, names, discoveryError)
}

func connObserveOAuth(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget) {
	work := r.getWork(t.Name)
	if work == nil || work.attempt == nil {
		return
	}
	snap := work.attempt.Poll()
	if snap.Status != "approved" && snap.Status != "error" {
		return
	}
	r.popWork(t.Name)
	if snap.Status == "approved" {
		if snap.DiscoveryError != "" {
			connConnect(op, t, nil, connDetail(r, t, snap.DiscoveryError))
		} else {
			names, registrationError := connRegisterConnected(ctx, r, t, t.Name)
			connConnect(op, t, names, registrationError)
		}
		return
	}
	errText := orText(snap.Error, "the authorization flow failed")
	if r.action == "install" {
		connFailInstall(ctx, r, op, t, errText)
		return
	}
	connFail(op, t, connDetail(r, t, errText))
}

// An install is an OAuth attempt for an OAuth entry and a worker for every other one
func connObserveInstall(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget) {
	if work := r.getWork(t.Name); work != nil && work.attempt != nil {
		connObserveOAuth(ctx, r, op, t)
		return
	}
	connObserveWorker(ctx, r, op, t)
}

func connObserveWorker(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget) {
	work := r.getWork(t.Name)
	if work == nil || work.done == nil || !work.done.IsSet() {
		return
	}
	r.popWork(t.Name)
	r.mu.Lock()
	errText := work.err
	r.mu.Unlock()
	if errText != "" {
		if r.action == "install" {
			connFailInstall(ctx, r, op, t, errText)
			return
		}
		connFail(op, t, connDetail(r, t, errText))
		return
	}
	names, discoveryError := connRegisterConnected(ctx, r, t, t.Name)
	connConnect(op, t, names, discoveryError)
}

// Authorize needs no approval, the row's verb opens the link the flow already minted
func connNothing(ctx context.Context, r *connRunner, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
}

func connAnswerEnv(entry *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	raw, _ := entry.Get("env")
	if obj, ok := raw.(*pyjson.Object); ok {
		for _, k := range obj.Keys {
			out.Set(k, pyrepr.Str(obj.Vals[k]))
		}
	}
	return out
}

// Folds the card's answer into the operation, a skip, an approval that starts the work and Continue
func ApplyAnswer(ctx context.Context, op *tools.ConnectionOperation, raw string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			it, ok := p.(*tools.IllegalTransition)
			if !ok {
				panic(p)
			}
			err = it
		}
	}()
	answer, _ := func() (*pyjson.Object, error) {
		v, err := pyjson.Loads(raw)
		obj, _ := v.(*pyjson.Object)
		return obj, err
	}()
	if answer == nil {
		answer = pyjson.NewObject()
	}
	connRunnersMu.Lock()
	runner := connRunners[op.OpID]
	connRunnersMu.Unlock()
	rawTargets, _ := answer.Get("targets")
	entries, _ := rawTargets.([]any)
	for _, e := range entries {
		entry, ok := e.(*pyjson.Object)
		if !ok {
			continue
		}
		name, _ := entry.Get("name")
		t := op.Target(strings.ToLower(strings.TrimSpace(connStr(name))))
		if t == nil {
			continue
		}
		statusRaw, _ := entry.Get("status")
		status := strings.ToLower(connStr(statusRaw))
		switch {
		case status == "skipped":
			kept := connCancelAttempt(runner, t)
			var f tools.TransitionFields
			if kept != "" {
				f.Detail = &kept
			}
			if _, err := op.Transition(t.Name, tools.TargetSkipped, tools.ActorUser, f); err != nil {
				if !op.ResolvedTarget(t) && !op.Settled() {
					return err
				}
			}
		case status == "approved" && runner != nil:
			switch op.StateOf(t) {
			case tools.TargetPending:
				runner.run(ctx, stepApprove, op, t, connAnswerEnv(entry))
			case tools.TargetFailed, tools.TargetExpired:
				runner.run(ctx, stepRetry, op, t, connAnswerEnv(entry))
			}
		}
	}
	if by, _ := answer.Get("settled_by"); by == string(tools.SettleContinue) && !op.AllResolved() {
		op.Settle(tools.SettleContinue)
	}
	return nil
}

// Stops the target's OAuth attempt, the note when the cancel lost the race to a commit
func connCancelAttempt(r *connRunner, t *tools.ConnTarget) string {
	if r == nil {
		return ""
	}
	work := r.popWork(t.Name)
	if work == nil || work.attempt == nil {
		return ""
	}
	flow := work.attempt.Flow()
	if flow == nil {
		return ""
	}
	if CancelAttempt(flow) {
		return authorizationKept
	}
	return ""
}

// Re-runs the named MCP targets on the open operation, the refusal text when this module is not running it
func Retry(ctx context.Context, op *tools.ConnectionOperation, names []string) (msg string, err error) {
	defer func() {
		if p := recover(); p != nil {
			it, ok := p.(*tools.IllegalTransition)
			if !ok {
				panic(p)
			}
			err = it
		}
	}()
	connRunnersMu.Lock()
	runner := connRunners[op.OpID]
	connRunnersMu.Unlock()
	if runner == nil {
		return "this operation has no MCP work to re-run", nil
	}
	if op.Settled() {
		return "this operation has settled; its result is frozen", nil
	}
	for _, name := range names {
		if t := op.Target(name); t != nil {
			runner.run(ctx, stepRetry, op, t, nil)
		}
	}
	return "", nil
}

// Runs every target at once with no card and returns the result with its links
func connNoCardResult(ctx context.Context, r *connRunner, names []string, action, sessionKey string) string {
	targets := make([]*tools.ConnTarget, len(names))
	for i, n := range names {
		targets[i] = tools.NewConnTarget(n, "mcp", action)
	}
	op := tools.NewDetachedOperation(targets, sessionKey)
	for _, t := range op.Targets {
		r.run(ctx, stepNoCard, op, t, nil)
	}
	payload := op.Result(ctx, r.env, true)
	status := "settled"
	for _, t := range op.Targets {
		if op.StateOf(t) == tools.TargetInitiated {
			status = "initiated"
			break
		}
	}
	payload.Set("status", status)
	payload.Set("note", connectorNoCardNote)
	out, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return out
}

// Runs one MCP manage_connections action, a card operation with a callback and the links at once without
func RunMCPOperation(ctx context.Context, env *tools.Env, names []string, action string, opts tools.MCPOperationOptions) string {
	if msg := ValidateMCPNames(ctx, env, action, names); msg != "" {
		return toolError(msg)
	}
	backend, _ := opts.Backend.(ConnectorBackend)
	r := openRunner(env, action, backend)
	sessionKey := tools.OperationSessionKey(env, opts.SessionID)
	if opts.Callback == nil {
		return connNoCardResult(ctx, r, names, action, sessionKey)
	}
	defer r.close(ctx)
	targets := make([]*tools.ConnTarget, len(names))
	for i, n := range names {
		targets[i] = tools.NewConnTarget(n, "mcp", action)
	}
	kind := tools.ConnKind{
		Prepare: func(op *tools.ConnectionOperation) { r.prepare(ctx, op) },
		Observe: func(op *tools.ConnectionOperation) { r.observe(ctx, op) },
		Note:    connectorNote,
	}
	return tools.RunOperation(ctx, env, targets, kind, tools.RunOptions{SessionKey: sessionKey, ToolCallID: opts.ToolCallID,
		Callback: opts.Callback, WithURLsInResult: false})
}
