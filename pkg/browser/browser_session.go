package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	localSuffix                  = "::local"
	browserOrphanReapInterval    = 300
	maxInactivityCleanupFailures = 3
	staleEndpointRetries         = 2
	botDesktopPlacementTerminal  = "terminal"
)

// Process wide session tables, every one guarded by sessionsMu as the Python cleanup lock guards them
var (
	sessionsMu          sync.Mutex
	activeSessions      = pydict.New[string, map[string]any]()
	sessionLastActivity = pydict.New[string, float64]()
	sessionOwnerEnvs    = pydict.New[string, *tools.Env]()
	cleanupFailures     = map[string]int{}
	cleanupDone         bool
	sessionNow          = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	sessionInactivity   = sync.OnceValue(func() int { return getSessionInactivityTimeout(processBrowserEnv()) })
)

// Collaborators of session creation and teardown, the points hermes tests patch
var (
	sessionLaunchChromium   = launchChromium
	sessionCloudProvider    = getCloudProvider
	sessionStartCleanup     = startCleanupThread
	sessionEnsureSupervisor = ensureCDPSupervisor
	sessionStopSupervisor   = stopCDPSupervisor
	sessionCDPOverride      = func(env *tools.Env) string { return cfgOf(env).cdpOverride(context.Background()) }
)

// Idle seconds before the janitor reaps a session, config first and BROWSER_INACTIVITY_TIMEOUT as the legacy fallback
func getSessionInactivityTimeout(env *tools.Env) int {
	return cfgOf(env).inactivityTimeout()
}

// Child env of every browser process, the served profile without credentials plus the desktop DISPLAY, loopback NO_PROXY and a short TMPDIR
func buildBrowserEnv(env *tools.Env) map[string]string {
	served, err := tools.ServedProfileChildEnv(context.Background(), env, nil, "", false)
	if err != nil {
		panic(err)
	}
	out := provider.AddLoopbackNoProxy(botDesktopDesktopEnv(env, served))
	tmp, err := home.SocketSafeTmpdir()
	if err != nil {
		panic(err)
	}
	out["TMPDIR"] = tmp
	return out
}

func featuresOf(info map[string]any) map[string]any {
	if f, ok := info["features"].(map[string]any); ok {
		return f
	}
	return map[string]any{}
}

func sameSession(a, b map[string]any) bool {
	return a != nil && b != nil && reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// Local sessions share the Bot Desktop screen while its DISPLAY is up or a human lease still fences it
func sharesBotDesktopBrowser(env *tools.Env, info map[string]any) bool {
	if !pyrepr.Truthy(featuresOf(info)["local"]) {
		return false
	}
	return botDesktopPublishedEnv(env)["DISPLAY"] != "" || botDesktopHumanHolds(env)
}

// A human holding the lease over the shared browser, which the janitor counts as activity
func humanHoldsSharedBrowser(env *tools.Env, info map[string]any) bool {
	if !sharesBotDesktopBrowser(env, info) {
		return false
	}
	return botDesktopHumanHolds(env)
}

// Runs fn under the Bot Desktop lease fence, refusing or voiding the result while a human holds the screen
func runFenced(env *tools.Env, info map[string]any, fn func() map[string]any) map[string]any {
	if !sharesBotDesktopBrowser(env, info) {
		return fn()
	}
	admitted, err := botDesktopAssertAgentMayAct(env)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error(), "code": "human_has_control"}
	}
	result := fn()
	if botDesktopLeaseEpoch(env) != admitted {
		return map[string]any{"success": false, "code": "human_has_control",
			"error": "A human took over the bot's screen while this browser command ran; its result was " +
				"discarded. Tell the user what you need; retry once they hand back."}
	}
	return result
}

// Brings up the Bot Desktop screen before a headed Chromium starts
func ensureScreenForHeadedChromium(env *tools.Env) {
	if cfgOf(env).headed() {
		botDesktopEnsureStartedForTool(env)
	}
}

// Fresh session record named prefix_ten hex chars
func sessionRecord(prefix string, cdpURL any, features map[string]any) map[string]any {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return map[string]any{"session_name": prefix + "_" + hex.EncodeToString(b[:]), "bb_session_id": nil,
		"cdp_url": cdpURL, "features": features}
}

func isLocalSidecarKey(key string) bool { return strings.HasSuffix(key, localSuffix) }

// Task a session key belongs to, without the local sidecar suffix or the served profile home tag
func ownerTaskID(key string) string {
	if isLocalSidecarKey(key) {
		key = key[:len(key)-len(localSuffix)]
	}
	if i := strings.LastIndex(key, "@"); i >= 0 {
		return key[:i]
	}
	return key
}

// Real profile fallback note on the key's existing session, never creating one
func realProfileNote(key string) any {
	if key == "" {
		key = "default"
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	info, _ := activeSessions.Get(key)
	return info["real_profile_unavailable"]
}

// Session serving the task by exact key, else the first non sidecar session it owns, else empty
func sessionForTask(taskID string) map[string]any {
	task := taskID
	if task == "" {
		task = "default"
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if info, ok := activeSessions.Get(task); ok {
		return info
	}
	for key, info := range activeSessions.All() {
		if ownerTaskID(key) == task && !isLocalSidecarKey(key) {
			return info
		}
	}
	return map[string]any{}
}

// Shared Bot Desktop profile launch while its screen is up, the dock and the agent driving one browser
func botDesktopTarget(env *tools.Env) (string, LaunchOptions, bool) {
	if botDesktopPublishedEnv(env)["DISPLAY"] == "" {
		return "", LaunchOptions{}, false
	}
	udd := botDesktopProfileDir(env)
	return "profile:" + udd, LaunchOptions{Executable: botDesktopExecutable(env), UserDataDir: udd, Ephemeral: false,
		ExtraArgs: botDesktopSharedProfileArgs()}, true
}

// Launcher key, options and whether a persistent profile shares one Chromium across cache keys
func launchTarget(env *tools.Env, cacheKey string) (string, LaunchOptions, bool) {
	if key, opts, ok := botDesktopTarget(env); ok {
		return key, opts, true
	}
	if udd := cfgOf(env).userDataDir(); udd != "" {
		return "profile:" + udd, LaunchOptions{UserDataDir: udd, Ephemeral: false}, true
	}
	return cacheKey, LaunchOptions{Ephemeral: true}, false
}

// Local Chromium session placed by the Bot Desktop policy, a terminal placement building a sandbox session
func createChromiumSession(ctx context.Context, env *tools.Env, cacheKey string) (map[string]any, error) {
	where, err := botDesktopToolPlacement(env)
	if err != nil {
		return nil, runtimeError("The browser cannot run here: " + err.Error())
	}
	if where == botDesktopPlacementTerminal {
		return createSandboxSession(env, cacheKey)
	}
	ensureScreenForHeadedChromium(env)
	launcherKey, opts, shared := launchTarget(env, cacheKey)
	b, msg := sessionLaunchChromium(tools.WithEnvHome(ctx, env), env, launcherKey, opts)
	if b == nil {
		if msg == "" {
			msg = "the local browser could not be started"
		}
		return nil, runtimeError(msg)
	}
	info := sessionRecord("h", b.CDPURL, map[string]any{"local": true})
	info["launcher_key"], info["shared_profile"] = launcherKey, shared
	pid := any(nil)
	if b.PID != nil {
		pid = *b.PID
	}
	cfgOf(env).logger().Info(fmt.Sprintf("Created local browser session %s for %s (Chromium pid %s)", info["session_name"], cacheKey, pyrepr.Str(pid)))
	return info, nil
}

// Lightpanda serve session, private networks blocked unless browser and terminal are both local
func createLightpandaSession(ctx context.Context, env *tools.Env, cacheKey string) (map[string]any, error) {
	info := sessionRecord("lp", nil, map[string]any{"local": true, "lightpanda": true})
	server, msg := launchLightpanda(tools.WithEnvHome(ctx, env), env, info["session_name"].(string), !isLocalBackend(env))
	if msg != "" {
		return nil, runtimeError(msg)
	}
	info["cdp_url"] = server.CDPURL()
	cfgOf(env).logger().Info(fmt.Sprintf("Created Lightpanda session %s (port %d) for %s", info["session_name"], server.Port, cacheKey))
	return info, nil
}

// Local session deciding real profile versus a fresh browser, the fresh one carrying the fallback note
func createLocalSession(ctx context.Context, env *tools.Env, cacheKey string, allowRealProfile bool) (map[string]any, error) {
	var fallbackNote any
	if allowRealProfile {
		cdpURL, msg := realProfileCDP(env)
		if msg != "" {
			cfgOf(env).logger().Warn(fmt.Sprintf("real-profile unavailable for %s, using a fresh browser: %s", cacheKey, msg))
			fallbackNote = realProfileFallbackNote(msg)
		} else if cdpURL != "" {
			info := sessionRecord("rp", cdpURL, map[string]any{"local": true, "real_profile": true})
			info["launcher_key"], info["shared_profile"] = realProfileKey(env), true
			cfgOf(env).logger().Info(fmt.Sprintf("Created real-profile local session %s for %s", info["session_name"], cacheKey))
			return info, nil
		}
	}
	var info map[string]any
	var err error
	if usingLightpandaEngine(env) {
		info, err = createLightpandaSession(tools.WithEnvHome(ctx, env), env, cacheKey)
	} else {
		info, err = createChromiumSession(tools.WithEnvHome(ctx, env), env, cacheKey)
	}
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(fallbackNote) {
		info["real_profile_unavailable"] = fallbackNote
	}
	return info, nil
}

func providerClassName(p BrowserProvider) string {
	return reflect.Indirect(reflect.ValueOf(p)).Type().Name()
}

// Provider create that turns a raised panic into the error Python's except clause sees
func providerCreate(env *tools.Env, p BrowserProvider, cacheKey string) (info map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	info, err = p.CreateSession(env, cacheKey)
	if err == nil && len(info) == 0 {
		err = valueError("Cloud provider returned invalid session: " + pyrepr.Repr(info))
	}
	return info, err
}

// Cloud session, a provider failure falling back to a local session marked fallback_from_cloud
func createCloudSessionOrFallback(ctx context.Context, env *tools.Env, cacheKey string, p BrowserProvider) (map[string]any, error) {
	info, err := providerCreate(env, p, cacheKey)
	if err != nil {
		name := providerClassName(p)
		cfgOf(env).logger().Warn(fmt.Sprintf("Cloud provider %s failed (%s); falling back to local Chromium for %s", name, err, cacheKey))
		local, localErr := createLocalSession(tools.WithEnvHome(ctx, env), env, cacheKey, true)
		if localErr != nil {
			return nil, runtimeError(fmt.Sprintf("Cloud provider %s failed (%s) and local fallback also failed (%s)", name, err, localErr))
		}
		out := make(map[string]any, len(local)+3)
		for k, v := range local {
			out[k] = v
		}
		out["fallback_from_cloud"], out["fallback_reason"], out["fallback_provider"] = true, err.Error(), name
		return out, nil
	}
	out := make(map[string]any, len(info))
	for k, v := range info {
		out[k] = v
	}
	if pyrepr.Truthy(out["cdp_url"]) {
		out["cdp_url"] = ResolveCDPOverride(context.Background(), pyrepr.Str(out["cdp_url"]))
	}
	return out, nil
}

// Fresh session for the key in precedence CDP override, forced local sidecar, cloud provider, local
func createSessionForKey(ctx context.Context, env *tools.Env, cacheKey string, forceLocal bool) (map[string]any, error) {
	override := sessionCDPOverride(env)
	if override != "" && !forceLocal {
		cfgOf(env).logger().Info("Created CDP browser session for " + cacheKey)
		return sessionRecord("cdp", override, map[string]any{"cdp_override": true}), nil
	}
	if forceLocal {
		return createLocalSession(tools.WithEnvHome(ctx, env), env, cacheKey, false)
	}
	p, err := sessionCloudProvider(env)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return createLocalSession(tools.WithEnvHome(ctx, env), env, cacheKey, true)
	}
	return createCloudSessionOrFallback(tools.WithEnvHome(ctx, env), env, cacheKey, p)
}

// Local session whose process is gone or whose shared launcher key now names another endpoint
func backendDead(env *tools.Env, info map[string]any) bool {
	features := featuresOf(info)
	if pyrepr.Truthy(features["lightpanda"]) {
		name, _ := info["session_name"].(string)
		server := getLightpandaServer(name)
		return server == nil || !server.IsAlive()
	}
	if pyrepr.Truthy(features["sandbox"]) {
		return !sandboxBrowserAlive(env, info)
	}
	if key, _ := info["launcher_key"].(string); key != "" {
		b := getBrowser(key)
		return b == nil || !pyrepr.Equal(b.CDPURL, info["cdp_url"])
	}
	return false
}

// Live unexpired session for the key, else tears the old one down and returns a concurrent replacement
func reuseOrEvict(ctx context.Context, env *tools.Env, cacheKey string) map[string]any {
	sessionsMu.Lock()
	existing, ok := activeSessions.Get(cacheKey)
	sessionsMu.Unlock()
	if !ok {
		return nil
	}
	if !sessionHasExpired(cfgOf(env).logger(), existing, nil) && !backendDead(env, existing) {
		return existing
	}
	cfgOf(env).logger().Info("Replacing expired or dead browser session for " + cacheKey)
	cleanupSingleSession(tools.WithEnvHome(ctx, env), env, cacheKey)
	updateSessionActivity(env, cacheKey)
	sessionsMu.Lock()
	replacement, ok := activeSessions.Get(cacheKey)
	sessionsMu.Unlock()
	if ok && !sameSession(replacement, existing) {
		return replacement
	}
	return nil
}

// Publishes a copy of the record under the key, the caller holding sessionsMu
func publishSession(cacheKey string, info map[string]any, homeKey string) map[string]any {
	out := make(map[string]any, len(info)+3)
	for k, v := range info {
		out[k] = v
	}
	for k, v := range map[string]any{"session_key": cacheKey, "owner_task_id": ownerTaskID(cacheKey), "home_key": homeKey} {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	activeSessions.Set(cacheKey, out)
	return out
}

// Gets or creates the key's session, a live endpoint publish mid build discarding the stale session and rebuilding
func getSessionInfo(ctx context.Context, env *tools.Env, cacheKey string) (map[string]any, error) {
	key := cacheKey
	if key == "" {
		key = "default"
	}
	sessionStartCleanup(tools.WithEnvHome(ctx, env), env)
	updateSessionActivity(env, key)
	if reused := reuseOrEvict(tools.WithEnvHome(ctx, env), env, key); reused != nil {
		return reused, nil
	}
	forceLocal := isLocalSidecarKey(key)
	homeKey := cfgOf(env).homeKey()
	var info map[string]any
	for attempt := 0; attempt <= staleEndpointRetries; attempt++ {
		generation := LiveCDPGeneration(homeKey)
		built, err := createSessionForKey(tools.WithEnvHome(ctx, env), env, key, forceLocal)
		if err != nil {
			return nil, err
		}
		sessionsMu.Lock()
		winner, raced := activeSessions.Get(key)
		published := false
		if !raced && (LiveCDPGeneration(homeKey) == generation || attempt == staleEndpointRetries) {
			info = publishSession(key, built, homeKey)
			published = true
		}
		sessionsMu.Unlock()
		if raced {
			releaseSessionBackend(tools.WithEnvHome(ctx, env), env, key, built)
			return winner, nil
		}
		if published {
			break
		}
		cfgOf(env).logger().Info(fmt.Sprintf("Browser endpoint changed during session creation for %s; recreating", key))
		releaseSessionBackend(tools.WithEnvHome(ctx, env), env, key, built)
	}
	if !pyrepr.Truthy(featuresOf(info)["local"]) {
		sessionEnsureSupervisor(env, key)
	}
	return info, nil
}

// Closes this process's browser sessions then reaps orphans of crashed processes, once per process
func EmergencyCleanupAllSessions(ctx context.Context) { emergencyCleanupAllSessions(ctx) }

// Process exit sweep, every session closed and the janitor stopped, which the binary's shutdown path calls
func AtexitCleanup(ctx context.Context) {
	emergencyCleanupAllSessions(ctx)
	stopCleanupThread()
}
