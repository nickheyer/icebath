package env

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	reaperInterval     = 60 * time.Second
	reaperJoinLimit    = 5 * time.Second
	exitCleanupTimeout = 15 * time.Second
)

type ToolGateway interface {
	tool.ManagedGateway
	Ready(ctx context.Context, vendor string) bool
}

type InstallHints interface {
	TerminalBackendInstallHint(ctx context.Context, backend string) string
}

type Processes interface {
	HasActiveProcesses(taskID string) bool
	RunningInContainer(key string) bool
}

type FileOps interface {
	ClearFileOpsCache(taskID string)
}

type HostedProvider interface {
	Hosted() bool
}

type hostMounter interface {
	HostCwd() string
	HostCwdMount() string
}

type idleStopper interface {
	StopIdle(ctx context.Context) error
}

type cleanupWaiter interface {
	WaitForCleanup(ctx context.Context, timeout time.Duration)
}

type sessionScoper interface {
	SessionScoped() bool
}

func HostMount(e tool.Environment) (hostCwd, mount string, ok bool) {
	sh, isShell := e.(*Shell)
	if !isShell {
		return "", "", false
	}
	hm, has := sh.b.(hostMounter)
	if !has {
		return "", "", false
	}
	return hm.HostCwd(), hm.HostCwdMount(), hm.HostCwd() != ""
}

type ManagerOptions struct {
	Config     *config.Store
	Homes      *home.Resolver
	Secrets    *secret.Scope
	Host       *Host
	Settings   *Settings
	Providers  *Providers
	Foreground *Foreground
	Prepare    CommandPreparer
	Gateway    ToolGateway
	Hints      InstallHints
	Processes  Processes
	FileOps    FileOps
}

type Manager struct {
	cfg       *config.Store
	homes     *home.Resolver
	secrets   *secret.Scope
	host      *Host
	settings  *Settings
	providers *Providers
	fg        *Foreground
	prepare   CommandPreparer
	gateway   ToolGateway
	hints     InstallHints
	processes Processes
	fileOps   FileOps

	mu       sync.Mutex
	active   map[string]*Shell
	last     map[string]time.Time
	inFlight map[string]int

	createMu sync.Mutex
	creating map[string]*sync.Mutex

	ovMu      sync.Mutex
	overrides map[string]TaskOverrides

	cwdMu      sync.Mutex
	sessionCwd map[string]string

	aliasMu sync.Mutex
	aliases map[string]string

	reasonMu  sync.Mutex
	reason    string
	reasonSet bool

	reaperMu   sync.Mutex
	reaperStop chan struct{}
	reaperDone chan struct{}

	probe *toolchainProbe
}

func NewManager(o ManagerOptions) (*Manager, error) {
	switch {
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "env.ManagerOptions.Config"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "env.ManagerOptions.Homes"}
	case o.Secrets == nil:
		return nil, &tool.UnfilledError{Name: "env.ManagerOptions.Secrets"}
	case o.Host == nil:
		return nil, &tool.UnfilledError{Name: "env.Host"}
	case o.Settings == nil:
		return nil, &tool.UnfilledError{Name: "env.Settings"}
	case o.Providers == nil:
		return nil, &tool.UnfilledError{Name: "env.Providers"}
	case o.Foreground == nil:
		return nil, &tool.UnfilledError{Name: "env.ManagerOptions.Foreground"}
	case o.Prepare == nil:
		return nil, &tool.UnfilledError{Name: "env.CommandPreparer"}
	case o.Gateway == nil:
		return nil, &tool.UnfilledError{Name: "env.ToolGateway"}
	case o.Hints == nil:
		return nil, &tool.UnfilledError{Name: "env.InstallHints"}
	case o.Processes == nil:
		return nil, &tool.UnfilledError{Name: "env.Processes"}
	case o.FileOps == nil:
		return nil, &tool.UnfilledError{Name: "env.FileOps"}
	}
	return &Manager{
		cfg: o.Config, homes: o.Homes, secrets: o.Secrets, host: o.Host, settings: o.Settings, providers: o.Providers,
		fg: o.Foreground, prepare: o.Prepare, gateway: o.Gateway, hints: o.Hints, processes: o.Processes, fileOps: o.FileOps,
		active: map[string]*Shell{}, last: map[string]time.Time{}, inFlight: map[string]int{},
		creating: map[string]*sync.Mutex{}, overrides: map[string]TaskOverrides{}, sessionCwd: map[string]string{},
		aliases: map[string]string{}, probe: newToolchainProbe(),
	}, nil
}

func (m *Manager) Settings() *Settings { return m.settings }

func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

func (m *Manager) lookupLocked(effective, taskID string) *Shell {
	for _, key := range []string{effective, taskID} {
		if key == "" {
			continue
		}
		if sh, ok := m.active[key]; ok {
			m.last[key] = time.Now()
			return sh
		}
	}
	return nil
}

func (m *Manager) creationLock(key string) *sync.Mutex {
	m.createMu.Lock()
	defer m.createMu.Unlock()
	lock, ok := m.creating[key]
	if !ok {
		lock = &sync.Mutex{}
		m.creating[key] = lock
	}
	return lock
}

func (m *Manager) Acquire(ctx context.Context, c *tool.Call) (tool.Environment, error) {
	if c == nil {
		return nil, &tool.UnfilledError{Name: "tool.Call"}
	}
	p, err := m.Place(ctx, c.TaskID, false)
	if err != nil {
		return nil, err
	}
	sh, err := m.AcquirePlaced(ctx, c.TaskID, p, p.Config.Timeout)
	if err != nil {
		return nil, err
	}
	return sh, nil
}

func (m *Manager) AcquirePlaced(ctx context.Context, taskID string, p Placement, timeoutSeconds int) (*Shell, error) {
	m.startReaper()
	m.mu.Lock()
	sh := m.lookupLocked(p.Key, taskID)
	m.mu.Unlock()
	if sh != nil {
		return sh, nil
	}
	lock := m.creationLock(p.Key)
	lock.Lock()
	defer lock.Unlock()
	m.mu.Lock()
	sh = m.lookupLocked(p.Key, taskID)
	m.mu.Unlock()
	if sh != nil {
		return sh, nil
	}
	slog.Info(fmt.Sprintf("Creating new %s environment for task %s...", p.Kind, shortKey(p.Key)))
	created, err := m.create(ctx, buildRequest{config: p.Config, kind: p.Kind, image: p.Image, cwd: p.Cwd,
		timeout: timeoutSeconds, key: p.Key, hostCwd: p.HostCwd})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.active[p.Key] = created
	m.last[p.Key] = time.Now()
	m.mu.Unlock()
	slog.Info(fmt.Sprintf("%s environment ready for task %s", p.Kind, shortKey(p.Key)))
	return created, nil
}

func (m *Manager) Ensure(ctx context.Context, taskID string) (*Shell, error) {
	cfg, err := m.settings.EnvConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Kind == "local" {
		return nil, nil
	}
	key, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		return nil, err
	}
	existing, ok, err := m.ActiveEnv(ctx, key)
	if err != nil {
		return nil, err
	}
	if ok {
		m.mu.Lock()
		m.last[key] = time.Now()
		m.mu.Unlock()
		return existing, nil
	}
	ov, err := m.ResolveTaskOverrides(ctx, taskID)
	if err != nil {
		return nil, err
	}
	image := selectImage(cfg.Kind, ov, cfg)
	m.startReaper()
	lock := m.creationLock(key)
	lock.Lock()
	defer lock.Unlock()
	if existing, ok, err := m.ActiveEnv(ctx, key); err != nil {
		return nil, err
	} else if ok {
		return existing, nil
	}
	hostCwd, err := m.resolveTaskHostCwd(ctx, cfg, taskID)
	var created *Shell
	if err == nil {
		created, err = m.create(ctx, buildRequest{config: cfg, kind: cfg.Kind, image: image, cwd: cfg.Cwd,
			timeout: cfg.Timeout, key: key, hostCwd: hostCwd})
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Lazy %s environment init failed for task %s: %v", cfg.Kind, shortKey(key), err))
		return nil, nil
	}
	m.mu.Lock()
	m.active[key] = created
	m.last[key] = time.Now()
	m.mu.Unlock()
	slog.Info(fmt.Sprintf("%s environment lazily initialized for task %s", cfg.Kind, shortKey(key)))
	return created, nil
}

func (m *Manager) ActiveEnv(ctx context.Context, taskID string) (*Shell, bool, error) {
	lookup, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if sh, ok := m.active[lookup]; ok {
		return sh, true, nil
	}
	sh, ok := m.active[taskID]
	return sh, ok, nil
}

func (m *Manager) Environments() []*Shell {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.active))
	for k := range m.active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*Shell, 0, len(keys))
	for _, k := range keys {
		out = append(out, m.active[k])
	}
	return out
}

func (m *Manager) IsPersistent(ctx context.Context, taskID string) (bool, error) {
	sh, ok, err := m.ActiveEnv(ctx, taskID)
	if err != nil || !ok {
		return false, err
	}
	if sc, isScoped := sh.b.(sessionScoper); isScoped && sc.SessionScoped() {
		return true, nil
	}
	return sh.Persistent(), nil
}

func (m *Manager) CallInFlight(keys ...string) func() {
	held := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if k != "" && !seen[k] {
			seen[k] = true
			held = append(held, k)
		}
	}
	m.mu.Lock()
	for _, k := range held {
		m.inFlight[k]++
	}
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			now := time.Now()
			for _, k := range held {
				if left := m.inFlight[k] - 1; left > 0 {
					m.inFlight[k] = left
				} else {
					delete(m.inFlight, k)
				}
				if _, ok := m.last[k]; ok {
					m.last[k] = now
				}
			}
		})
	}
}

func (m *Manager) unregister(taskID string) *Shell {
	m.mu.Lock()
	sh := m.active[taskID]
	delete(m.active, taskID)
	delete(m.last, taskID)
	m.mu.Unlock()
	m.createMu.Lock()
	delete(m.creating, taskID)
	m.createMu.Unlock()
	return sh
}

func teardown(ctx context.Context, sh *Shell, taskID string, remove, stopIdle bool, doneMsg string) {
	var err error
	if stopIdle {
		err = sh.b.(idleStopper).StopIdle(ctx)
	} else {
		err = sh.Close(ctx, remove)
	}
	if err == nil {
		slog.Info(fmt.Sprintf(doneMsg, taskID))
		return
	}
	text := err.Error()
	if strings.Contains(text, "404") || strings.Contains(strings.ToLower(text), "not found") {
		slog.Info(fmt.Sprintf("Environment for task %s already cleaned up", taskID))
		return
	}
	slog.Warn(fmt.Sprintf("Error cleaning up environment for task %s: %v", taskID, err))
}

func (m *Manager) CleanupVM(ctx context.Context, taskID string, forceRemove bool) {
	sh := m.unregister(taskID)
	m.fileOps.ClearFileOpsCache(taskID)
	if sh == nil {
		return
	}
	teardown(ctx, sh, taskID, forceRemove, false, "Manually cleaned up environment for task: %s")
}

func (m *Manager) CleanupAll(ctx context.Context) int {
	m.fg.KillAll(false)
	m.mu.Lock()
	keys := make([]string, 0, len(m.active))
	for k := range m.active {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		m.CleanupVM(ctx, k, false)
	}
	if len(keys) > 0 {
		slog.Info(fmt.Sprintf("Cleaned %d environments", len(keys)))
	}
	return len(keys)
}

func (m *Manager) Evict(ctx context.Context, taskID string) error {
	key, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		return err
	}
	keys := []string{key}
	if taskID != "" && taskID != key {
		keys = append(keys, taskID)
	}
	var evicted []*Shell
	m.mu.Lock()
	for _, k := range keys {
		if sh, ok := m.active[k]; ok {
			evicted = append(evicted, sh)
		}
		delete(m.active, k)
		delete(m.last, k)
	}
	m.mu.Unlock()
	for _, sh := range evicted {
		if err := sh.Close(ctx, false); err != nil {
			slog.Debug("cleanup of degraded environment failed", "err", err)
		}
	}
	return nil
}

func (m *Manager) cleanupInactive(ctx context.Context, lifetimeSeconds int) {
	now := time.Now()
	m.mu.Lock()
	keys := make([]string, 0, len(m.last))
	for k := range m.last {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	for _, k := range keys {
		if m.processes.HasActiveProcesses(k) || (isWorkspaceKey(k) && m.processes.RunningInContainer(k)) {
			m.mu.Lock()
			if _, ok := m.last[k]; ok {
				m.last[k] = now
			}
			m.mu.Unlock()
		}
	}
	lifetime := time.Duration(lifetimeSeconds) * time.Second
	type stale struct {
		key string
		sh  *Shell
	}
	var stopping []stale
	m.mu.Lock()
	for k, last := range m.last {
		if now.Sub(last) > lifetime && m.inFlight[k] == 0 {
			stopping = append(stopping, stale{k, m.active[k]})
		}
	}
	m.createMu.Lock()
	for _, s := range stopping {
		delete(m.active, s.key)
		delete(m.last, s.key)
		delete(m.creating, s.key)
	}
	m.createMu.Unlock()
	m.mu.Unlock()
	for _, s := range stopping {
		if s.sh == nil {
			continue
		}
		m.fileOps.ClearFileOpsCache(s.key)
		_, canStop := s.sh.b.(idleStopper)
		teardown(ctx, s.sh, s.key, false, isWorkspaceKey(s.key) && canStop, "Cleaned up inactive environment for task: %s")
	}
}

func (m *Manager) startReaper() {
	m.reaperMu.Lock()
	defer m.reaperMu.Unlock()
	if m.reaperDone != nil {
		select {
		case <-m.reaperDone:
		default:
			return
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	m.reaperStop, m.reaperDone = stop, done
	go m.reap(stop, done)
}

func (m *Manager) reap(stop, done chan struct{}) {
	defer close(done)
	ctx := context.Background()
	for {
		cfg, err := m.settings.EnvConfig(ctx)
		if err != nil {
			slog.Warn("Error in cleanup thread", "err", err)
		} else {
			m.cleanupInactive(ctx, cfg.LifetimeSeconds)
		}
		select {
		case <-stop:
			return
		case <-time.After(reaperInterval):
		}
	}
}

func (m *Manager) stopReaper() {
	m.reaperMu.Lock()
	stop, done := m.reaperStop, m.reaperDone
	m.reaperStop = nil
	m.reaperMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(reaperJoinLimit):
	}
}

func (m *Manager) Close(ctx context.Context) error {
	m.stopReaper()
	envs := m.Environments()
	if len(envs) == 0 {
		return nil
	}
	slog.Info(fmt.Sprintf("Shutting down %d remaining sandbox(es)...", len(envs)))
	m.CleanupAll(ctx)
	for _, sh := range envs {
		if w, ok := sh.b.(cleanupWaiter); ok {
			w.WaitForCleanup(ctx, exitCleanupTimeout)
		}
	}
	return nil
}

func (m *Manager) RecordUnavailableReason(reason string, set bool) {
	m.reasonMu.Lock()
	m.reason, m.reasonSet = reason, set
	m.reasonMu.Unlock()
}

func (m *Manager) UnavailableReason() (string, bool) {
	m.reasonMu.Lock()
	defer m.reasonMu.Unlock()
	return m.reason, m.reasonSet
}

func (m *Manager) reject(reason string) bool {
	m.RecordUnavailableReason(reason, true)
	slog.Error(reason)
	return false
}
