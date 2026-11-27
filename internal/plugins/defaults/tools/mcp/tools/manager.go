package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/osv"
	"github.com/nickheyer/icebath/internal/packages"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
)

// What sampling needs to reach the helper models of a profile when no session is calling
type Helpers struct {
	Tasks       *auxiliary.Registry
	Providers   *provider.Registry
	Transports  *transport.Registry
	Credentials *credentials.Registry
	// Package manager whose Node, npm and uv run the npx, npm, node, uv and uvx servers
	Packages *packages.Packages
	// Hook points the consents servers ask for are told to, as approvals
	Hooks *hooks.Hooks
}

// Engine of the MCP plugin: the servers each profile connects, their tools, sign ins and connection operations
type Manager struct {
	config  *config.Config
	catalog *mcp.Registry
	helpers Helpers
	learned *auxiliary.State
	owner   mcp.Owner
	// Sign ins in progress, by id
	logins sync.Map
	// Connection operations of manage_connections
	ops *operations

	mu     sync.Mutex
	pools  map[string]*pool
	closed bool
}

func newManager(cfg *config.Config, catalog *mcp.Registry, helpers Helpers, owner mcp.Owner) *Manager {
	m := &Manager{config: cfg, catalog: catalog, helpers: helpers, learned: auxiliary.NewState(), owner: owner, pools: map[string]*pool{}}
	m.ops = newOperations(m)
	return m
}

// Servers of one profile
type pool struct {
	m       *Manager
	profile string
	cfg     *config.Config

	mu      sync.Mutex
	servers map[string]*server
	// Servers connecting now, and why the last connect of each failed
	connecting map[string]bool
	failures   map[string]string
	// When a server that failed to connect may be tried again, and how many times it failed in a row
	cooldown map[string]time.Time
	strikes  map[string]int
	run      *discovery
	// Folder ${workspaceFolder} and stdio servers without a cwd start in, the one of the session that first asked
	workspace string
	providers map[string]*mcp.Provider
	warned    map[string]bool
	stderr    *os.File
	tallies   tallies

	// Tools of every server, built again once something changed
	defsMu  sync.Mutex
	defs    []tools.Def
	version atomic.Int64
	built   int64
}

// One discovery pass of a profile
type discovery struct {
	done chan struct{}
}

func (d *discovery) alive() bool {
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

// Pool of the profile the settings are of
func (m *Manager) pool(cfg *config.Config) *pool {
	profile := cfg.Profile()
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pools[profile]
	if p == nil {
		p = &pool{m: m, profile: profile, cfg: m.config.In(profile), servers: map[string]*server{}, connecting: map[string]bool{}, failures: map[string]string{},
			cooldown: map[string]time.Time{}, strikes: map[string]int{}, providers: map[string]*mcp.Provider{}, warned: map[string]bool{},
			tallies: tallies{spent: map[[2]string]float64{}, asks: map[[2]string]*sync.Mutex{}}}
		m.pools[profile] = p
	}
	return p
}

// Pool of a profile when it exists, nil before anything asked for it
func (m *Manager) existing(profile string) *pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pools[profile]
}

// Marks the tools to be built again
func (p *pool) touch() { p.version.Add(1) }

// Folder of the profile
func (p *pool) dir() string { return p.cfg.ProfileDir(p.profile) }

func (p *pool) workspaceDir() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workspace != "" {
		return p.workspace
	}
	dir, _ := os.Getwd()
	return dir
}

// Every server the profile sets up, those plugins ship included, by name: suspicious ones and ones with broken cost tables left out,
// none at all in safe mode
func (p *pool) specs() map[string]*spec {
	out := map[string]*spec{}
	if safe(p.cfg) {
		return out
	}
	workspace := p.workspaceDir()
	raw := table(p.cfg.Get(mcpconfig.Servers))
	rejected := map[string]bool{}
	for name, value := range raw {
		entry := table(value)
		if entry == nil {
			continue
		}
		if issues := validate(name, entry); len(issues) > 0 {
			slog.Warn("Skipping suspicious MCP server", "server", name, "issues", strings.Join(issues, "; "))
			rejected[name] = true
			continue
		}
		s := parseSpec(p.cfg, name, entry, workspace)
		if s.CostErr != nil {
			slog.Error("Skipping MCP server: fix its tool_costs, spend_limit or cost_unit", "server", name, "error", s.CostErr)
			rejected[name] = true
			continue
		}
		p.mu.Lock()
		s.warnWhitespace(p.warned)
		p.mu.Unlock()
		out[name] = s
	}
	for _, entry := range p.m.catalog.Bundled(p.cfg) {
		switch _, native := out[entry.Name]; {
		case rejected[entry.Name]:
			slog.Warn("MCP server a plugin ships skipped: the profile's own entry of that name was rejected", "server", entry.Name)
		case native:
			// The profile's own settings win over what the plugin ships
		default:
			s := parseSpec(p.cfg, entry.Name, entryConfig(entry, ""), workspace)
			s.Plugin = entry.Plugin
			out[entry.Name] = s
		}
	}
	return out
}

// Settings of one server the profile has, false when it has none of that name
func (p *pool) spec(name string) (*spec, bool) {
	s, ok := p.specs()[name]
	return s, ok
}

// Starts a discovery pass of every enabled server not yet connected, unless one runs; the pass that runs or ran last
func (p *pool) discover(workspace string) *discovery {
	p.mu.Lock()
	if p.workspace == "" && workspace != "" {
		p.workspace = workspace
	}
	if p.run != nil && p.run.alive() {
		run := p.run
		p.mu.Unlock()
		return run
	}
	p.mu.Unlock()
	awaiting := p.awaiting()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.run != nil && p.run.alive() {
		return p.run
	}
	run := &discovery{done: make(chan struct{})}
	if len(awaiting) == 0 {
		close(run.done)
		if p.run == nil {
			p.run = run
		}
		return p.run
	}
	p.run = run
	go func() {
		defer close(run.done)
		p.connectAll(context.Background(), awaiting)
	}()
	return run
}

// Enabled servers of the settings with no server running, none connecting and no cooldown waiting
func (p *pool) awaiting() map[string]*spec {
	specs := p.specs()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]*spec{}
	for name, s := range specs {
		_, live := p.servers[name]
		if s.Enabled && !live && !p.connecting[name] && time.Now().After(p.cooldown[name]) {
			out[name] = s
		}
	}
	return out
}

// Connects the servers at most discovery_concurrency at a time, a lazy one with a fresh schema cache registered from it without a process
func (p *pool) connectAll(ctx context.Context, specs map[string]*spec) {
	limit := p.cfg.GetInt(mcpconfig.DiscoveryConcurrency)
	if limit <= 0 {
		limit = len(specs)
	}
	slots := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	lazy := 0
	for _, name := range slices.Sorted(maps.Keys(specs)) {
		s := specs[name]
		if s.Lazy && p.registerCached(s) {
			lazy++
			continue
		}
		p.mu.Lock()
		p.connecting[name] = true
		p.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			p.connect(ctx, s)
		}()
	}
	wg.Wait()
	if lazy > 0 {
		slog.Info("MCP servers registered from their schema cache without starting them", "profile", p.profile, "servers", lazy)
	}
}

// Connects one server within its connect timeout, keeping a task that parked by itself so it comes back when it can
func (p *pool) connect(ctx context.Context, s *spec) (*server, error) {
	srv := newServer(p, s)
	p.mu.Lock()
	if p.m.closed {
		p.mu.Unlock()
		return nil, errors.New("icebath is shutting down")
	}
	old := p.servers[s.Name]
	p.servers[s.Name] = srv
	p.connecting[s.Name] = true
	delete(p.failures, s.Name)
	p.mu.Unlock()
	if old != nil {
		old.shutdown()
	}
	connectCtx, cancel := context.WithTimeout(ctx, s.ConnectTimeout+30*time.Second)
	err := srv.start(connectCtx)
	timedOut := errors.Is(connectCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	p.mu.Lock()
	delete(p.connecting, s.Name)
	if err == nil {
		delete(p.cooldown, s.Name)
		delete(p.strikes, s.Name)
		p.mu.Unlock()
		p.touch()
		return srv, nil
	}
	if timedOut {
		err = fmt.Errorf("the server did not answer within %s", s.ConnectTimeout+30*time.Second)
	}
	p.failures[s.Name] = describe(err)
	p.strikes[s.Name]++
	p.cooldown[s.Name] = time.Now().Add(min(30*time.Second<<(p.strikes[s.Name]-1), 600*time.Second))
	keep := !timedOut && srv.alive()
	if !keep && p.servers[s.Name] == srv {
		delete(p.servers, s.Name)
	}
	p.mu.Unlock()
	if !keep {
		srv.shutdown()
	}
	note := ""
	if tail := tailLog(logPath(p.dir()), s.Name, 8192, 8); tail != "" {
		note = "\n  child stderr tail (full log: mcp-stderr.log):\n    " + strings.ReplaceAll(tail, "\n", "\n    ")
	}
	slog.Warn("MCP server failed to connect"+note, "server", s.Name, "profile", p.profile, "error", describe(err))
	p.touch()
	return nil, err
}

// Whether the run goroutine still runs
func (s *server) alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Registers a lazy server from its schema cache without starting it, false when the cache holds nothing current for it
func (p *pool) registerCached(s *spec) bool {
	cached, utility, ok := p.cached(s)
	if !ok {
		return false
	}
	srv := newServer(p, s)
	srv.state, srv.cached, srv.cachedUtility = stateLazy, cached, utility
	p.mu.Lock()
	p.servers[s.Name] = srv
	p.mu.Unlock()
	p.touch()
	slog.Info("MCP server registered lazily from its schema cache", "server", s.Name, "tools", len(cached))
	return true
}

// A server's tools changed: built again, and the schema cache written for its next lazy start
func (p *pool) connected(s *server) {
	p.touch()
	p.writeCache(s)
}

// Server of the name the profile runs, a lazy one started and a dormant stdio one woken first
func (p *pool) live(ctx context.Context, name string, wait time.Duration) (*server, error) {
	p.mu.Lock()
	srv := p.servers[name]
	p.mu.Unlock()
	if srv == nil {
		return nil, fmt.Errorf("MCP server '%s' is not connected", name)
	}
	state, session, _ := srv.snapshot()
	switch {
	case session != nil:
		return srv, nil
	case state == stateLazy:
		slog.Info("MCP server lazy start on first use", "server", name)
		s := srv.currentSpec()
		started, err := p.connect(ctx, s)
		if err != nil {
			return nil, err
		}
		p.dropPhantoms(srv, started)
		return started, nil
	case state == stateRecycled:
		srv.signal()
		wait = max(wait, 15*time.Second)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := srv.wait(waitCtx); err != nil {
		return srv, err
	}
	return srv, nil
}

// Logs the cached tools a lazily started server no longer serves, dropped from its tools now it lists its own
func (p *pool) dropPhantoms(lazy, live *server) {
	phantom := difference(names(lazy.cached), names(live.offered()))
	if len(phantom) > 0 {
		slog.Info("MCP server no longer serves tools its schema cache listed; dropped them", "server", lazy.name, "tools", strings.Join(phantom, ", "))
	}
}

// Stops every server of the profile and forgets them
func (p *pool) close() {
	p.mu.Lock()
	servers := slices.Collect(maps.Values(p.servers))
	p.servers = map[string]*server{}
	stderr := p.stderr
	p.stderr = nil
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.shutdown()
		}()
	}
	wg.Wait()
	if stderr != nil {
		stderr.Close()
	}
	p.touch()
}

// Stops the named servers and forgets them
func (p *pool) drop(names ...string) {
	var wg sync.WaitGroup
	for _, name := range names {
		p.mu.Lock()
		srv := p.servers[name]
		delete(p.servers, name)
		delete(p.failures, name)
		delete(p.cooldown, name)
		delete(p.strikes, name)
		p.mu.Unlock()
		if srv != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.shutdown()
			}()
		}
	}
	wg.Wait()
	p.touch()
}

// Shared stderr log of the profile's stdio servers, a sink that drops everything when it can't be opened
func (p *pool) stderrLog() io.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stderr != nil {
		return p.stderr
	}
	path := logPath(p.dir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return io.Discard
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		slog.Debug("MCP stderr log unavailable", "path", path, "error", err)
		return io.Discard
	}
	p.stderr = f
	return f
}

// Variables this process hands every stdio child: those that carry no secret
var safeEnv = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TERM", "SHELL", "TMPDIR"}

// Windows variables launchers need, matched in any casing
var safeWindowsEnv = map[string]bool{"ALLUSERSPROFILE": true, "APPDATA": true, "COMMONPROGRAMFILES": true, "COMMONPROGRAMFILES(X86)": true,
	"COMMONPROGRAMW6432": true, "COMPUTERNAME": true, "COMSPEC": true, "HOMEDRIVE": true, "HOMEPATH": true, "LOCALAPPDATA": true, "NUMBER_OF_PROCESSORS": true,
	"OS": true, "PATHEXT": true, "PROCESSOR_ARCHITECTURE": true, "PROGRAMDATA": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true,
	"PUBLIC": true, "SYSTEMDRIVE": true, "SYSTEMROOT": true, "TEMP": true, "TMP": true, "USERDOMAIN": true, "USERNAME": true, "USERPROFILE": true, "WINDIR": true}

// Environment and command of a stdio child: the safe variables of this process with the server's own over them, a reference that rendered
// to nothing left out; npx, npm, node, uv and uvx the package manager's own, installed when missing and allowed, their folders first on
// PATH; any other bare command found on the child's PATH, whose folder goes first on it
func (p *pool) launchEnv(ctx context.Context, s *spec) (map[string]string, string, error) {
	env := map[string]string{}
	for name, value := range terminal.ProcessEnv() {
		if slices.Contains(safeEnv, name) || strings.HasPrefix(name, "XDG_") || runtime.GOOS == "windows" && safeWindowsEnv[strings.ToUpper(name)] {
			env[name] = value
		}
	}
	for name, value := range s.Env {
		if !s.Unset[name] {
			env[name] = value
		}
	}
	command := terminal.ExpandUser(s.Command)
	launcher := windowsLauncher.ReplaceAllString(command, "")
	if _, family := managedLaunchers[launcher]; family {
		executable, dirs, ok, err := p.managedLauncher(ctx, launcher)
		if err != nil {
			return nil, "", err
		}
		if ok {
			key, found := terminal.PathKey(env)
			if !found {
				key = "PATH"
			}
			env[key] = terminal.PrependMissing(env[key], dirs...)
			return env, executable, nil
		}
	}
	if !strings.ContainsAny(command, `/\`) {
		key, _ := terminal.PathKey(env)
		if found := lookPath(command, env[key]); found != "" {
			command = found
		}
	}
	if dir := filepath.Dir(command); strings.ContainsRune(command, '/') || runtime.GOOS == "windows" && strings.ContainsRune(command, '\\') {
		key, ok := terminal.PathKey(env)
		if !ok {
			key = "PATH"
		}
		env[key] = terminal.PrependMissing(env[key], dir)
	}
	return env, command, nil
}

// Bare MCP launchers the package manager ships, by the package providing them
var managedLaunchers = map[string]string{"npx": packages.NPM, "npm": packages.NPM, "node": packages.NPM, "uv": packages.UV, "uvx": packages.UV}

var windowsLauncher = regexp.MustCompile(`(?i)\.(cmd|exe)$`)

// The package manager's executable of a launcher and the folders its children need first on PATH, installed under the profile's lazy install
// setting; false where the package manager has no build of it for this machine
func (p *pool) managedLauncher(ctx context.Context, command string) (string, []string, bool, error) {
	pm := p.m.helpers.Packages
	name := managedLaunchers[command]
	if missing, err := pm.Missing(name); err != nil || missing != "" {
		return "", nil, false, err
	}
	if _, err := pm.Ensure(ctx, p.cfg, name, packages.Ensuring{}); err != nil {
		return "", nil, false, err
	}
	var dirs []string
	if name == packages.UV {
		launcher, err := pm.UVLauncher(command)
		if err != nil {
			return "", nil, false, err
		}
		if launcher != "" {
			dirs = []string{filepath.Dir(launcher)}
		}
	} else {
		dirs = filepath.SplitList(pm.EnvFor([]string{packages.NPM}, map[string]string{"PATH": ""})["PATH"])
	}
	dirs = slices.DeleteFunc(dirs, func(dir string) bool { return dir == "" })
	executable := packages.Which(command, strings.Join(dirs, string(os.PathListSeparator)))
	if executable == "" {
		return "", nil, false, fmt.Errorf("icebath-managed %s is not installed; run `icebath pm install %s`", command, name)
	}
	return executable, dirs, true, nil
}

// Executable a bare command resolves to on a PATH, empty when none
func lookPath(command, path string) string {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = append([]string{""}, strings.Split(strings.ToLower(cmp.Or(os.Getenv("PATHEXT"), ".COM;.EXE;.BAT;.CMD")), ";")...)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, command+ext)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return candidate
			}
		}
	}
	return ""
}

// Longest the malware check may hold a stdio launch
const osvBound = 12 * time.Second

// Malware check of the package an npx, uvx or pipx launch runs, then the copy npx already cached in place of a resident npm exec
func (p *pool) preflight(ctx context.Context, server, command string, args []string) (string, []string, error) {
	verdict := make(chan string, 1)
	go func() { verdict <- osv.Launch(ctx, p.cfg, command, args) }()
	select {
	case message := <-verdict:
		if message != "" {
			return "", nil, fmt.Errorf("MCP server '%s': %s", server, message)
		}
	case <-time.After(osvBound):
		slog.Warn("MCP malware check timed out (network slow or unreachable); starting the server without it", "server", server)
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	if strings.HasPrefix(strings.ToLower(filepath.Base(command)), "npx") {
		if bin, rest, ok := npxCached(args); ok {
			slog.Debug("MCP server runs its npx cached binary directly", "server", server, "binary", bin)
			return bin, rest, nil
		}
	}
	return command, args, nil
}

// Signs requests to a remote server with OAuth, one provider per server and address
func (p *pool) provider(s *spec, opts mcp.HTTPOptions) (*mcp.Provider, error) {
	if s.OAuthErr != nil {
		return nil, s.OAuthErr
	}
	key := s.Name + "\x00" + s.URL
	p.mu.Lock()
	defer p.mu.Unlock()
	if provider := p.providers[key]; provider != nil {
		return provider, nil
	}
	provider, err := mcp.NewProvider(s.Name, s.URL, s.OAuth, p.store(s.Name), opts)
	if err != nil {
		return nil, err
	}
	p.providers[key] = provider
	return provider, nil
}

// OAuth state of a server on disk
func (p *pool) store(name string) *mcp.Store {
	return mcp.NewStore(filepath.Join(p.dir(), "mcp-tokens"), name)
}

// Forgets the providers of a server, after its sign in state changed on disk
func (p *pool) forgetProvider(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.providers {
		if strings.HasPrefix(key, name+"\x00") {
			delete(p.providers, key)
		}
	}
}

// Stops every server of every profile
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	pools := slices.Collect(maps.Values(m.pools))
	m.mu.Unlock()
	for _, p := range pools {
		p.close()
	}
	return nil
}

// Stops the servers of a profile being deleted
func (m *Manager) Delete(profile string) error {
	m.mu.Lock()
	p := m.pools[profile]
	delete(m.pools, profile)
	m.mu.Unlock()
	if p != nil {
		p.close()
	}
	return nil
}

// Stops the servers of a profile before its folder moves to a new name, where they start again as they are next used
func (m *Manager) Stop(profile string) error {
	return m.Delete(profile)
}
