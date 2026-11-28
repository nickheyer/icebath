// Language servers that check the files the agent writes: the servers icebath knows and those the settings declare, found, installed and
// started per workspace root, the diagnostics an edit brought in handed back with the write or, when they come late, with what the
// session does next
package tools

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/worktrees"
)

// Language servers of every profile, with what they share: installs, announcements and the notes of sessions
type Manager struct {
	ctx      context.Context
	stop     context.CancelFunc
	terminal *terminal.Terminal
	packages *packages.Packages
	announce announcer
	installs installs
	notes    *notes

	mu       sync.Mutex
	services map[string]*service
}

// Registers the language servers with the hosts they join: the checks of what the file tools write, the profiles and worktrees whose
// servers stop with them, a settings section for each server, and the guard every write of the servers goes through
func Register(cfg *config.Config, registry *tools.Registry, profiles *profile.Registry, stores *sessions.Registry, trees *worktrees.Registry,
	host *terminal.Terminal, pm *packages.Packages) *Manager {
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{ctx: ctx, stop: stop, terminal: host, packages: pm, notes: newNotes(), services: map[string]*service{},
		announce: announcer{warned: map[string]bool{}}, installs: installs{locks: map[string]*sync.Mutex{}, results: map[string]string{}}}
	for i := range m.announce.said {
		m.announce.said[i] = map[[2]string]bool{}
	}
	registry.Register(m)
	profiles.Register(m)
	trees.Register(m)
	cfg.Register(sections{})
	guard(cfg, m)
	stores.OnRelease(func(_ context.Context, session string) { m.notes.drop(session) })
	return m
}

// Language servers give the agent no tools of their own: they check what the file tools write
func (m *Manager) Tools() []tools.Def { return nil }

// Service of the profile, made again once its settings changed, the one before shut down
func (m *Manager) service(cfg *config.Config) *service {
	cfg = cfg.In(cfg.Profile())
	s := m.read(cfg)
	m.mu.Lock()
	current := m.services[cfg.Profile()]
	if current != nil && current.settings.same(s) {
		m.mu.Unlock()
		return current
	}
	fresh := newService(m, cfg, s)
	m.services[cfg.Profile()] = fresh
	m.mu.Unlock()
	if current != nil {
		go current.shutdown()
	}
	return fresh
}

// Service of the profile when language servers check its files, nil when they are off
func (m *Manager) active(cfg *config.Config) *service {
	if s := m.service(cfg); s.settings.enabled {
		return s
	}
	return nil
}

// Whether a server takes files of the extension, so a write keeps the text it replaces; the built in servers answer while checking is off
func (m *Manager) Handles(cfg *config.Config, extension string) bool {
	if s := m.active(cfg); s != nil {
		return s.handles(extension)
	}
	return builtinHandles(strings.ToLower(extension))
}

// Whether a server checks the file
func (m *Manager) Checks(cfg *config.Config, path string) bool {
	s := m.active(cfg)
	return s != nil && s.enabledFor(path)
}

// Diagnostics the edit brought in as a block the model reads; empty when there are none, or when the verdict comes after the write's
// budget, which then goes to the owner's notes
func (m *Manager) Check(_ context.Context, cfg *config.Config, path string, before, after *string, owner string, started time.Time) string {
	s := m.active(cfg)
	if s == nil || !s.enabledFor(path) {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	var carry []carried
	if older := m.notes.take(owner, abs); older != nil {
		carry = []carried{*older}
	}
	f := s.startCheck(abs, before, after, owner, carry)
	if f == nil {
		m.notes.restore(owner, abs, carry)
		return ""
	}
	res, ready, err := f.wait(inlineWait(started))
	if err != nil {
		m.notes.restore(owner, abs, carry)
		return ""
	}
	if !ready {
		if owner != "" {
			settle := m.notes.track(owner)
			f.then(func(res *result, cancelled bool, err error) {
				defer settle()
				m.late(owner, abs, res, cancelled, err, carry)
			})
		}
		return ""
	}
	if res.superseded {
		return ""
	}
	if !res.verdict {
		m.notes.restore(owner, abs, res.carry)
		return ""
	}
	return note(path, res.diags, "LSP diagnostics introduced by this edit:\n")
}

// Keeps a verdict that came after its write returned for the owner; a superseded check keeps nothing, a failed one hands its notes back
func (m *Manager) late(owner, abs string, res *result, cancelled bool, err error, carry []carried) {
	if err != nil || cancelled || res == nil {
		m.notes.restore(owner, abs, carry)
		return
	}
	if res.superseded {
		return
	}
	if !res.verdict {
		m.notes.restore(owner, abs, res.carry)
		return
	}
	text := note(abs, res.diags, "LSP diagnostics introduced by your earlier edit of "+abs+" (the language server answered after that tool result "+
		"returned):\n")
	if text != "" {
		m.notes.post(owner, abs, text, res.before)
	}
}

// Late verdicts of the session's edits, after waiting until the deadline for its checks still running
func (m *Manager) Notes(ctx context.Context, session string, deadline time.Time) []string {
	return m.notes.settle(ctx, session, deadline)
}

// A call is about to run: the folder its session works in is trusted for the profile's servers, as where icebath started is, unless the
// session is a scheduled job, which no one points at a folder
func (m *Manager) Dispatched(call *tools.Call) {
	env := call.Env
	if env == nil || env.Config == nil || env.Cwd == "" || tools.Scheduled(env.Platform) {
		return
	}
	if s := m.active(env.Config); s != nil {
		s.note(env.Cwd)
	}
}

// Stops the servers of a deleted profile
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	s := m.services[name]
	delete(m.services, name)
	m.mu.Unlock()
	if s != nil {
		s.shutdown()
	}
	return nil
}

// Stops the servers of a profile before its folder moves to a new name, where they start again as files are next checked
func (m *Manager) Stop(name string) error {
	return m.Delete(name)
}

// What language servers add to the upkeep of worktrees: they let go of a tree before it is removed
func (m *Manager) Hooks() worktrees.Hooks {
	return worktrees.Hooks{Release: func(tree string) { m.Release(tree) }}
}

// Stops the servers of a worktree about to be removed, and of every root under it, in every profile; how many
func (m *Manager) Release(tree string) int {
	released := 0
	for _, s := range m.all() {
		released += s.release(tree)
	}
	return released
}

func (m *Manager) all() []*service {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Values(m.services))
}

// Stops every server of every profile
func (m *Manager) Restart() {
	m.mu.Lock()
	services := m.services
	m.services = map[string]*service{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range services {
		wg.Go(s.shutdown)
	}
	wg.Wait()
}

// Stops every server as icebath ends, and what still installs
func (m *Manager) Close() error {
	m.Restart()
	m.stop()
	return nil
}
