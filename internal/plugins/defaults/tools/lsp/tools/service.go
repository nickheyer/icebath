package tools

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

// Budgets of a service
const (
	// Longest an edit check may take beyond twice its wait, for a server to install and start
	spawnCeiling = 60 * time.Second
	// Longest a release waits for the spawns under way
	releaseWait = 10 * time.Second
	// Longest the shutdown of every client takes
	shutdownWait = 10 * time.Second
	// Longest a shutdown of one broken client takes
	brokenShutdown = time.Second
)

// Server and root a client or a failure is kept by, the root empty for a server shared by every trusted root
type key struct{ server, root string }

// When a failed server and root are tried again, never when forever
type failure struct {
	until   time.Time
	forever bool
}

// A spawn under way that other checks of the root wait on
type spawning struct {
	done   chan struct{}
	client *client
}

// The language server clients of one profile and the checks that use them
type service struct {
	m        *Manager
	cfg      *config.Config
	settings settings
	customs  []*server
	ctx      context.Context
	stop     context.CancelFunc
	// Goroutines of the service that end with it
	work sync.WaitGroup

	mu        sync.Mutex
	clients   map[key]*client
	broken    map[key]failure
	spawns    map[key]*spawning
	used      map[key]time.Time
	untrusted map[key]bool
	// Git worktrees of the folders sessions of the profile work in, which trust spans
	working map[string]bool
	checks  *checks
}

func newService(m *Manager, cfg *config.Config, s settings) *service {
	ctx, stop := context.WithCancel(m.ctx)
	sv := &service{m: m, cfg: cfg, settings: s, customs: m.customs(s.entries), ctx: ctx, stop: stop, clients: map[key]*client{},
		broken: map[key]failure{}, spawns: map[key]*spawning{}, used: map[key]time.Time{}, untrusted: map[key]bool{}, working: map[string]bool{},
		checks: newChecks()}
	if _, ok := tools.TrustedFolders(cfg); !ok {
		m.logf(slog.LevelWarn, "%s must be a list of directories, e.g. ['~/code/my-app']; only the workspaces you launched icebath or opened a session "+
			"in are trusted until the key is fixed", toolsconfig.TrustedWorkspaces)
	}
	if s.excludeInvalid {
		m.logf(slog.LevelWarn, "%s must be a list of glob patterns, e.g. ['~/big-monorepo', '/srv/*/vendor']; LSP is skipped for every workspace "+
			"until the key is fixed", lspconfig.ExcludeRoots)
	}
	if s.enabled && s.idle > 0 {
		sv.work.Add(1)
		go sv.reaper()
	}
	return sv
}

// Server of a file: those the settings declare first, then the built in ones
func (s *service) serverFor(path string) *server {
	if srv := serverOf(path, s.customs); srv != nil {
		return srv
	}
	return serverOf(path, builtins)
}

// Whether a declared or a built in server takes the extension
func (s *service) handles(extension string) bool {
	extension = strings.ToLower(extension)
	for _, srv := range s.customs {
		if slices.Contains(srv.extensions, extension) {
			return true
		}
	}
	return builtinHandles(extension)
}

// Git worktree a file is checked in: that of the folder icebath works in when the file lies in it, else the file's own; false outside any
func workspace(path string) (string, bool) {
	if wd, err := os.Getwd(); err == nil {
		if root := tools.Worktree(wd); root != "" && tools.Inside(path, root) {
			return root, true
		}
	}
	if root := tools.Worktree(path); root != "" {
		return root, true
	}
	return "", false
}

// Key a file's failures are kept by, false for a file outside any worktree
func (s *service) brokenKey(srv *server, path string) (key, bool) {
	ws, ok := workspace(path)
	if !ok {
		return key{}, false
	}
	root := ws
	if r := srv.root(path, ws); r != "" {
		root = r
	}
	return key{srv.id, root}, true
}

// Notes the folder a session of the profile works in, so its worktree is trusted for every session of the process
func (s *service) note(dir string) {
	if dir == "" {
		return
	}
	root := tools.Worktree(dir)
	if root == "" {
		return
	}
	s.mu.Lock()
	s.working[root] = true
	s.mu.Unlock()
}

// Whether the root is trusted to run its own code, by the setting, where icebath started or where a session works
func (s *service) trusted(root string) bool {
	s.mu.Lock()
	working := slices.Collect(maps.Keys(s.working))
	s.mu.Unlock()
	return tools.Trusted(s.cfg, root, working...)
}

// Whether LSP checks the file: a server takes it, it lies in a worktree that is neither excluded nor untrusted for the server, and its
// server did not fail there lately
func (s *service) enabledFor(path string) bool {
	if !s.settings.enabled {
		return false
	}
	srv := s.serverFor(path)
	if srv == nil || s.settings.disabled[srv.id] {
		return false
	}
	k, ok := s.brokenKey(srv, path)
	if !ok {
		return false
	}
	if s.excluded(k.root) {
		s.m.logExcluded(srv.id, k.root, path, s.settings.excludeInvalid)
		return false
	}
	if s.untrustedDenied(srv, k.root, path, s.trusted(k.root)) {
		return false
	}
	if s.isBroken(k) {
		retryIn, retries := s.retryIn(k)
		s.m.logSkippedBroken(srv.id, k.root, path, retryIn, retries)
		return false
	}
	return true
}

// Client key of a root with the lock held: its own while a client of its own runs or starts, the shared one of a server that takes
// several trusted roots
func (s *service) liveKey(srv *server, root string, trusted bool) key {
	own := key{srv.id, root}
	if _, ok := s.clients[own]; ok {
		return own
	}
	if _, ok := s.spawns[own]; ok {
		return own
	}
	if srv.multiRoot && trusted {
		return key{srv.id, ""}
	}
	return own
}

// Whether a server that may run the project's code is kept out of a root that is not trusted
func (s *service) untrustedDenied(srv *server, root, path string, trusted bool) bool {
	if trusted || untrustedSafe[srv.id] {
		return false
	}
	s.mu.Lock()
	s.untrusted[key{srv.id, root}] = true
	s.mu.Unlock()
	s.m.logUntrusted(srv.id, root, path)
	return true
}

// Whether the root matches an exclude glob, or the setting is no list
func (s *service) excluded(root string) bool {
	if s.settings.excludeInvalid {
		return true
	}
	sep := string(filepath.Separator)
	for _, pattern := range s.settings.excludes {
		if tools.Glob(root, pattern) || tools.Glob(root, strings.TrimRight(pattern, sep)+sep+"*") {
			return true
		}
	}
	return false
}

// Whether the server failed in the root, a failure past its retry dropped for one more try
func (s *service) isBroken(k key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.broken[k]
	if !ok {
		return false
	}
	if !f.forever && !time.Now().Before(f.until) {
		delete(s.broken, k)
		return false
	}
	return true
}

func (s *service) retryIn(k key) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.broken[k]
	if !ok || f.forever {
		return 0, false
	}
	return max(0, time.Until(f.until).Seconds()), true
}

func (s *service) markBroken(k key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings.retry > 0 {
		s.broken[k] = failure{until: time.Now().Add(duration(s.settings.retry))}
	} else {
		s.broken[k] = failure{forever: true}
	}
}

// Wait a check of the file gets: the warm up budget while its server has not started, when that is longer
func (s *service) budget(path string) time.Duration {
	wait := duration(s.settings.wait)
	if s.settings.warmup <= s.settings.wait {
		return wait
	}
	srv := s.serverFor(path)
	if srv == nil {
		return wait
	}
	k, ok := s.brokenKey(srv, path)
	if !ok {
		return wait
	}
	trusted := s.trusted(k.root)
	s.mu.Lock()
	c := s.clients[s.liveKey(srv, k.root, trusted)]
	s.mu.Unlock()
	if c != nil && c.running() {
		return wait
	}
	return duration(s.settings.warmup)
}

// Marks the file's server and root failed after an error, takes its client out and says so once; the client is shut down within a second
func (s *service) breakFor(path string, cause error) {
	srv := s.serverFor(path)
	if srv == nil {
		return
	}
	k, ok := s.brokenKey(srv, path)
	if !ok {
		return
	}
	already := s.isBroken(k)
	s.markBroken(k)
	trusted := s.trusted(k.root)
	s.mu.Lock()
	live := s.liveKey(srv, k.root, trusted)
	c := s.clients[live]
	delete(s.clients, live)
	delete(s.used, live)
	s.mu.Unlock()
	if !already {
		s.m.logSpawnFailed(k.server, k.root, cause)
	}
	if c != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), brokenShutdown)
		defer cancel()
		if err := c.shutdown(ctx); err != nil {
			s.m.logf(slog.LevelDebug, "shutdown of broken LSP client failed")
		}
	}
}

// Running client of a file's root, started once while other checks of the root wait on that start; nil when none can run
func (s *service) client(ctx context.Context, path string) (*client, error) {
	srv := s.serverFor(path)
	if srv == nil {
		return nil, nil
	}
	if s.settings.disabled[srv.id] {
		s.m.logDisabled(srv.id, path, "disabled in config")
		return nil, nil
	}
	ws, ok := workspace(path)
	if !ok {
		s.m.logNoRoot(srv.id, path)
		return nil, nil
	}
	root := srv.root(path, ws)
	if root == "" {
		s.m.logDisabled(srv.id, path, "exclude marker hit (server gated off)")
		return nil, nil
	}
	trusted := s.trusted(root)
	if s.untrustedDenied(srv, root, path, trusted) || s.isBroken(key{srv.id, root}) {
		return nil, nil
	}
	s.mu.Lock()
	k := s.liveKey(srv, root, trusted)
	c := s.clients[k]
	var spawn *spawning
	owner := false
	if c != nil && c.running() {
		s.used[k] = time.Now()
	} else {
		c = nil
		if spawn = s.spawns[k]; spawn == nil {
			spawn, owner = &spawning{done: make(chan struct{})}, true
			s.spawns[k] = spawn
		}
	}
	s.mu.Unlock()
	if c != nil {
		s.m.logActive(srv.id, root)
		return s.attach(ctx, srv, c, root)
	}
	if !owner {
		select {
		case <-spawn.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if spawn.client == nil {
			return nil, nil
		}
		return s.attach(ctx, srv, spawn.client, root)
	}
	defer func() {
		s.mu.Lock()
		delete(s.spawns, k)
		s.mu.Unlock()
		close(spawn.done)
	}()
	c, err := s.start(ctx, srv, root, k.root == "" || trusted)
	if err != nil {
		return nil, err
	}
	if c == nil {
		s.markBroken(key{srv.id, root})
		return nil, nil
	}
	s.mu.Lock()
	s.clients[k] = c
	s.used[k] = time.Now()
	s.mu.Unlock()
	s.m.logActive(srv.id, root)
	spawn.client = c
	return c, nil
}

// Announces another root to a server that takes several, rather than starting another
func (s *service) attach(ctx context.Context, srv *server, c *client, root string) (*client, error) {
	if srv.multiRoot {
		if err := c.addFolder(ctx, root); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Finds the binary and starts a client, nil after saying why when either fails
func (s *service) start(ctx context.Context, srv *server, root string, trusted bool) (*client, error) {
	sp, err := srv.spawn(&build{s: s, root: root, trusted: trusted})
	if err != nil {
		return nil, err
	}
	if sp == nil {
		s.m.logUnavailable(srv.id)
		return nil, nil
	}
	c := newClient(s.m, s.cfg, launch{server: srv.id, root: root, command: sp.command, env: sp.env, node: sp.node, init: sp.init,
		seeds: sp.seeds || srv.seeds})
	if err := c.start(ctx); err != nil {
		if cancelled(err) {
			return nil, err
		}
		s.m.logSpawnFailed(srv.id, root, err)
		return nil, nil
	}
	return c, nil
}

// Keeps the client counted as just used, while it is still one of the service's
func (s *service) touch(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, live := range s.clients {
		if live == c {
			s.used[k] = time.Now()
		}
	}
}

// Stops idle clients every so often, a failed sweep never ending it
func (s *service) reaper() {
	defer s.work.Done()
	interval := duration(min(60, s.settings.idle))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if err := s.reap(); err != nil && !cancelled(err) {
			s.m.logf(slog.LevelDebug, "LSP idle reaper sweep error: %s", err)
		}
	}
}

// Stops the clients idle past the idle time and those whose every root is gone
func (s *service) reap() error {
	cutoff := time.Now().Add(-duration(s.settings.idle))
	s.mu.Lock()
	var idle []*client
	for k, c := range s.clients {
		if last, ok := s.used[k]; !ok || last.Before(cutoff) {
			idle = append(idle, c)
			delete(s.clients, k)
			delete(s.used, k)
		}
	}
	s.mu.Unlock()
	if len(idle) > 0 {
		s.m.logReaped(idle, s.settings.idle)
		s.shutdownAll(idle)
	}
	_, err := s.detach(func(dir string) bool {
		st, err := os.Stat(dir)
		return err != nil || !st.IsDir()
	}, "workspace root deleted")
	return err
}

// Shuts clients down at once, their own failures let go
func (s *service) shutdownAll(clients []*client) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), shutdownWait)
	defer cancel()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(func() { c.shutdown(ctx) })
	}
	wg.Wait()
}

// Stops the clients of a worktree about to be removed, and of every root under it; how many
func (s *service) release(dir string) int {
	root, err := filepath.Abs(dir)
	if err != nil {
		return 0
	}
	under := func(path string) bool {
		return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
	}
	s.mu.Lock()
	var pending []chan struct{}
	for _, spawn := range s.spawns {
		pending = append(pending, spawn.done)
	}
	for k := range s.broken {
		if under(k.root) {
			delete(s.broken, k)
		}
	}
	s.mu.Unlock()
	deadline := time.Now().Add(releaseWait)
	for _, done := range pending {
		if !wait(done, max(0, time.Until(deadline))) {
			break
		}
	}
	n, err := s.detach(under, "workspace released")
	if err != nil {
		s.m.logf(slog.LevelDebug, "LSP release of %s failed: %s", root, err)
		return 0
	}
	return n
}

// Stops the clients whose every folder is gone and takes the gone folders off the others
func (s *service) detach(gone func(dir string) bool, reason string) (int, error) {
	s.mu.Lock()
	var dead []*client
	var trims []*client
	for k, c := range s.clients {
		_, folders, _ := c.snapshot()
		if all := !slices.ContainsFunc(folders, func(f string) bool { return !gone(f) }); all {
			dead = append(dead, c)
			delete(s.clients, k)
			delete(s.used, k)
			continue
		}
		trims = append(trims, c)
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), shutdownWait)
	defer cancel()
	for _, c := range trims {
		_, folders, _ := c.snapshot()
		for _, f := range folders {
			if !gone(f) {
				continue
			}
			if err := c.removeFolder(ctx, f); err != nil {
				if cancelled(err) {
					return 0, err
				}
				s.m.logf(slog.LevelDebug, "LSP folder removal for %s failed: %s", f, err)
			}
		}
	}
	if len(dead) > 0 {
		s.m.logReleased(dead, reason)
		s.shutdownAll(dead)
	}
	return len(dead), nil
}

// Stops the reaper and every check, then shuts every client down
func (s *service) shutdown() {
	s.stop()
	s.mu.Lock()
	clients := slices.Collect(maps.Values(s.clients))
	s.clients, s.broken, s.used = map[key]*client{}, map[key]failure{}, map[key]time.Time{}
	s.mu.Unlock()
	s.shutdownAll(clients)
	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	if !wait(done, 2*time.Second) {
		s.m.logf(slog.LevelDebug, "LSP shutdown left checks running")
	}
}
