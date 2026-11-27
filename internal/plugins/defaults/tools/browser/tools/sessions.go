package tools

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
)

const (
	// Suffix of the key of the local browser beside a cloud one, for private addresses
	localSuffix = "::local"
	// Most times an idle session's teardown may fail before it is dropped anyway
	maxCleanupFailures = 3
	// Most times a session is made again while a /browser connect published another endpoint
	staleRetries = 2
	// How often the janitor reaps what crashed processes left
	orphanEvery = 300 * time.Second
)

// A browser one task drives: where it runs, its endpoint and what it is
type session struct {
	name string
	// Id the cloud service closes it by, empty for one of this machine
	cloud string
	cdp   string
	// local, lightpanda, real_profile, sandbox, cdp_override, and what a cloud service turned on
	features map[string]bool
	// Key of the launched browser it runs on, and whether that browser serves other keys too
	launcher string
	shared   bool
	expires  time.Time
	// Made on this machine because the cloud service failed, and why
	fallback        bool
	fallbackReason  string
	fallbackService string
	// Why the user's own profile could not serve it
	realNote string
	key      string
	owner    string
	cfg      *config.Config
	// Backend the profile named when it was made: an attached endpoint, a cloud service or this machine
	backend string
}

// Every session this process keeps, when each was last used and how many of its teardowns failed
type sessions struct {
	mu       sync.Mutex
	order    []string
	byKey    map[string]*session
	activity map[string]time.Time
	owners   map[string]*config.Config
	failures map[string]int
	done     bool
	janitor  bool
	stop     chan struct{}
}

func newSessions() sessions {
	return sessions{byKey: map[string]*session{}, activity: map[string]time.Time{}, owners: map[string]*config.Config{}, failures: map[string]int{}}
}

func (s *sessions) get(key string) *session { return s.byKey[key] }

func (s *sessions) set(key string, info *session) {
	if _, ok := s.byKey[key]; !ok {
		s.order = append(s.order, key)
	}
	s.byKey[key] = info
}

func (s *sessions) del(key string) {
	if _, ok := s.byKey[key]; ok {
		delete(s.byKey, key)
		s.order = slices.DeleteFunc(s.order, func(k string) bool { return k == key })
	}
}

// Variables of every browser process: the profile's child environment without credentials, the Bot Desktop's display, loopback past
// any proxy and a temp folder short enough for Chromium's sockets
func (r *registrant) childEnv(ctx context.Context, cfg *config.Config) map[string]string {
	env := r.desktop.Env(ctx, cfg, r.terminal.ChildEnv(cfg, nil))
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		hosts := strings.Split(env[name], ",")
		for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		env[name] = strings.Trim(strings.Join(hosts, ","), ",")
	}
	env["TMPDIR"] = shortTemp()
	return env
}

// Temp folder whose paths fit a unix socket address
func shortTemp() string {
	if tmp := os.TempDir(); runtime.GOOS == "windows" || len(tmp) <= 64 {
		return tmp
	}
	return "/tmp"
}

// Whether a local session shares the Bot Desktop's screen: its display is up, or a person's hold still fences it
func (r *registrant) sharesScreen(ctx context.Context, cfg *config.Config, s *session) bool {
	if s == nil || !s.features["local"] {
		return false
	}
	if published, _ := r.desktop.Published(ctx, cfg); published["DISPLAY"] != "" {
		return true
	}
	return r.desktop.HumanHolds(cfg)
}

const takenOver = "A human took over the agent desktop while this browser command ran; its result was discarded. Tell the user what you need; " +
	"retry once they hand back."

// Runs work under the Bot Desktop's lease when the session shares its screen: refused while a person holds it, voided when one took it
// meanwhile; the refusal as the result
func (r *registrant) fenced(ctx context.Context, cfg *config.Config, s *session, work func() (any, error)) (any, map[string]any, error) {
	if !r.sharesScreen(ctx, cfg, s) {
		out, err := work()
		return out, nil, err
	}
	admitted, err := r.desktop.Admit(cfg)
	if err != nil {
		return nil, map[string]any{"success": false, "error": err.Error(), "code": "human_has_control"}, nil
	}
	out, err := work()
	if r.desktop.Epoch(cfg) != admitted {
		return nil, map[string]any{"success": false, "code": "human_has_control", "error": takenOver}, nil
	}
	return out, nil, err
}

// The fence's refusal as JSON, its code before the error once a takeover voided the result
func refusalJSON(refusal map[string]any) string {
	if msg, _ := refusal["error"].(string); strings.HasPrefix(msg, "A human took over the agent desktop while this browser command ran") {
		return orderedJSON(refusal, "success", "code", "error")
	}
	return orderedJSON(refusal, "success", "error", "code")
}

// Fresh session named prefix and ten hex digits
func newSession(prefix, cdp string, features map[string]bool) *session {
	b := make([]byte, 5)
	rand.Read(b)
	return &session{name: prefix + "_" + hex.EncodeToString(b), cdp: cdp, features: features}
}

func sidecar(key string) bool { return strings.HasSuffix(key, localSuffix) }

// Task a session key is of, without the local suffix or the profile tag
func ownerOf(key string) string {
	key = strings.TrimSuffix(key, localSuffix)
	if i := strings.LastIndex(key, "@"); i >= 0 {
		return key[:i]
	}
	return key
}

// Session of the task by its exact key, else the first not on the side it owns; nil for none
func (r *registrant) sessionFor(task string) *session {
	task = cmp.Or(task, "default")
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if s := r.sessions.get(task); s != nil {
		return s
	}
	for _, key := range r.sessions.order {
		if ownerOf(key) == task && !sidecar(key) {
			return r.sessions.byKey[key]
		}
	}
	return nil
}

// Key, launch options and whether the browser is shared, for the Bot Desktop's profile while its screen is up
func (r *registrant) screenTarget(ctx context.Context, cfg *config.Config) (string, launchOptions, bool) {
	if published, _ := r.desktop.Published(ctx, cfg); published["DISPLAY"] == "" {
		return "", launchOptions{}, false
	}
	dir := botdesktop.ProfileDir(cfg)
	return "profile:" + dir, launchOptions{exe: r.desktop.Executable(cfg), dir: dir, extra: slices.Clone(botdesktop.SharedArgs)}, true
}

// Launcher key, options and whether a persistent profile shares one Chromium across keys
func (r *registrant) launchTarget(ctx context.Context, cfg *config.Config, key string) (string, launchOptions, bool) {
	if target, opts, ok := r.screenTarget(ctx, cfg); ok {
		return target, opts, true
	}
	if dir := browser.ProfileDir(cfg); dir != "" {
		return "profile:" + dir, launchOptions{dir: dir}, true
	}
	return key, launchOptions{ephemeral: true}, false
}

// Local Chromium where the Bot Desktop policy puts it, a sandbox placement making a sandbox session
func (r *registrant) createChromium(ctx context.Context, cfg *config.Config, key string) (*session, error) {
	where, err := r.desktop.Place(ctx, cfg)
	if err != nil {
		return nil, errors.New("The browser cannot run here: " + err.Error())
	}
	if where == botdesktop.Terminal {
		return r.createSandbox(ctx, cfg, key)
	}
	if headed(cfg) {
		r.desktop.Ready(ctx, cfg)
	}
	target, opts, shared := r.launchTarget(ctx, cfg, key)
	b, failure := r.launch(ctx, cfg, target, opts)
	if b == nil {
		return nil, errors.New(cmp.Or(failure, "the local browser could not be started"))
	}
	s := newSession("h", b.cdp, map[string]bool{"local": true})
	s.launcher, s.shared = target, shared
	slog.Info(fmt.Sprintf("Created local browser session %s for %s (Chromium pid %d)", s.name, key, b.pid))
	return s, nil
}

// Lightpanda session, private networks blocked unless the browser and the terminal are both local
func (r *registrant) createLightpanda(ctx context.Context, cfg *config.Config, key string) (*session, error) {
	s := newSession("lp", "", map[string]bool{"local": true, "lightpanda": true})
	server, failure := r.launchLightpanda(ctx, cfg, s.name, !r.localBackend(cfg))
	if failure != "" {
		return nil, errors.New(failure)
	}
	s.cdp = server.cdp()
	slog.Info(fmt.Sprintf("Created Lightpanda session %s (port %d) for %s", s.name, server.port, key))
	return s, nil
}

// Local session: the user's own profile when allowed and it loads, else a fresh browser carrying why it did not
func (r *registrant) createLocal(ctx context.Context, cfg *config.Config, key string, allowReal bool) (*session, error) {
	note := ""
	if allowReal {
		cdp, failure := r.realProfileCDP(ctx, cfg)
		switch {
		case failure != "":
			slog.Warn(fmt.Sprintf("real-profile unavailable for %s, using a fresh browser: %s", key, failure))
			note = fallbackNote(failure)
		case cdp != "":
			s := newSession("rp", cdp, map[string]bool{"local": true, "real_profile": true})
			s.launcher, s.shared = realProfileKey(cfg), true
			slog.Info(fmt.Sprintf("Created real-profile local session %s for %s", s.name, key))
			return s, nil
		}
	}
	var s *session
	var err error
	if lightpanda(cfg) {
		s, err = r.createLightpanda(ctx, cfg, key)
	} else {
		s, err = r.createChromium(ctx, cfg, key)
	}
	if err != nil {
		return nil, err
	}
	s.realNote = note
	return s, nil
}

// Cloud session, a failure of the service falling back to a local one marked as such
func (r *registrant) createCloud(ctx context.Context, cfg *config.Config, key string, p *browser.Provider) (*session, error) {
	opened, err := p.Create(ctx, cfg, key)
	if err == nil && (opened == nil || opened.CDP == "" && opened.ID == "") {
		err = fmt.Errorf("Cloud provider returned invalid session: %v", opened)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Cloud provider %s failed (%s); falling back to local Chromium for %s", p.Title, err, key))
		local, localErr := r.createLocal(ctx, cfg, key, true)
		if localErr != nil {
			return nil, fmt.Errorf("Cloud provider %s failed (%s) and local fallback also failed (%s)", p.Title, err, localErr)
		}
		local.fallback, local.fallbackReason, local.fallbackService = true, err.Error(), p.Title
		return local, nil
	}
	s := &session{name: opened.Name, cloud: opened.ID, features: maps.Clone(opened.Features), expires: opened.Expires}
	if s.features == nil {
		s.features = map[string]bool{}
	}
	if opened.CDP != "" {
		s.cdp = resolveCDP(ctx, opened.CDP)
	}
	return s, nil
}

// Fresh session for the key: an attached endpoint first, then the forced local one beside a cloud browser, then the cloud service, then local
func (r *registrant) createFor(ctx context.Context, cfg *config.Config, key string, forceLocal bool) (*session, error) {
	if override := r.override(ctx, cfg); override != "" && !forceLocal {
		slog.Info("Created CDP browser session for " + key)
		return newSession("cdp", override, map[string]bool{"cdp_override": true}), nil
	}
	if forceLocal {
		return r.createLocal(ctx, cfg, key, false)
	}
	cloud, err := r.browser.Cloud(cfg)
	if err != nil {
		return nil, err
	}
	if cloud == nil {
		return r.createLocal(ctx, cfg, key, true)
	}
	return r.createCloud(ctx, cfg, key, cloud)
}

// Backend the profile names for the key now, read without the network: the attached endpoint, else the cloud service, else this machine
func (r *registrant) backendOf(cfg *config.Config, key string) string {
	if sidecar(key) {
		return "local"
	}
	if raw := r.overrideRaw(cfg); raw != "" {
		return "cdp " + raw
	}
	if cloud, err := r.browser.Cloud(cfg); err == nil && cloud != nil {
		return "cloud " + cloud.Name
	}
	return "local"
}

// Whether a local session's process is gone, or its shared launcher key now names another endpoint
func (r *registrant) dead(ctx context.Context, cfg *config.Config, s *session) bool {
	switch {
	case s.features["lightpanda"]:
		server := r.lightpanda.get(s.name)
		return server == nil || !server.alive()
	case s.features["sandbox"]:
		return !r.sandboxAlive(ctx, cfg, s)
	case s.launcher != "":
		b := r.lookup(cfg, s.launcher)
		return b == nil || b.cdp != s.cdp
	}
	return false
}

// The key's live session that has not expired, else the old one torn down and a replacement another call made meanwhile; nil for none
func (r *registrant) reuse(ctx context.Context, cfg *config.Config, key string) *session {
	r.sessions.mu.Lock()
	existing := r.sessions.get(key)
	r.sessions.mu.Unlock()
	if existing == nil {
		return nil
	}
	moved := existing.backend != r.backendOf(cfg, key)
	if !moved && (existing.expires.IsZero() || time.Now().Before(existing.expires)) && !r.dead(ctx, cfg, existing) {
		return existing
	}
	if moved {
		slog.Info("Replacing the browser session for " + key + ": the profile's browser backend changed")
	} else {
		slog.Info("Replacing expired or dead browser session for " + key)
	}
	r.cleanupOne(ctx, cfg, key)
	r.touch(cfg, key)
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if replacement := r.sessions.get(key); replacement != nil && replacement != existing {
		return replacement
	}
	return nil
}

// Session of the key, made when it has none; one made while a /browser connect published another endpoint is made again
func (r *registrant) info(ctx context.Context, cfg *config.Config, key string) (*session, error) {
	key = cmp.Or(key, "default")
	r.startJanitor(cfg)
	r.touch(cfg, key)
	if reused := r.reuse(ctx, cfg, key); reused != nil {
		return reused, nil
	}
	forceLocal := sidecar(key)
	var published *session
	for attempt := 0; attempt <= staleRetries; attempt++ {
		generation := r.live.generation(cfg.Profile())
		backend := r.backendOf(cfg, key)
		built, err := r.createFor(ctx, cfg, key, forceLocal)
		if err != nil {
			return nil, err
		}
		r.sessions.mu.Lock()
		winner := r.sessions.get(key)
		if winner == nil && (r.live.generation(cfg.Profile()) == generation || attempt == staleRetries) {
			built.key, built.owner, built.cfg, built.backend = key, ownerOf(key), cfg, backend
			r.sessions.set(key, built)
			published = built
		}
		r.sessions.mu.Unlock()
		if winner != nil {
			r.releaseBackend(ctx, cfg, built)
			return winner, nil
		}
		if published != nil {
			break
		}
		slog.Info("Browser endpoint changed during session creation for " + key + "; recreating")
		r.releaseBackend(ctx, cfg, built)
	}
	if !published.features["local"] {
		r.ensureSupervisor(ctx, cfg, key)
	}
	return published, nil
}

// Notes the key was just used, and the profile it serves the first time
func (r *registrant) touch(cfg *config.Config, key string) {
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	r.sessions.activity[key] = time.Now()
	if _, ok := r.sessions.owners[key]; !ok {
		r.sessions.owners[key] = cfg
	}
}

// Profile the key's session serves, the one given when none was noted
func (r *registrant) ownerCfg(cfg *config.Config, key string) *config.Config {
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if owner := r.sessions.owners[key]; owner != nil {
		return owner
	}
	return cfg
}

func (r *registrant) forgetKey(key string, activity, record bool) {
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if record {
		r.sessions.del(key)
	}
	if activity {
		delete(r.sessions.activity, key)
	}
	delete(r.sessions.owners, key)
	delete(r.sessions.failures, key)
}

// Whether another session than this one runs on the launcher key
func (r *registrant) launcherInUse(launcher string, s *session) bool {
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	for _, other := range r.sessions.byKey {
		if other != s && other.launcher == launcher {
			return true
		}
	}
	return false
}

// Lets the session's browser go: its cloud browser closed, its Lightpanda stopped, its Chromium stopped unless another session uses it
func (r *registrant) releaseBackend(ctx context.Context, cfg *config.Config, s *session) {
	if s.cloud != "" {
		if cloud, err := r.browser.Cloud(cfg); err == nil && cloud != nil && cloud.Close != nil {
			r.quietly("Cloud browser close", func() { cloud.Close(ctx, cfg, s.cloud) })
		}
	}
	if s.features["lightpanda"] {
		r.stopLightpanda(cfg, s.name)
	}
	if s.launcher == "" || r.launcherInUse(s.launcher, s) {
		return
	}
	switch {
	case s.features["sandbox"]:
		r.stopSandbox(ctx, cfg, s)
	case !s.features["real_profile"]:
		r.stopBrowser(cfg, s.launcher)
	}
}

// Tears down the session of the exact key, its supervisor first
func (r *registrant) cleanupOne(ctx context.Context, cfg *config.Config, key string) {
	r.supervisors.stop(key)
	r.sessions.mu.Lock()
	s := r.sessions.get(key)
	r.sessions.del(key)
	r.sessions.mu.Unlock()
	r.forgetKey(key, true, false)
	if s != nil {
		r.releaseBackend(ctx, cfg, s)
		slog.Debug("Closed browser session " + key)
	}
}

// Closes every session of the task, the local one beside it and those of every profile tag
func (r *registrant) cleanupTask(ctx context.Context, cfg *config.Config, task string) {
	task = cmp.Or(task, "default")
	var keys []string
	r.sessions.mu.Lock()
	if sidecar(task) {
		if r.sessions.get(task) != nil {
			keys = []string{task}
		}
	} else {
		for _, key := range r.sessions.order {
			if key == task || ownerOf(key) == task {
				keys = append(keys, key)
			}
		}
	}
	r.sessions.mu.Unlock()
	for _, key := range keys {
		r.cleanupOne(ctx, r.ownerCfg(cfg, key), key)
	}
	if len(keys) == 0 {
		r.forgetKey(task, true, false)
	}
}

// Closes one profile's sessions, supervisors and browser_exec connections
func (r *registrant) cleanupProfile(ctx context.Context, cfg *config.Config) {
	var keys []string
	r.sessions.mu.Lock()
	for _, key := range r.sessions.order {
		if s := r.sessions.byKey[key]; s.cfg != nil && s.cfg.Profile() == cfg.Profile() {
			keys = append(keys, key)
		}
	}
	r.sessions.mu.Unlock()
	for _, key := range keys {
		r.cleanupOne(ctx, cfg, key)
	}
	r.supervisors.stopProfile(cfg.Profile())
	r.execs.close(cfg.Profile())
}

// Closes every session under its own profile and stops everything this process launched
func (r *registrant) cleanupAll(ctx context.Context) {
	r.sessions.mu.Lock()
	keys := slices.Clone(r.sessions.order)
	r.sessions.mu.Unlock()
	for _, key := range keys {
		r.cleanupOne(ctx, r.ownerCfg(r.config, key), key)
	}
	r.supervisors.stopAll()
	r.execs.close("")
	r.stopAll()
	r.stopAllLightpanda()
}

// Reaps the Chromium and Lightpanda processes whose owner is gone, in every profile a session serves
func (r *registrant) reapOrphans() int {
	r.sessions.mu.Lock()
	var profiles []*config.Config
	for _, cfg := range r.sessions.owners {
		profiles = append(profiles, cfg)
	}
	r.sessions.mu.Unlock()
	reaped := r.reapLightpanda(r.config)
	for _, cfg := range profiles {
		if cfg.Profile() != r.config.Profile() {
			reaped += r.reapLightpanda(cfg)
		}
	}
	reaped += r.reapBrowsers(profiles)
	if reaped > 0 {
		slog.Info(fmt.Sprintf("Reaped %d orphaned browser process(es) from previous run(s)", reaped))
	}
	return reaped
}

// Closes one idle session under its owner, dropping it anyway after the third failure
func (r *registrant) cleanupIdle(ctx context.Context, key string, elapsed time.Duration) {
	owner := r.ownerCfg(r.config, key)
	slog.Info(fmt.Sprintf("Cleaning up inactive session %s (inactive for %ds)", key, int(elapsed.Seconds())))
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%v", p)
			}
		}()
		r.cleanupOne(ctx, owner, key)
		r.forgetKey(key, true, false)
		return nil
	}()
	if err == nil {
		return
	}
	r.sessions.mu.Lock()
	r.sessions.failures[key]++
	failures := r.sessions.failures[key]
	r.sessions.mu.Unlock()
	if failures < maxCleanupFailures {
		slog.Warn(fmt.Sprintf("Error cleaning up inactive session %s (attempt %d/%d): %s", key, failures, maxCleanupFailures, err))
		return
	}
	slog.Error(fmt.Sprintf("Browser cleanup failed %d times for %s; force-reaping: %s", failures, key, err))
	r.supervisors.stop(key)
	r.sessions.mu.Lock()
	s := r.sessions.get(key)
	r.sessions.mu.Unlock()
	r.forgetKey(key, true, true)
	if s != nil {
		r.releaseBackend(ctx, owner, s)
	}
}

// Closes the sessions idle past their profile's inactivity timeout, a person holding the shared screen counting as use
func (r *registrant) cleanupInactive(ctx context.Context) {
	now := time.Now()
	r.sessions.mu.Lock()
	activity := maps.Clone(r.sessions.activity)
	r.sessions.mu.Unlock()
	for key, last := range activity {
		owner := r.ownerCfg(r.config, key)
		elapsed := now.Sub(last)
		if elapsed <= inactivity(owner) {
			continue
		}
		r.sessions.mu.Lock()
		s := r.sessions.get(key)
		r.sessions.mu.Unlock()
		if r.sharesScreen(ctx, owner, s) && r.desktop.HumanHolds(owner) {
			r.touch(owner, key)
			continue
		}
		r.cleanupIdle(ctx, key, elapsed)
	}
}

// Starts the janitor once: idle sessions closed every thirty seconds, orphans reaped at its start and every five minutes
func (r *registrant) startJanitor(cfg *config.Config) {
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if r.sessions.janitor {
		return
	}
	r.sessions.janitor = true
	r.sessions.stop = make(chan struct{})
	stop := r.sessions.stop
	slog.Info(fmt.Sprintf("Started browser inactivity janitor (timeout: %ds)", int(inactivity(cfg).Seconds())))
	go func() {
		ctx := context.Background()
		lastReap := time.Time{}
		for {
			if time.Since(lastReap) >= orphanEvery {
				lastReap = time.Now()
				r.quietly("Orphan reap", func() { r.reapOrphans() })
			}
			r.quietly("Inactive browser cleanup", func() { r.cleanupInactive(ctx) })
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
}

// Runs work so one failure of a teardown never stops the rest, logging it
func (r *registrant) quietly(label string, work func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.Debug(fmt.Sprintf("%s failed: %v", label, p))
		}
	}()
	work()
}

// Closes this process's browsers and reaps the orphans of crashed processes, once, as icebath exits
func (r *registrant) emergency(ctx context.Context) {
	r.sessions.mu.Lock()
	if r.sessions.done {
		r.sessions.mu.Unlock()
		return
	}
	r.sessions.done = true
	n := len(r.sessions.byKey)
	if r.sessions.janitor {
		close(r.sessions.stop)
		r.sessions.janitor = false
	}
	r.sessions.mu.Unlock()
	if n > 0 {
		slog.Info(fmt.Sprintf("Emergency cleanup: closing %d active session(s)...", n))
	}
	r.quietly("Browser cleanup on exit", func() { r.cleanupAll(ctx) })
	r.sessions.mu.Lock()
	r.sessions.order, r.sessions.byKey, r.sessions.activity, r.sessions.owners, r.sessions.failures = nil, map[string]*session{},
		map[string]time.Time{}, map[string]*config.Config{}, map[string]int{}
	r.sessions.mu.Unlock()
	r.quietly("Orphan reap on exit", func() { r.reapOrphans() })
}
