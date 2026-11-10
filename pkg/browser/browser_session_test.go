package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const localTestCDP = "ws://127.0.0.1:9333/devtools/browser/local"

type scriptedCloud struct {
	provider.ProviderDefaults
	create func(taskID string) (map[string]any, error)
	close  func(id string) bool
	calls  int
}

func newScriptedCloud() *scriptedCloud {
	p := &scriptedCloud{}
	p.Self = p
	return p
}

func (p *scriptedCloud) Name() string                { return "scripted" }
func (p *scriptedCloud) IsAvailable(*tools.Env) bool { return true }
func (p *scriptedCloud) CreateSession(_ *tools.Env, taskID string) (map[string]any, error) {
	p.calls++
	if p.create == nil {
		return nil, nil
	}
	return p.create(taskID)
}
func (p *scriptedCloud) CloseSession(_ *tools.Env, id string) bool {
	if p.close != nil {
		return p.close(id)
	}
	return true
}
func (p *scriptedCloud) EmergencyCleanup(*tools.Env, string) {}

// Fresh session tables and no janitor or supervisor, as the hermes fixtures isolate browser state
func isolateSessions(t *testing.T) *tools.Env {
	t.Helper()
	pin(t, &activeSessions, pydict.New[string, map[string]any]())
	pin(t, &sessionLastActivity, pydict.New[string, float64]())
	pin(t, &sessionOwnerEnvs, pydict.New[string, *tools.Env]())
	pin(t, &cleanupFailures, map[string]int{})
	pin(t, &sessionStartCleanup, func(context.Context, *tools.Env) {})
	pin(t, &sessionEnsureSupervisor, func(*tools.Env, string) {})
	pin(t, &sessionLaunchChromium, func(_ context.Context, _ *tools.Env, key string, _ LaunchOptions) (*LaunchedBrowser, string) {
		pid := int64(4242)
		return &LaunchedBrowser{Key: key, CDPURL: localTestCDP, PID: &pid}, ""
	})
	pin(t, &sessionCDPOverride, func(*tools.Env) string { return "" })
	return testEnv(t)
}

func withProvider(t *testing.T, p BrowserProvider) {
	pin(t, &sessionCloudProvider, func(*tools.Env) (BrowserProvider, error) { return p, nil })
}

// TestCloudProviderRuntimeFallback test_cloud_failure_falls_back_to_local
func TestCloudFailureFallsBackToLocal(t *testing.T) {
	env := isolateSessions(t)
	p := newScriptedCloud()
	p.create = func(string) (map[string]any, error) { return nil, runtimeError("401 Unauthorized") }
	withProvider(t, p)
	session, err := getSessionInfo(context.Background(), env, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if session["fallback_from_cloud"] != true || session["fallback_reason"] != "401 Unauthorized" || session["fallback_provider"] != "scriptedCloud" {
		t.Fatalf("session %v", session)
	}
	if featuresOf(session)["local"] != true || session["cdp_url"] != localTestCDP {
		t.Fatalf("not the local browser %v", session)
	}
}

// TestCloudProviderRuntimeFallback test_no_provider_uses_local_directly
func TestNoProviderUsesLocalDirectly(t *testing.T) {
	env := isolateSessions(t)
	withProvider(t, nil)
	session, err := getSessionInfo(context.Background(), env, "task-4")
	if err != nil {
		t.Fatal(err)
	}
	if featuresOf(session)["local"] != true {
		t.Fatalf("session %v", session)
	}
	if _, ok := session["fallback_from_cloud"]; ok {
		t.Fatal("direct local session marked as a fallback")
	}
	if session["session_key"] != "task-4" || session["owner_task_id"] != "task-4" || session["home_key"] != cfgOf(env).homeKey() {
		t.Fatalf("publish fields %v", session)
	}
}

// TestCloudProviderRuntimeFallback test_cloud_returns_invalid_session_triggers_fallback
func TestCloudInvalidSessionTriggersFallback(t *testing.T) {
	env := isolateSessions(t)
	withProvider(t, newScriptedCloud())
	session, err := getSessionInfo(context.Background(), env, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	if session["fallback_from_cloud"] != true || session["fallback_reason"] != "Cloud provider returned invalid session: {}" {
		t.Fatalf("session %v", session)
	}
}

func TestCloudAndLocalFailureReportsBoth(t *testing.T) {
	env := isolateSessions(t)
	p := newScriptedCloud()
	p.create = func(string) (map[string]any, error) { return nil, runtimeError("quota") }
	withProvider(t, p)
	pin(t, &sessionLaunchChromium, func(context.Context, *tools.Env, string, LaunchOptions) (*LaunchedBrowser, string) { return nil, "" })
	_, err := getSessionInfo(context.Background(), env, "task-9")
	want := "Cloud provider scriptedCloud failed (quota) and local fallback also failed (the local browser could not be started)"
	if err == nil || err.Error() != want {
		t.Fatalf("err %v", err)
	}
}

// test_browser_use_session_expiry test_browser_use_preserves_provider_timeout
func TestBrowserUsePreservesProviderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id": "browser-session-1", "cdpUrl": "ws://browser-use.example/devtools/browser/1", "timeoutAt": "2030-01-01T00:05:00Z"}`))
	}))
	defer srv.Close()
	env, _ := browserUseCase(t, "", "nous", &tools.ManagedToolGatewayConfig{GatewayOrigin: srv.URL, NousUserToken: "test-key", ManagedMode: true})
	session, err := newBrowserUseProvider().CreateSession(env, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if session["expires_at"] != "2030-01-01T00:05:00Z" {
		t.Fatalf("session %v", session)
	}
}

// test_browser_use_session_expiry test_live_cloud_session_is_reused
func TestLiveCloudSessionIsReused(t *testing.T) {
	env := isolateSessions(t)
	existing := map[string]any{"session_name": "existing", "bb_session_id": "browser-session-1",
		"cdp_url": "ws://browser-use.example/devtools/browser/1", "expires_at": "2999-01-01T00:05:00Z"}
	activeSessions.Set("task-1", existing)
	p := newScriptedCloud()
	withProvider(t, p)
	session, err := getSessionInfo(context.Background(), env, "task-1")
	if err != nil || !sameSession(session, existing) || p.calls != 0 {
		t.Fatalf("session %v err %v creates %d", session, err, p.calls)
	}
}

// test_browser_use_session_expiry test_expired_cloud_session_is_replaced_without_reusing_dead_cdp
func TestExpiredCloudSessionIsReplaced(t *testing.T) {
	env := isolateSessions(t)
	activeSessions.Set("task-1", map[string]any{"session_name": "expired", "bb_session_id": "browser-session-old",
		"cdp_url": "ws://browser-use.example/devtools/browser/old", "expires_at": "2020-01-01T00:05:00Z"})
	sessionLastActivity.Set("task-1", 1.0)
	var events [][2]string
	p := newScriptedCloud()
	p.close = func(id string) bool { events = append(events, [2]string{"close", id}); return true }
	p.create = func(taskID string) (map[string]any, error) {
		events = append(events, [2]string{"create", taskID})
		return map[string]any{"session_name": "replacement", "bb_session_id": "browser-session-new",
			"cdp_url": "ws://browser-use.example/devtools/browser/new", "expires_at": "2999-01-01T00:05:00Z"}, nil
	}
	withProvider(t, p)
	pin(t, &sessionStopSupervisor, func(_ *tools.Env, key string) { events = append(events, [2]string{"stop", key}) })
	session, err := getSessionInfo(context.Background(), env, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if session["bb_session_id"] != "browser-session-new" || session["cdp_url"] != "ws://browser-use.example/devtools/browser/new" {
		t.Fatalf("session %v", session)
	}
	if published, _ := activeSessions.Get("task-1"); !sameSession(published, session) {
		t.Fatal("returned session is not the published one")
	}
	if !sessionLastActivity.Has("task-1") {
		t.Fatal("replacement activity untracked")
	}
	want := [][2]string{{"stop", "task-1"}, {"close", "browser-session-old"}, {"create", "task-1"}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v", events)
	}
}

func TestStaleEndpointSessionIsRebuilt(t *testing.T) {
	env := isolateSessions(t)
	withProvider(t, nil)
	builds := 0
	pin(t, &sessionLaunchChromium, func(_ context.Context, e *tools.Env, key string, _ LaunchOptions) (*LaunchedBrowser, string) {
		builds++
		if builds == 1 {
			SetLiveCDPOverride(cfgOf(e).homeKey(), "")
		}
		return &LaunchedBrowser{Key: key, CDPURL: localTestCDP}, ""
	})
	if _, err := getSessionInfo(context.Background(), env, "task-2"); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("builds %d", builds)
	}
}

// TestSessionInactivityTimeout test_default_matches_config_default
func TestSessionInactivityTimeoutDefault(t *testing.T) {
	t.Setenv("BROWSER_INACTIVITY_TIMEOUT", "")
	os.Unsetenv("BROWSER_INACTIVITY_TIMEOUT")
	want := ConfigDefaults()["inactivity_timeout"]
	if got := getSessionInactivityTimeout(&tools.Env{Config: config.Tree{}}); got != want {
		t.Fatalf("timeout %d want %v", got, want)
	}
}

// TestSessionInactivityTimeout test_invalid_config_preserves_env_fallback
func TestSessionInactivityTimeoutInvalidConfigKeepsEnvFallback(t *testing.T) {
	t.Setenv("BROWSER_INACTIVITY_TIMEOUT", "240")
	if got := getSessionInactivityTimeout(browserEnv(map[string]any{"inactivity_timeout": "not-an-int"})); got != 240 {
		t.Fatalf("timeout %d", got)
	}
}

func TestOwnerTaskIDAndSessionForTask(t *testing.T) {
	isolateSessions(t)
	for key, want := range map[string]string{"t1": "t1", "t1::local": "t1", "t1@/h/p": "t1", "t1@/h/p::local": "t1", "a@b@c": "a@b"} {
		if got := ownerTaskID(key); got != want {
			t.Fatalf("%s -> %s", key, got)
		}
	}
	sidecar, tagged := map[string]any{"n": 1}, map[string]any{"n": 2}
	activeSessions.Set("t1::local", sidecar)
	activeSessions.Set("t1@/h/p", tagged)
	if !sameSession(sessionForTask("t1"), tagged) || len(sessionForTask("t2")) != 0 {
		t.Fatal("session for task picked the wrong record")
	}
}

func TestSessionExpiryParsing(t *testing.T) {
	log := cfgOf(nil).logger()
	now := 1893456300.0
	for _, c := range []struct {
		v       any
		expired bool
	}{{"2030-01-01T00:05:00Z", true}, {"2030-01-01T00:05:00", true}, {"2030-01-01T00:05:01+00:00", false},
		{1893456300, true}, {1893456301.5, false}, {true, false}, {"  ", false}, {"garbage", false}} {
		if got := sessionHasExpired(log, map[string]any{"expires_at": c.v}, &now); got != c.expired {
			t.Fatalf("%v expired %v", c.v, got)
		}
	}
}

func TestRunFencedRunsUnsharedSessionsDirectly(t *testing.T) {
	ran := false
	out := runFenced(nil, map[string]any{"features": map[string]any{"cdp_override": true}}, func() map[string]any {
		ran = true
		return map[string]any{"success": true}
	})
	if !ran || out["success"] != true {
		t.Fatal("unshared session was fenced")
	}
	if err := catchRaised(func() error { panic(errors.New("boom")) }); err == nil || err.Error() != "boom" {
		t.Fatalf("raised %v", err)
	}
}

// TestBrowserCleanup test_cleanup_browser_clears_tracking_state
func TestCleanupBrowserClearsTrackingState(t *testing.T) {
	env := isolateSessions(t)
	activeSessions.Set("task-1", map[string]any{"session_name": "sess-1", "bb_session_id": nil, "launcher_key": "task-1",
		"features": map[string]any{"local": true}})
	sessionLastActivity.Set("task-1", 123.0)
	var stopped []string
	pin(t, &sessionStopBrowser, func(_ context.Context, _ *tools.Env, key string) { stopped = append(stopped, key) })
	pin(t, &sessionStopSupervisor, func(*tools.Env, string) {})
	if err := cleanupBrowser(context.Background(), env, "task-1"); err != nil {
		t.Fatal(err)
	}
	if activeSessions.Has("task-1") || sessionLastActivity.Has("task-1") || !reflect.DeepEqual(stopped, []string{"task-1"}) {
		t.Fatalf("tracking left %v %v", activeSessions.Keys(), stopped)
	}
}

// TestBrowserCleanup test_emergency_cleanup_clears_all_tracking_state
func TestEmergencyCleanupClearsAllTrackingState(t *testing.T) {
	isolateSessions(t)
	pin(t, &cleanupDone, false)
	activeSessions.Set("task-1", map[string]any{"session_name": "sess-1"})
	activeSessions.Set("task-2", map[string]any{"session_name": "sess-2"})
	sessionLastActivity.Set("task-1", 1.0)
	sessionLastActivity.Set("task-2", 2.0)
	cleaned, reaped := 0, 0
	pin(t, &lifecycleCleanupAll, func(context.Context, *tools.Env) { cleaned++ })
	pin(t, &orphanReapRuns, func(context.Context, *tools.Env) int { reaped++; return 0 })
	emergencyCleanupAllSessions(context.Background())
	emergencyCleanupAllSessions(context.Background())
	if cleaned != 1 || reaped != 1 || activeSessions.Len() != 0 || sessionLastActivity.Len() != 0 || !cleanupDone {
		t.Fatalf("cleaned %d reaped %d sessions %d", cleaned, reaped, activeSessions.Len())
	}
}

// TestInactivityJanitorMultiplex test_janitor_tears_down_under_owner_profile_scope
func TestJanitorTearsDownUnderOwnerProfile(t *testing.T) {
	isolateSessions(t)
	pin(t, &sessionInactivity, func() int { return 0 })
	pin(t, &sessionStopSupervisor, func(*tools.Env, string) {})
	clearCloudEnv(t)
	p1 := t.TempDir()
	if err := os.WriteFile(p1+"/.env", []byte("BROWSER_USE_API_KEY=p1-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := &tools.Env{Home: p1, Secrets: config.NewSecrets(p1), Config: config.Tree{"browser": map[string]any{"cloud_provider": "local"}}}
	updateSessionActivity(owner, "t1")
	activeSessions.Set("t1", map[string]any{"session_name": "s1", "bb_session_id": nil, "launcher_key": "t1",
		"features": map[string]any{"local": true}})
	last, _ := sessionLastActivity.Get("t1")
	sessionLastActivity.Set("t1", last-10)
	seen := map[string]string{}
	pin(t, &sessionStopBrowser, func(_ context.Context, env *tools.Env, _ string) {
		seen["home"], seen["key"] = env.Home, cfgOf(env).secret("BROWSER_USE_API_KEY")
	})
	cleanupInactiveSessions(context.Background())
	if !reflect.DeepEqual(seen, map[string]string{"home": p1, "key": "p1-key"}) {
		t.Fatalf("teardown ran under %v", seen)
	}
	if sessionLastActivity.Has("t1") || activeSessions.Has("t1") || sessionOwnerEnvs.Has("t1") {
		t.Fatal("janitor left tracking state")
	}
}

// TestInactivityJanitorMultiplex test_repeated_failures_force_reap_and_close_cloud_session
func TestRepeatedFailuresForceReapAndCloseCloudSession(t *testing.T) {
	env := isolateSessions(t)
	pin(t, &sessionInactivity, func() int { return 0 })
	pin(t, &sessionStopSupervisor, func(*tools.Env, string) {})
	activeSessions.Set("t1", map[string]any{"session_name": "s1", "bb_session_id": "bb-1"})
	sessionLastActivity.Set("t1", 1.0)
	var closed []string
	p := newScriptedCloud()
	p.close = func(id string) bool { closed = append(closed, id); return true }
	withProvider(t, p)
	pin(t, &lifecycleCleanupSingle, func(context.Context, *tools.Env, string) error { return runtimeError("boom") })
	for range maxInactivityCleanupFailures - 1 {
		cleanupInactiveSessions(context.Background())
	}
	updateSessionActivity(env, "t1")
	sessionLastActivity.Set("t1", 1.0)
	if cleanupFailures["t1"] != maxInactivityCleanupFailures-1 || !activeSessions.Has("t1") || len(closed) != 0 {
		t.Fatalf("failures %d closed %v", cleanupFailures["t1"], closed)
	}
	cleanupInactiveSessions(context.Background())
	if !reflect.DeepEqual(closed, []string{"bb-1"}) || activeSessions.Has("t1") || sessionLastActivity.Has("t1") {
		t.Fatalf("force reap closed %v", closed)
	}
	if _, ok := cleanupFailures["t1"]; ok {
		t.Fatal("failure count kept")
	}
}

// TestAtexitStopSwallowsInterrupt test_second_ctrl_c_during_join_does_not_propagate, the join bounded
func TestStopCleanupThreadJoinIsBounded(t *testing.T) {
	pin(t, &janitorJoin, 50*time.Millisecond)
	janitorMu.Lock()
	janitorRunning, janitorStop, janitorDone = true, make(chan struct{}), make(chan struct{})
	janitorMu.Unlock()
	start := time.Now()
	stopCleanupThread()
	if time.Since(start) > 2*time.Second || janitorRunning {
		t.Fatal("stop waited on a janitor that never finished")
	}
}

func TestJanitorStartsOnceAndStops(t *testing.T) {
	isolateSessions(t)
	pin(t, &janitorTick, time.Millisecond)
	reaps := make(chan struct{}, 4)
	pin(t, &orphanReapRuns, func(context.Context, *tools.Env) int { reaps <- struct{}{}; return 0 })
	startCleanupThread(context.Background(), nil)
	startCleanupThread(context.Background(), nil)
	select {
	case <-reaps:
	case <-time.After(5 * time.Second):
		t.Fatal("startup reap never ran")
	}
	stopCleanupThread()
	if janitorRunning {
		t.Fatal("janitor still marked running")
	}
}
