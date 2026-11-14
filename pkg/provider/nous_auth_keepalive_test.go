package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Observed credential lifetimes on real installs
var observedNousLifetimes = []int{3594, 899}

func TestRefreshAlwaysFiresBeforeExpiryForObservedLifetimes(t *testing.T) {
	for _, lifetime := range observedNousLifetimes {
		tick := nousKeepaliveTickSeconds(NousAuthKeepaliveInterval, lifetime, true)
		horizon := nousRefreshHorizonSeconds(tick, int(NousInvokeJWTMinTTL/time.Second))
		refreshedAt, found := 0, false
		for elapsed := 0; elapsed <= lifetime; elapsed += tick {
			if lifetime-elapsed <= horizon {
				refreshedAt, found = elapsed, true
				break
			}
		}
		if !found || refreshedAt >= lifetime {
			t.Fatalf("lifetime %d: refresh at %d found %v (tick %d horizon %d)", lifetime, refreshedAt, found, tick, horizon)
		}
	}
}

func keepaliveEnvWithConfig(t *testing.T, h string, section map[string]any) PoolEnv {
	t.Helper()
	env := testEnv(t, h)
	env.Config = func() map[string]any { return map[string]any{"nous": section} }
	return env
}

func TestIntervalPrecedenceAndDisable(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	if got := nousKeepaliveIntervalSeconds(keepaliveEnvWithConfig(t, h, map[string]any{}), nil); got != NousAuthKeepaliveInterval {
		t.Fatalf("absent section = %d", got)
	}
	env := keepaliveEnvWithConfig(t, h, map[string]any{NousAuthKeepaliveIntervalConfigKey: 600})
	if got := nousKeepaliveIntervalSeconds(env, nil); got != 600 {
		t.Fatalf("configured = %d", got)
	}
	explicit := 300
	if got := nousKeepaliveIntervalSeconds(env, &explicit); got != 300 {
		t.Fatalf("explicit = %d", got)
	}
	env = keepaliveEnvWithConfig(t, h, map[string]any{NousAuthKeepaliveIntervalConfigKey: "not-a-number"})
	if got := nousKeepaliveIntervalSeconds(env, nil); got != NousAuthKeepaliveInterval {
		t.Fatalf("malformed = %d", got)
	}
	env = keepaliveEnvWithConfig(t, h, map[string]any{NousAuthKeepaliveIntervalConfigKey: 0})
	if got := nousKeepaliveIntervalSeconds(env, nil); got != 0 {
		t.Fatalf("zero = %d", got)
	}
	if StartNousAuthKeepalive(env, NousKeepaliveStartOptions{}) {
		t.Fatalf("zero interval started the keepalive")
	}
}

// Scripted pool for the keepalive tests
type fakeKeepalivePool struct {
	hasCredentials bool
	entry          PooledCredential
	refreshed      bool
	onSelect       func()
}

func (p *fakeKeepalivePool) HasCredentials() bool { return p.hasCredentials }
func (p *fakeKeepalivePool) Select(string) (PooledCredential, bool) {
	if p.onSelect != nil {
		p.onSelect()
	}
	return p.entry, true
}
func (p *fakeKeepalivePool) TryRefreshCurrent() (PooledCredential, bool) {
	p.refreshed = true
	return p.entry, true
}

func staleKeepaliveEntry() PooledCredential {
	return PooledCredential{Provider: "nous", AccessToken: "pooled-access-token", ExpiresAt: "2000-01-01T00:00:00+00:00",
		Extra: map[string]any{"scope": "inference:invoke"}}
}

func TestKeepaliveRefreshesStalePoolEntry(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	pool := &fakeKeepalivePool{hasCredentials: true, entry: staleKeepaliveEntry()}
	nousKeepaliveLoadPoolFn = func(env PoolEnv) nousKeepalivePool { return pool }
	if !RefreshNousAuthKeepaliveOnce(testEnv(t, h), NousKeepaliveOptions{}) || !pool.refreshed {
		t.Fatalf("stale pool entry not refreshed")
	}
}

func TestKeepaliveFallsBackToSingletonState(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{"access_token": "stored-access-token"}}})
	nousKeepaliveLoadPoolFn = func(env PoolEnv) nousKeepalivePool { return &fakeKeepalivePool{} }
	var calls []NousResolveOptions
	nousKeepaliveResolveFn = func(env PoolEnv, opts NousResolveOptions) (map[string]any, error) {
		calls = append(calls, opts)
		return map[string]any{"provider": "nous", "api_key": "fresh-agent-key", "base_url": "https://inference-api.nousresearch.com/v1"}, nil
	}
	if !RefreshNousAuthKeepaliveOnce(testEnv(t, h), NousKeepaliveOptions{Timeout: 15 * time.Second}) {
		t.Fatalf("singleton refresh reported failure")
	}
	if len(calls) != 1 || calls[0].Timeout != 15*time.Second || calls[0].ForceRefresh {
		t.Fatalf("calls = %+v", calls)
	}
}

func writeLaunchEnv(t *testing.T, h, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKeepaliveBindsLaunchScopeForMultiplexedSingletonRefresh(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeLaunchEnv(t, h, "NOUS_INFERENCE_BASE_URL=https://inference.example/v1\nICEBATH_PORTAL_BASE_URL=https://portal.example\n")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{"access_token": "stored-token"}}})
	nousKeepaliveLoadPoolFn = func(env PoolEnv) nousKeepalivePool { return nil }
	type seenRouting struct{ inference, portal string }
	var seen []seenRouting
	nousKeepaliveResolveFn = func(env PoolEnv, opts NousResolveOptions) (map[string]any, error) {
		seen = append(seen, seenRouting{nousInferenceEnvOverrideIn(env), nousPortalEnvOverride(env)})
		return map[string]any{"provider": "nous"}, nil
	}
	env, rec := recordingEnv(t, h)
	env.Scope = nil
	env.Secrets = nil
	SetMultiplexActive(true)
	if !RefreshNousAuthKeepaliveOnce(env, NousKeepaliveOptions{Timeout: 15 * time.Second}) {
		t.Fatalf("keepalive reported failure")
	}
	if len(seen) != 1 || seen[0].inference != "https://inference.example/v1" || seen[0].portal != "https://portal.example" {
		t.Fatalf("seen = %+v", seen)
	}
	if len(rec.containing("no profile secret scope")) > 0 {
		t.Fatalf("daemon tick lost the launch profile scope")
	}
}

func TestKeepaliveBindsLaunchScopeForMultiplexedPoolRefresh(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeLaunchEnv(t, h, "NOUS_INFERENCE_BASE_URL=https://inference.example/v1\n")
	var seen []string
	pool := &fakeKeepalivePool{hasCredentials: true, entry: staleKeepaliveEntry()}
	nousKeepaliveLoadPoolFn = func(env PoolEnv) nousKeepalivePool {
		pool.onSelect = func() { seen = append(seen, CurrentSecretScope(env.ctx())["NOUS_INFERENCE_BASE_URL"]) }
		return pool
	}
	env := testEnv(t, h)
	env.Scope = nil
	env.Ctx = context.Background()
	SetMultiplexActive(true)
	if !RefreshNousAuthKeepaliveOnce(env, NousKeepaliveOptions{}) {
		t.Fatalf("keepalive reported failure")
	}
	if len(seen) != 1 || seen[0] != "https://inference.example/v1" {
		t.Fatalf("seen = %v", seen)
	}
}

func TestStartAndStopNousAuthKeepalive(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	nousKeepaliveLoadPoolFn = func(env PoolEnv) nousKeepalivePool { return nil }
	env := keepaliveEnvWithConfig(t, h, map[string]any{})
	if !StartNousAuthKeepalive(env, NousKeepaliveStartOptions{InitialDelaySeconds: 60, HasInitialDelay: true}) || !NousAuthKeepaliveRunning() {
		t.Fatalf("keepalive did not start")
	}
	if !StartNousAuthKeepalive(env, NousKeepaliveStartOptions{}) {
		t.Fatalf("second start did not report the running keepalive")
	}
	StopNousAuthKeepalive(5 * time.Second)
	if NousAuthKeepaliveRunning() {
		t.Fatalf("keepalive still running after stop")
	}
}
