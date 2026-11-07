package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Janitor goroutine state, running cleared to stop it within one tick
var (
	janitorMu      sync.Mutex
	janitorRunning bool
	janitorStop    chan struct{}
	janitorDone    chan struct{}
	janitorTick    = time.Second
	orphanReapRuns = func(env *tools.Env) int { return reapOrphanedBrowserSessions(env) }
	janitorJoin    = 5 * time.Second
	// Teardown collaborators the hermes lifecycle tests patch
	sessionStopBrowser     = func(_ *tools.Env, key string) { stopBrowser(key) }
	lifecycleCleanupAll    = func(env *tools.Env) { cleanupAllBrowsers(env) }
	lifecycleCleanupSingle = func(env *tools.Env, key string) error { return cleanupSingleSession(env, key) }
	processEnvOnce         = sync.OnceValue(func() *tools.Env {
		h := home.Process()
		tree, err := config.Load(h, nil)
		if err != nil {
			tree = config.Tree{}
		}
		return &tools.Env{Home: h, Config: tree}
	})
)

// Process profile env the janitor and the exit sweep run under, as the Python thread runs unscoped
func processBrowserEnv() *tools.Env { return processEnvOnce() }

// Provider expiry as epoch seconds, false when absent or malformed
func sessionExpiryTimestamp(log *slog.Logger, info map[string]any) (float64, bool) {
	value := info["expires_at"]
	switch v := value.(type) {
	case bool:
		return 0, false
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case string:
		normalized := strings.TrimSpace(v)
		if normalized == "" {
			return 0, false
		}
		if strings.HasSuffix(normalized, "Z") || strings.HasSuffix(normalized, "z") {
			normalized = normalized[:len(normalized)-1] + "+00:00"
		}
		t, aware, err := pydatetime.ParseISOFormat(normalized)
		if err != nil {
			log.Warn("Ignoring invalid cloud browser session expiry timestamp")
			return 0, false
		}
		if !aware {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
		}
		return float64(t.Unix()) + float64(t.Nanosecond())/1e9, true
	}
	return 0, false
}

// Whether a cached session crossed its provider deadline
func sessionHasExpired(log *slog.Logger, info map[string]any, now *float64) bool {
	expiresAt, ok := sessionExpiryTimestamp(log, info)
	if !ok {
		return false
	}
	current := sessionNow()
	if now != nil {
		current = *now
	}
	return current >= expiresAt
}

// Runs fn and logs any returned or raised error so one teardown failure never stops the rest
func bestEffort(log *slog.Logger, label string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			log.Debug(fmt.Sprintf("%s failed: %v", label, r))
		}
	}()
	if err := fn(); err != nil {
		log.Debug(fmt.Sprintf("%s failed: %s", label, err))
	}
}

// Touches the key's activity and records its owning profile env on first sight
func updateSessionActivity(env *tools.Env, key string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	sessionLastActivity.Set(key, sessionNow())
	if !sessionOwnerEnvs.Has(key) {
		if env == nil {
			env = processBrowserEnv()
		}
		sessionOwnerEnvs.Set(key, env)
	}
}

// Profile env owning the key's session, the caller's env when none was recorded
func sessionOwnerEnv(env *tools.Env, key string) *tools.Env {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if owner, ok := sessionOwnerEnvs.Get(key); ok {
		return owner
	}
	return env
}

func forgetSessionTracking(key string, activity, session bool) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if session {
		activeSessions.Delete(key)
	}
	if activity {
		sessionLastActivity.Delete(key)
	}
	sessionOwnerEnvs.Delete(key)
	delete(cleanupFailures, key)
}

func launcherKeyInUse(launcherKey string, info map[string]any) bool {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	for _, other := range activeSessions.All() {
		if !sameSession(other, info) && pyrepr.Equal(other["launcher_key"], launcherKey) {
			return true
		}
	}
	return false
}

// Closes the session's cloud browser through the profile's provider, a provider error only logged
func closeCloudSession(env *tools.Env, info map[string]any) error {
	if !pyrepr.Truthy(info["bb_session_id"]) {
		return nil
	}
	p, err := sessionCloudProvider(env)
	if err != nil || p == nil {
		return err
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				cfgOf(env).logger().Warn(fmt.Sprintf("Could not close cloud browser session: %v", r))
			}
		}()
		p.CloseSession(env, pyrepr.Str(info["bb_session_id"]))
	}()
	return nil
}

// Releases the record's browser by object, a shared launcher key another live session uses kept running
func releaseSessionBackend(env *tools.Env, key string, info map[string]any) error {
	if err := closeCloudSession(env, info); err != nil {
		return err
	}
	features := featuresOf(info)
	if pyrepr.Truthy(features["lightpanda"]) {
		name, _ := info["session_name"].(string)
		stopLightpanda(env, name)
	}
	launcherKey, _ := info["launcher_key"].(string)
	if !pyrepr.Truthy(info["launcher_key"]) || launcherKeyInUse(launcherKey, info) {
		return nil
	}
	if pyrepr.Truthy(features["sandbox"]) {
		stopSandboxBrowser(env, info)
	} else if !pyrepr.Truthy(features["real_profile"]) {
		sessionStopBrowser(env, launcherKey)
	}
	return nil
}

// Tears down one session by its exact key, its CDP supervisor first
func cleanupSingleSession(env *tools.Env, key string) error {
	sessionStopSupervisor(env, key)
	sessionsMu.Lock()
	info, ok := activeSessions.Get(key)
	activeSessions.Delete(key)
	sessionsMu.Unlock()
	forgetSessionTracking(key, true, false)
	if ok && len(info) > 0 {
		if err := releaseSessionBackend(env, key, info); err != nil {
			return err
		}
		cfgOf(env).logger().Debug("Closed browser session " + key)
	}
	return nil
}

// Closes every session serving the task, its local sidecar and its served profile tagged keys
func cleanupBrowser(env *tools.Env, taskID string) error {
	task := taskID
	if task == "" {
		task = "default"
	}
	var keys []string
	sessionsMu.Lock()
	if isLocalSidecarKey(task) {
		if activeSessions.Has(task) {
			keys = []string{task}
		}
	} else {
		for key := range activeSessions.All() {
			if key == task || ownerTaskID(key) == task {
				keys = append(keys, key)
			}
		}
	}
	sessionsMu.Unlock()
	for _, key := range keys {
		if err := cleanupSingleSession(env, key); err != nil {
			return err
		}
	}
	if len(keys) == 0 {
		forgetSessionTracking(task, true, false)
	}
	return nil
}

// Closes one profile's sessions, supervisors and harness daemons and drops its cached provider
func cleanupProfileBrowsers(env *tools.Env, homeKey string) error {
	var keys []string
	sessionsMu.Lock()
	for key, info := range activeSessions.All() {
		if pyrepr.Equal(info["home_key"], homeKey) {
			keys = append(keys, key)
		}
	}
	sessionsMu.Unlock()
	for _, key := range keys {
		if err := cleanupSingleSession(env, key); err != nil {
			return err
		}
	}
	log := cfgOf(env).logger()
	bestEffort(log, "CDP supervisor stop", func() error { supervisorStopHome(homeKey); return nil })
	bestEffort(log, "Browser Use harness daemon stop", func() error { stopHarnessDaemons(homeKey); return nil })
	resetCaches(homeKey)
	return nil
}

// Closes every session under its owning profile, stops everything this process launched and resets caches
func cleanupAllBrowsers(env *tools.Env) {
	sessionsMu.Lock()
	keys := activeSessions.Keys()
	sessionsMu.Unlock()
	log := cfgOf(env).logger()
	for _, key := range keys {
		bestEffort(log, "browser cleanup for "+key, func() error { return cleanupSingleSession(sessionOwnerEnv(env, key), key) })
	}
	bestEffort(log, "CDP supervisor stop", func() error { supervisorStopAll(); return nil })
	bestEffort(log, "Browser Use harness daemon stop", func() error { stopHarnessDaemons(""); return nil })
	bestEffort(log, "Chromium stop", func() error { stopAllBrowsers(); return nil })
	bestEffort(log, "Lightpanda stop", func() error { stopAllLightpanda(env); return nil })
	resetCaches("")
}

// Exit sweep closing this process's browsers then reaping orphans of crashed processes, run once
func emergencyCleanupAllSessions() {
	env := processBrowserEnv()
	log := cfgOf(env).logger()
	sessionsMu.Lock()
	if cleanupDone {
		sessionsMu.Unlock()
		return
	}
	cleanupDone = true
	n := activeSessions.Len()
	sessionsMu.Unlock()
	if n > 0 {
		log.Info(fmt.Sprintf("Emergency cleanup: closing %d active session(s)...", n))
	}
	bestEffort(log, "Browser cleanup on exit", func() error { lifecycleCleanupAll(env); return nil })
	sessionsMu.Lock()
	activeSessions.Clear()
	sessionLastActivity.Clear()
	sessionOwnerEnvs.Clear()
	clear(cleanupFailures)
	sessionsMu.Unlock()
	bestEffort(log, "Orphan reap on exit", func() error { orphanReapRuns(env); return nil })
}

// Stops Chromium and lightpanda serve processes whose owner is gone, Chromium in every profile a session serves
func reapOrphanedBrowserSessions(env *tools.Env) int {
	sessionsMu.Lock()
	var homes []string
	for _, owner := range sessionOwnerEnvs.Values() {
		homes = append(homes, cfgOf(owner).homeDir())
	}
	sessionsMu.Unlock()
	log := cfgOf(env).logger()
	reaped := 0
	n, err := reapOrphanedLightpanda(env)
	var oserr *fs.PathError
	if err != nil && errors.As(err, &oserr) {
		log.Debug(fmt.Sprintf("orphan reap (lightpanda) failed: %s", err))
	} else if err != nil {
		panic(err)
	} else {
		reaped += n
	}
	reaped += reapOrphanedBrowsers(homes)
	if reaped > 0 {
		log.Info(fmt.Sprintf("Reaped %d orphaned browser process(es) from previous run(s)", reaped))
	}
	return reaped
}

func humanHoldsSharedBrowserKey(env *tools.Env, key string) bool {
	sessionsMu.Lock()
	info, ok := activeSessions.Get(key)
	sessionsMu.Unlock()
	return ok && len(info) > 0 && humanHoldsSharedBrowser(env, info)
}

// Janitor last resort, the record dropped first and its backend released
func forceReap(env *tools.Env, key string) error {
	sessionStopSupervisor(env, key)
	sessionsMu.Lock()
	info, ok := activeSessions.Get(key)
	sessionsMu.Unlock()
	forgetSessionTracking(key, true, true)
	if ok && len(info) > 0 {
		return releaseSessionBackend(env, key, info)
	}
	return nil
}

// Runs fn turning a raised panic into its error as an except clause sees it
func catchRaised(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return fn()
}

// Closes one idle session under its owner, counting failures and force reaping after the third
func cleanupOneInactive(key string, elapsed int) {
	owner := sessionOwnerEnv(processBrowserEnv(), key)
	log := cfgOf(owner).logger()
	log.Info(fmt.Sprintf("Cleaning up inactive session %s (inactive for %ds)", key, elapsed))
	err := catchRaised(func() error {
		if err := lifecycleCleanupSingle(owner, key); err != nil {
			return err
		}
		forgetSessionTracking(key, true, false)
		return nil
	})
	if err == nil {
		return
	}
	sessionsMu.Lock()
	cleanupFailures[key]++
	failures := cleanupFailures[key]
	sessionsMu.Unlock()
	if failures < maxInactivityCleanupFailures {
		log.Warn(fmt.Sprintf("Error cleaning up inactive session %s (attempt %d/%d): %s", key, failures, maxInactivityCleanupFailures, err))
		return
	}
	log.Error(fmt.Sprintf("Browser cleanup failed %d times for %s; force-reaping: %s", failures, key, err))
	defer forgetSessionTracking(key, false, false)
	if err := forceReap(owner, key); err != nil {
		panic(err)
	}
}

// Closes sessions idle past the inactivity timeout, a human holding the shared screen counting as activity
func cleanupInactiveSessions() {
	now := sessionNow()
	limit := float64(sessionInactivity())
	type idleKey struct {
		key     string
		elapsed int
	}
	var idle []idleKey
	sessionsMu.Lock()
	for key, last := range sessionLastActivity.All() {
		if now-last > limit {
			idle = append(idle, idleKey{key, int(now - last)})
		}
	}
	sessionsMu.Unlock()
	for _, it := range idle {
		owner := sessionOwnerEnv(processBrowserEnv(), it.key)
		if humanHoldsSharedBrowserKey(owner, it.key) {
			updateSessionActivity(owner, it.key)
			continue
		}
		cleanupOneInactive(it.key, it.elapsed)
	}
}

// Every thirty ticks closes idle sessions, reaping orphans at start and every orphan reap interval
func janitor(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	env := processBrowserEnv()
	log := cfgOf(env).logger()
	reapEvery := int(math.Max(1, math.RoundToEven(float64(browserOrphanReapInterval)/30)))
	for cycle := 0; ; cycle++ {
		select {
		case <-stop:
			return
		default:
		}
		if cycle%reapEvery == 0 {
			bestEffort(log, "Orphan reap", func() error { orphanReapRuns(env); return nil })
		}
		bestEffort(log, "Inactive browser cleanup", func() error { cleanupInactiveSessions(); return nil })
		for range 30 {
			select {
			case <-stop:
				return
			case <-time.After(janitorTick):
			}
		}
	}
}

// Starts the process wide inactivity janitor unless one is running
func startCleanupThread(env *tools.Env) {
	janitorMu.Lock()
	defer janitorMu.Unlock()
	if janitorRunning {
		return
	}
	janitorRunning = true
	janitorStop, janitorDone = make(chan struct{}), make(chan struct{})
	go janitor(janitorStop, janitorDone)
	cfgOf(env).logger().Info(fmt.Sprintf("Started browser inactivity janitor (timeout: %ds)", sessionInactivity()))
}

// Stops the janitor and waits up to five seconds for it to finish
func stopCleanupThread() {
	janitorMu.Lock()
	if !janitorRunning {
		janitorMu.Unlock()
		return
	}
	janitorRunning = false
	close(janitorStop)
	done := janitorDone
	janitorMu.Unlock()
	select {
	case <-done:
	case <-time.After(janitorJoin):
	}
}
