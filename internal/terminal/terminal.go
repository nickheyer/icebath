package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How often unused environments are looked for
const reapEvery = time.Minute

// Longest the backends' removal of their sandboxes is waited on when icebath stops
const teardownWait = 15 * time.Second

type Registrant interface {
	Backends() []Backend
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Terminal
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Backends() []Backend {
	var backends []Backend
	for _, registrant := range r.registrants {
		backends = append(backends, registrant.Backends()...)
	}
	return backends
}

// Backend of the name
func (r *Registry) Backend(name string) (Backend, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, backend := range r.Backends() {
		if backend.Name == name {
			return backend, true
		}
	}
	return Backend{}, false
}

// Every backend as a value terminal.backend may take. How each stands is what its page in Sandbox says
func (r *Registry) Options() []config.Option {
	var options []config.Option
	for _, backend := range r.Backends() {
		options = append(options, config.Option{Value: backend.Name, Title: backend.Title, About: backend.About})
	}
	return options
}

// Host the registry is nested in, through which plugins reach its environments and background jobs
func (r *Registry) Host() *Terminal {
	return r.host
}

// Egress proxy that keeps provider keys out of sandboxes, swapping them in their traffic for tokens of its own, as a profile has it
type Proxy struct {
	// Sandbox traffic goes through the proxy, and a sandbox refuses to start while the proxy is not ready
	Enabled, Enforce bool
	// The proxy was set up, and its process runs and listens on Port, the port after it taking plain http
	Configured, Running bool
	Port                int
	// Host file of the certificate of the proxy's authority, which sandboxes trust
	CA string
	// Provider keys the proxy swaps
	Tokens []ProxyToken
}

// One provider key the egress proxy swaps for a token of its own
type ProxyToken struct {
	// Variable holding the real key, and the others that name the same key
	Name    string
	Aliases []string
	Token   string
}

// What routes the traffic of sandboxes through the egress proxy. A registrant that does says so by having this too
type Proxier interface {
	Proxy(cfg *config.Config) Proxy
}

// Registrant with work left once every environment is gone, as icebath stops. A registrant that has some says so by having this too
type Sweeper interface {
	Sweep()
}

// Egress proxy of the profile, off when no registrant routes sandbox traffic
func (r *Registry) Proxy(cfg *config.Config) Proxy {
	proxy, _ := r.proxy(cfg)
	return proxy
}

func (r *Registry) proxy(cfg *config.Config) (Proxy, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if proxy, ok := nested.proxy(cfg); ok {
				return proxy, true
			}
			continue
		}
		if proxier, ok := registrant.(Proxier); ok {
			return proxier.Proxy(cfg), true
		}
	}
	return Proxy{}, false
}

// Runs the work every registrant has left as icebath stops
func (r *Registry) sweep() {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			nested.sweep()
			continue
		}
		if sweeper, ok := registrant.(Sweeper); ok {
			sweeper.Sweep()
		}
	}
}

// Where the agent's commands run: the backends plugins register, the environments kept for sessions and profiles, and the background jobs
type Terminal struct {
	Registry
	config *config.Config
	// Keeps the process groups of commands dying with icebath
	Supervisor *Supervisor
	// Background processes
	Jobs *Jobs
	// Hook points the output of background processes passes through on its way to the model
	hooks *hooks.Hooks

	mu       sync.Mutex
	kept     map[string]*kept
	creating map[string]*sync.Mutex
	// Folder each session's commands last ended in, by profile and session
	folders map[string]string
	// Sudo password each session gave, by profile and session
	sudo map[string]string
	// Shell of each session's client, as the session froze it, by profile and session
	clients map[string]*tools.Client
	// Session whose environments each session's commands run in, by profile and session, such as a subagent's in its parent's
	aliases  map[string]string
	probes   map[string]string
	reaping  bool
	stop     chan struct{}
	reaped   chan struct{}
	live     foreground
	ports    forwards
	teardown sync.WaitGroup
}

// One environment kept and what decides when it goes
type kept struct {
	env      Environment
	backend  Backend
	config   *config.Config
	shared   bool
	sessions map[string]bool
	last     time.Time
	calls    int
}

func New(cfg *config.Config, hk *hooks.Hooks) *Terminal {
	t := &Terminal{
		config:     cfg,
		hooks:      hk,
		Supervisor: NewSupervisor(),
		kept:       map[string]*kept{},
		creating:   map[string]*sync.Mutex{},
		folders:    map[string]string{},
		aliases:    map[string]string{},
		sudo:       map[string]string{},
		clients:    map[string]*tools.Client{},
		probes:     map[string]string{},
	}
	t.live.cond = sync.NewCond(&t.live.mu)
	t.live.procs = map[Proc]Runner{}
	t.Jobs = newJobs(t)
	terminalconfig.Register(cfg, t.Options)
	// Every write of the place in use a person asks for chooses it as the page of Sandbox does
	cfg.Guard(terminalconfig.Backend, config.Guard{Write: func(cfg *config.Config, _ string, value any) (bool, error) {
		name, ok := value.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return false, nil
		}
		_, _, err := t.Use(cfg, name)
		return true, err
	}})
	cfg.Guard(terminalconfig.CredentialFiles, config.Guard{Check: checkProfileFiles})
	return t
}

// Why files for sandboxes can't be kept: each names a file inside the profile's folder, by its path from there, which no path of this
// computer is
func checkProfileFiles(_ *config.Config, _ string, value any) error {
	var names []string
	switch held := value.(type) {
	case []string:
		names = held
	case []any:
		for _, item := range held {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if rel := filepath.Clean(name); filepath.IsAbs(name) || strings.HasPrefix(name, "~") || !filepath.IsLocal(rel) {
			return fmt.Errorf("%s is not inside the profile's folder. Write the path of the file from there, such as auth.json.", name)
		}
	}
	return nil
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (t *Terminal) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = t
	}
	t.Registry.Register(registrant)
}

// Backend the profile picked, not isolated when its settings hand the sandbox folders of the host, or an error naming every one there is
func (t *Terminal) Configured(cfg *config.Config) (Backend, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.GetString(terminalconfig.Backend)))
	if backend, ok := t.Named(cfg, name); ok {
		return backend, nil
	}
	return Backend{}, fmt.Errorf("icebath has no place to run commands named %q. Choose one of %s in /sandbox.", name, t.titles())
}

// Titles of every backend as one list, the last after "or"
func (r *Registry) titles() string {
	var titles []string
	for _, backend := range r.Backends() {
		titles = append(titles, backend.Title)
	}
	if len(titles) == 0 {
		return "the places that are installed"
	}
	return strs.Or(titles)
}

// Backend of the name as the profile's settings have it, not isolated when they hand its sandbox the folders of the host
func (t *Terminal) Named(cfg *config.Config, name string) (Backend, bool) {
	backend, ok := t.Backend(strings.ToLower(strings.TrimSpace(name)))
	if ok && backend.Exposed != nil && backend.Exposed(cfg) {
		backend.Isolated = false
	}
	return backend, ok
}

// Why the profile's backend can't run, nil when it can
func (t *Terminal) Check(cfg *config.Config) error {
	backend, err := t.Configured(cfg)
	if err != nil {
		return err
	}
	if backend.Check != nil {
		return backend.Check(cfg)
	}
	return nil
}

// Session's key under the profile
func sessionKey(cfg *config.Config, session string) string {
	return cfg.Profile() + "/" + cmp.Or(session, "default")
}

// Key the profile's environment for the session working in dir is kept under
func (t *Terminal) key(cfg *config.Config, backend Backend, session, dir string) (string, bool) {
	if backend.Shared != nil {
		if shared := backend.Shared(cfg, dir); shared != "" {
			return cfg.Profile() + "/" + backend.Name + "/" + shared, true
		}
	}
	return cfg.Profile() + "/" + backend.Name + "/session:" + cmp.Or(t.aliasOf(cfg, session), "default"), false
}

// Key of an environment of the backend without the profile and the backend before it, such as session:<id>, what a sandbox is named for
func Task(cfg *config.Config, backend, key string) string {
	return strings.TrimPrefix(key, cfg.Profile()+"/"+backend+"/")
}

// Whether an environment of the profile's backend is kept for the session: one it was held for, or one every session of the profile shares
func (t *Terminal) Kept(cfg *config.Config, session string) bool {
	backend, err := t.Configured(cfg)
	if err != nil {
		return false
	}
	key, _ := t.key(cfg, backend, session, "")
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.kept[key] != nil {
		return true
	}
	for _, k := range t.kept {
		if k.config.Profile() == cfg.Profile() && k.backend.Name == backend.Name && k.sessions[session] {
			return true
		}
	}
	return false
}

// Sessions the environment kept under the key served, sorted
func (t *Terminal) Sessions(key string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.kept[key]
	if k == nil {
		return nil
	}
	sessions := make([]string, 0, len(k.sessions))
	for session := range k.sessions {
		sessions = append(sessions, session)
	}
	slices.Sort(sessions)
	return sessions
}

// An environment held for one call, kept from the idle cleanup until released
type Lease struct {
	Env     Environment
	Backend Backend
	Key     string
	t       *Terminal
	cfg     *config.Config
	// Session the call is for
	session string
	once    sync.Once
}

// Lets the idle cleanup have the environment again from now
func (l *Lease) Release() {
	l.once.Do(func() {
		l.t.mu.Lock()
		defer l.t.mu.Unlock()
		if k := l.t.kept[l.Key]; k != nil {
			k.calls--
			k.last = time.Now()
		}
	})
}

// Environment of the profile's backend for the session, made when there is none, held until the lease is released
func (t *Terminal) Acquire(ctx context.Context, cfg *config.Config, session, dir string) (*Lease, error) {
	backend, err := t.Configured(cfg)
	if err != nil {
		return nil, err
	}
	key, shared := t.key(cfg, backend, session, dir)
	if lease := t.hold(key, cfg, backend, session); lease != nil {
		return lease, nil
	}
	lock := t.creation(key)
	lock.Lock()
	defer lock.Unlock()
	if lease := t.hold(key, cfg, backend, session); lease != nil {
		return lease, nil
	}
	cwd, host := t.plan(cfg, backend, session, dir)
	spec := &Spec{
		Terminal: t,
		Config:   cfg,
		Dir:      cfg.ProfileDir(cfg.Profile()),
		Key:      key,
		Cwd:      cwd,
		HostCwd:  host,
		Folder:   dir,
		Timeout:  time.Duration(cfg.GetInt(terminalconfig.Timeout)) * time.Second,
	}
	if !shared {
		spec.Session = t.aliasOf(cfg, session)
	}
	slog.Info(fmt.Sprintf("Creating new %s environment for %s...", backend.Name, key))
	env, err := backend.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	stampHome(env, backend, cwd)
	t.mu.Lock()
	t.kept[key] = &kept{env: env, backend: backend, config: cfg, shared: shared, sessions: map[string]bool{session: true}, last: time.Now()}
	t.mu.Unlock()
	t.startReaper()
	slog.Info(fmt.Sprintf("%s environment ready for %s", backend.Name, key))
	if lease := t.hold(key, cfg, backend, session); lease != nil {
		return lease, nil
	}
	return nil, errors.New("The place where commands run was removed while icebath was setting it up. Run the command again.")
}

// Records the home a backend declared on its environment, ~ meaning the folder it started in, unless the environment found its home itself
func stampHome(env Environment, backend Backend, cwd string) {
	if env.SandboxHome() != "" {
		return
	}
	home := backend.Cwd
	if home == "~" {
		home = ""
		if cwd == "~" {
			home = env.Cwd()
		}
	}
	if !strings.HasPrefix(home, "/") || home == "/" {
		return
	}
	if s, ok := env.(interface{ StampHome(string) }); ok {
		s.StampHome(home)
	}
}

// Lease of the environment of the profile's backend kept for the session working in dir, nil when none is kept; nothing is made
func (t *Terminal) Held(cfg *config.Config, session, dir string) *Lease {
	backend, err := t.Configured(cfg)
	if err != nil {
		return nil
	}
	key, _ := t.key(cfg, backend, session, dir)
	return t.hold(key, cfg, backend, session)
}

// Removes the environment of the profile's backend kept for the session working in dir, so the next command makes it again, such as on
// an image the user switched to
func (t *Terminal) Evict(cfg *config.Config, session, dir string) {
	if lease := t.Held(cfg, session, dir); lease != nil {
		lease.Evict()
	}
}

// Lease of the kept environment, nil when there is none
func (t *Terminal) hold(key string, cfg *config.Config, backend Backend, session string) *Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.kept[key]
	if k == nil {
		return nil
	}
	k.calls++
	k.last = time.Now()
	k.sessions[session] = true
	return &Lease{Env: k.env, Backend: backend, Key: key, t: t, cfg: cfg, session: session}
}

func (t *Terminal) creation(key string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	lock := t.creating[key]
	if lock == nil {
		lock = new(sync.Mutex)
		t.creating[key] = lock
	}
	return lock
}

// Folder commands start in when nothing names one: the session's for a backend on this machine, the backend's own otherwise
func defaultCwd(backend Backend, dir string) string {
	if !backend.Remote {
		return dir
	}
	return cmp.Or(backend.Cwd, "/root")
}

// The working folder as the place the commands run in reads it, which is no path of this computer: a ~ stays for the home folder of a remote
// place, and is written out only for a place on this computer
func inPlace(cwd string, backend Backend) string {
	if backend.Remote && strings.HasPrefix(cwd, "~") {
		return cwd
	}
	return fsx.ExpandHome(cwd)
}

// Folder a new environment starts in and the host folder it mounts
func (t *Terminal) plan(cfg *config.Config, backend Backend, session, dir string) (string, string) {
	fallback := defaultCwd(backend, dir)
	cwd := strings.TrimSpace(cfg.GetString(terminalconfig.Cwd))
	if Placeholder(cwd) {
		cwd = fallback
	} else {
		cwd = inPlace(cwd, backend)
	}
	host := ""
	if backend.Mount != nil {
		var sandbox string
		if host, sandbox = backend.Mount(cfg, cmp.Or(dir, cwd)); host != "" {
			cwd = sandbox
		}
	}
	if recorded := t.Folder(cfg, session); recorded != "" {
		cwd = recorded
	}
	if backend.Container && unusableInContainer(cwd, host) {
		remapped := fallback
		if host != "" {
			_, remapped = backend.Mount(cfg, cmp.Or(dir, cwd))
		}
		if cwd != remapped {
			slog.Info("Ignoring the working folder for this backend (a host or relative path does not work in the sandbox)", "cwd", cwd, "backend", backend.Name, "using", remapped)
		}
		cwd = remapped
	}
	return t.remap(cfg, backend, cwd), host
}

func (t *Terminal) remap(cfg *config.Config, backend Backend, cwd string) string {
	if backend.Remap != nil {
		return backend.Remap(cfg, cwd)
	}
	return cwd
}

// Path the sandbox sees a host path at, through the host folder it mounted
func visible(p string, backend Backend, env Environment) string {
	if p == "" || !backend.Container {
		return p
	}
	host, mount := env.Mount()
	if translated := TranslateMounted(p, host, mount); translated != "" {
		return translated
	}
	return p
}

// Folder a command of the call runs in: workdir when given, else where the session's commands last ended, else the default, each made
// one the sandbox can enter
func (l *Lease) Folder(workdir string) string {
	backend, env := l.Backend, l.Env
	if workdir != "" {
		return l.t.remap(l.cfg, backend, visible(workdir, backend, env))
	}
	recorded := l.t.Folder(l.cfg, l.session)
	host, mount := env.Mount()
	if recorded != "" && backend.Container && unusableInContainer(recorded, host) && !inSandboxHome(recorded, env) {
		if shown := visible(recorded, backend, env); shown != recorded {
			return shown
		}
		if mountedHost(recorded, host) && mount != "" {
			slog.Info("Remapping recorded session cwd for backend (mounted host directory)", "cwd", recorded, "backend", backend.Name, "using", mount)
			return mount
		}
		slog.Info("Ignoring recorded session cwd for backend (host/relative path won't work in sandbox)", "cwd", recorded, "backend", backend.Name)
		return l.t.remap(l.cfg, backend, cmp.Or(mount, env.Cwd()))
	}
	if recorded == "" {
		recorded = env.Cwd()
	}
	return l.t.remap(l.cfg, backend, recorded)
}

// Notes the folder the session's commands ended in
func (l *Lease) Record(cwd string) {
	if strings.TrimSpace(cwd) == "" {
		return
	}
	l.t.mu.Lock()
	l.t.folders[sessionKey(l.cfg, l.session)] = cwd
	l.t.mu.Unlock()
}

// Folder the session's commands last ended in, empty before any
func (t *Terminal) Folder(cfg *config.Config, session string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.folders[sessionKey(cfg, session)]
}

// Sudo password the session gave, empty when none
func (t *Terminal) SudoPassword(cfg *config.Config, session string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sudo[sessionKey(cfg, session)]
}

// Keeps the sudo password the session gave, an empty one forgetting it
func (t *Terminal) SetSudoPassword(cfg *config.Config, session, password string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if password == "" {
		delete(t.sudo, sessionKey(cfg, session))
		return
	}
	t.sudo[sessionKey(cfg, session)] = password
}

// Drops the environment of the call after its backend failed, so the next call makes it again
func (l *Lease) Evict() {
	l.Release()
	l.t.mu.Lock()
	k := l.t.kept[l.Key]
	delete(l.t.kept, l.Key)
	delete(l.t.creating, l.Key)
	l.t.mu.Unlock()
	if k != nil {
		l.t.closeForwards(k.env)
		if err := k.env.Cleanup(false); err != nil {
			slog.Debug("cleanup of degraded environment failed", "error", err)
		}
	}
}

// Keeps the shell of the session's client its commands on this machine start from, as the session froze it; nil keeps none
func (t *Terminal) Bind(cfg *config.Config, session string, client *tools.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if client == nil {
		delete(t.clients, sessionKey(cfg, session))
		return
	}
	t.clients[sessionKey(cfg, session)] = client
}

// Shell of the session's client, nil when it has none
func (t *Terminal) Client(cfg *config.Config, session string) *tools.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clients[sessionKey(cfg, session)]
}

// Ends the session, closed or deleted: kills its background processes but those kept on release, removes its own environments and
// forgets its folder, sudo password and client
func (t *Terminal) Release(ctx context.Context, session string) {
	t.Jobs.Release(ctx, session)
	t.mu.Lock()
	var gone []*kept
	var keys []string
	for key, k := range t.kept {
		// An environment the session ran in as another's, such as a subagent in its parent's, stays with that session
		if !k.shared && k.sessions[session] && strings.HasSuffix(key, "/session:"+session) {
			gone = append(gone, k)
			keys = append(keys, key)
		}
		delete(k.sessions, session)
	}
	for _, key := range keys {
		delete(t.kept, key)
		delete(t.creating, key)
	}
	for _, table := range []map[string]string{t.folders, t.sudo, t.aliases} {
		for key := range table {
			if strings.HasSuffix(key, "/"+session) {
				delete(table, key)
			}
		}
	}
	for key := range t.clients {
		if strings.HasSuffix(key, "/"+session) {
			delete(t.clients, key)
		}
	}
	t.mu.Unlock()
	for i, k := range gone {
		t.tearDown(k, keys[i], false, false)
	}
}

// Stops what runs for a profile before its folder moves to a new name, its background processes and environments, as a delete does
func (t *Terminal) Stop(profile string) error {
	return t.Delete(profile)
}

// Stops what the deleted profile left: its background processes, those kept on release included, and its environments
func (t *Terminal) Delete(profile string) error {
	t.Jobs.Forget(profile)
	t.mu.Lock()
	gone := map[string]*kept{}
	for key, k := range t.kept {
		if k.config.Profile() == profile {
			gone[key] = k
			delete(t.kept, key)
			delete(t.creating, key)
		}
	}
	for _, table := range []map[string]string{t.folders, t.sudo} {
		for key := range table {
			if strings.HasPrefix(key, profile+"/") {
				delete(table, key)
			}
		}
	}
	for key := range t.clients {
		if strings.HasPrefix(key, profile+"/") {
			delete(t.clients, key)
		}
	}
	t.mu.Unlock()
	for key, k := range gone {
		t.tearDown(k, key, true, false)
	}
	return nil
}

// Removes an environment, or stops it for a persistent sandbox that only went idle
func (t *Terminal) tearDown(k *kept, key string, force, idle bool) {
	t.closeForwards(k.env)
	var err error
	if stopper, ok := k.env.(Stopper); ok && idle && k.shared {
		err = stopper.StopIdle()
	} else {
		err = k.env.Cleanup(force)
	}
	switch {
	case err == nil:
		slog.Info("Cleaned up environment", "key", key)
	case strings.Contains(err.Error(), "404") || strings.Contains(strings.ToLower(err.Error()), "not found"):
		slog.Info("Environment already cleaned up", "key", key)
	default:
		slog.Warn("Error cleaning up environment", "key", key, "error", err)
	}
}

// Starts the idle cleanup once
func (t *Terminal) startReaper() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reaping {
		return
	}
	t.reaping = true
	t.stop, t.reaped = make(chan struct{}), make(chan struct{})
	go func(stop, reaped chan struct{}) {
		defer close(reaped)
		for {
			t.reap()
			select {
			case <-stop:
				return
			case <-time.After(reapEvery):
			}
		}
	}(t.stop, t.reaped)
}

// Removes environments unused for longer than their profile's idle limit, those whose sessions run background processes kept
func (t *Terminal) reap() {
	now := time.Now()
	t.mu.Lock()
	var gone []*kept
	var keys []string
	for key, k := range t.kept {
		var sessions []string
		for session := range k.sessions {
			sessions = append(sessions, session)
		}
		if t.Jobs.Running(sessions...) {
			k.last = now
			continue
		}
		lifetime := time.Duration(max(k.config.GetInt(terminalconfig.Lifetime), 0)) * time.Second
		if k.calls == 0 && now.Sub(k.last) > lifetime {
			gone = append(gone, k)
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		delete(t.kept, key)
		delete(t.creating, key)
	}
	t.mu.Unlock()
	for i, k := range gone {
		t.tearDown(k, keys[i], false, true)
	}
}

// Stops every background process, foreground command and environment, as icebath stops
func (t *Terminal) Close() error {
	t.mu.Lock()
	stop, reaped := t.stop, t.reaped
	t.reaping, t.stop, t.reaped = false, nil, nil
	t.mu.Unlock()
	if stop != nil {
		close(stop)
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
		}
	}
	t.KillForeground(true)
	t.Jobs.Close()
	t.mu.Lock()
	remaining := t.kept
	t.kept, t.creating = map[string]*kept{}, map[string]*sync.Mutex{}
	t.mu.Unlock()
	if len(remaining) > 0 {
		slog.Info("Shutting down remaining sandboxes", "count", len(remaining))
	}
	for key, k := range remaining {
		t.tearDown(k, key, false, false)
	}
	for _, k := range remaining {
		if waiter, ok := k.env.(CleanupWaiter); ok {
			waiter.WaitCleanup(teardownWait)
		}
	}
	done := make(chan struct{})
	go func() {
		t.teardown.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(teardownWait):
	}
	t.sweep()
	return nil
}

// Runs a backend's removal of a sandbox in the background, waited on when icebath stops
func (t *Terminal) Background(work func()) {
	t.teardown.Go(work)
}

// Foreground commands in flight in every environment, and the fence that stops new ones once icebath exits
type foreground struct {
	mu       sync.Mutex
	cond     *sync.Cond
	procs    map[Proc]Runner
	fenced   bool
	spawning int
}

// Counts a start of a foreground command, false once icebath is exiting
func (t *Terminal) enter() bool {
	t.live.mu.Lock()
	defer t.live.mu.Unlock()
	if t.live.fenced {
		return false
	}
	t.live.spawning++
	return true
}

// Records a started command, answering whether icebath began exiting meanwhile
func (t *Terminal) leave(runner Runner, p Proc) bool {
	t.live.mu.Lock()
	defer t.live.mu.Unlock()
	t.live.spawning--
	if p != nil {
		t.live.procs[p] = runner
	}
	t.live.cond.Broadcast()
	return t.live.fenced
}

func (t *Terminal) forget(p Proc) {
	t.live.mu.Lock()
	delete(t.live.procs, p)
	t.live.mu.Unlock()
}

// Kills every foreground command in flight, now raising the fence so none starts and killing at once, answering how many
func (t *Terminal) KillForeground(now bool) int {
	t.live.mu.Lock()
	if now {
		t.live.fenced = true
		deadline := time.Now().Add(500 * time.Millisecond)
		wake := time.AfterFunc(500*time.Millisecond, func() {
			t.live.mu.Lock()
			t.live.cond.Broadcast()
			t.live.mu.Unlock()
		})
		for t.live.spawning != 0 && time.Now().Before(deadline) {
			t.live.cond.Wait()
		}
		wake.Stop()
	}
	live := make(map[Proc]Runner, len(t.live.procs))
	for p, runner := range t.live.procs {
		live[p] = runner
	}
	t.live.mu.Unlock()
	var wg sync.WaitGroup
	for p, runner := range live {
		if !now {
			runner.Kill(p)
			continue
		}
		wg.Go(func() { runner.ForceKill(p) })
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
	}
	return len(live)
}

const probeCommand = "printf 'os=%s\\nkernel=%s\\n' \"$(uname -s 2>/dev/null || echo unknown)\" \"$(uname -r 2>/dev/null || echo unknown)\""

// What the machine of a remote backend runs, as "  OS: Linux 6.1"; probed in an environment made for it once per profile, backend and
// working folder, empty when the probe failed
func (t *Terminal) Probe(ctx context.Context, cfg *config.Config, backend Backend) string {
	key := cfg.Profile() + "/" + backend.Name + "/" + cfg.GetString(terminalconfig.Cwd)
	t.mu.Lock()
	probed, ok := t.probes[key]
	t.mu.Unlock()
	if ok {
		return probed
	}
	probed = t.probe(ctx, cfg, backend)
	t.mu.Lock()
	t.probes[key] = probed
	t.mu.Unlock()
	return probed
}

func (t *Terminal) probe(ctx context.Context, cfg *config.Config, backend Backend) string {
	cwd, host := t.plan(cfg, backend, "", "")
	env, err := backend.Create(ctx, &Spec{
		Terminal: t,
		Config:   cfg,
		Dir:      cfg.ProfileDir(cfg.Profile()),
		Key:      cfg.Profile() + "/" + backend.Name + "/prompt-backend-probe",
		Cwd:      cwd,
		HostCwd:  host,
		Timeout:  time.Duration(cfg.GetInt(terminalconfig.Timeout)) * time.Second,
		Probe:    true,
	})
	if err != nil {
		slog.Debug("Backend probe failed", "backend", backend.Name, "error", err)
		return ""
	}
	result, err := env.Execute(ctx, probeCommand, Exec{Timeout: 4 * time.Second})
	if cleanup := env.Cleanup(true); cleanup != nil {
		slog.Debug("Backend probe cleanup failed", "error", cleanup)
	}
	if err != nil || result.Code != 0 {
		slog.Debug("Backend probe failed", "backend", backend.Name, "error", err)
		return ""
	}
	facts := map[string]string{}
	for _, line := range strings.Split(result.Output, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			facts[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	var parts []string
	for _, name := range []string{"os", "kernel"} {
		if value := facts[name]; value != "" && value != "unknown" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  OS: " + strings.Join(parts, " ")
}
