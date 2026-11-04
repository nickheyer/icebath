package provider

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The tick derives from the server issued lifetime so a refresh fires while a tick can still act on it
const (
	NousAuthKeepaliveInterval          = 15 * 60
	NousAuthKeepaliveMinInterval       = 60
	NousAuthKeepaliveTicksPerLifetime  = 4
	NousAuthKeepaliveInitialDelay      = 60
	NousAuthKeepaliveIntervalConfigKey = "keepalive_interval_seconds"
)

// Resolve timeout, the explicit value else ICEBATH_NOUS_TIMEOUT_SECONDS else fifteen seconds
func nousKeepaliveTimeout(value time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	raw := os.Getenv("ICEBATH_NOUS_TIMEOUT_SECONDS")
	if raw == "" {
		raw = "15"
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 15 * time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

// The nous section of config.yaml, empty on any failure
func nousKeepaliveConfig(env PoolEnv) map[string]any {
	cfg := env.config()
	if cfg == nil && env.Store != nil {
		cfg = LoadConfigSafe(env.Store.Home())
	}
	section, _ := cfg["nous"].(map[string]any)
	if section == nil {
		return map[string]any{}
	}
	return section
}

// Tick interval, the explicit argument then nous.keepalive_interval_seconds then the default, non positive disables
func nousKeepaliveIntervalSeconds(env PoolEnv, value *int) int {
	if value != nil {
		return *value
	}
	raw := nousKeepaliveConfig(env)[NousAuthKeepaliveIntervalConfigKey]
	if raw == nil {
		return NousAuthKeepaliveInterval
	}
	if s, ok := raw.(string); ok && strings.TrimSpace(s) == "" {
		return NousAuthKeepaliveInterval
	}
	if f, ok := anyFloat(raw); ok {
		return int(f)
	}
	if s, ok := raw.(string); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return int(f)
		}
	}
	env.log().Warn(fmt.Sprintf("Ignoring invalid nous.%s=%v; using %ds", NousAuthKeepaliveIntervalConfigKey, raw, NousAuthKeepaliveInterval))
	return NousAuthKeepaliveInterval
}

// Server issued lifetime of the stored Nous credentials, the shorter of access token and agent key, false when none
func nousObservedLifetimeSeconds(env PoolEnv) (int, bool) {
	state, err := env.Store.GetProviderAuthState("nous")
	if err != nil || state == nil {
		return 0, false
	}
	lifetime, found := 0, false
	for _, key := range []string{"expires_in", "agent_key_expires_in"} {
		value, ok := anyFloat(state[key])
		if !ok {
			if s, isStr := state[key].(string); isStr {
				f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					continue
				}
				value = f
			} else {
				continue
			}
		}
		seconds := int(value)
		if seconds > 0 && (!found || seconds < lifetime) {
			lifetime, found = seconds, true
		}
	}
	return lifetime, found
}

// Tick fast enough to refresh several times per credential lifetime
func nousKeepaliveTickSeconds(configuredInterval int, lifetime int, hasLifetime bool) int {
	if !hasLifetime || lifetime <= 0 {
		return configuredInterval
	}
	derived := lifetime / NousAuthKeepaliveTicksPerLifetime
	tick := configuredInterval
	if derived < tick {
		tick = derived
	}
	if tick < NousAuthKeepaliveMinInterval {
		tick = NousAuthKeepaliveMinInterval
	}
	return tick
}

// Life a credential needs to be left alone this tick, it must survive until the next tick plus the skew
func nousRefreshHorizonSeconds(tickSeconds, floorSeconds int) int {
	horizon := tickSeconds + int(AccessTokenRefreshSkew/time.Second)
	if floorSeconds > horizon {
		return floorSeconds
	}
	return horizon
}

func nousKeepaliveEntryState(entry PooledCredential) map[string]any {
	return map[string]any{"agent_key": nilIfEmpty(entry.AgentKey), "agent_key_expires_at": nilIfEmpty(entry.AgentKeyExpiresAt), "scope": entry.Extra["scope"]}
}

// The pool surface the keepalive drives
type nousKeepalivePool interface {
	HasCredentials() bool
	Select(model string) (PooledCredential, bool)
	TryRefreshCurrent() (PooledCredential, bool)
}

// Nous pool loader the keepalive uses, nil for no pool, swappable so tests can script a pool
var nousKeepaliveLoadPoolFn = func(env PoolEnv) nousKeepalivePool {
	pool := LoadPool(env, "nous")
	if pool == nil {
		return nil
	}
	return pool
}

// Singleton resolver the keepalive falls back to, swappable so tests can observe the call
var nousKeepaliveResolveFn = resolveNousRuntimeCredentialsWith

// Refreshes the current pool entry when stale, usable true, no usable entry false, no pool at all known false
func nousRefreshSelectedPoolEntry(env PoolEnv, minKeyTTL time.Duration, minAccessTTL time.Duration, hasMinAccessTTL bool) (usable bool, known bool) {
	var pool nousKeepalivePool
	func() {
		defer func() {
			if r := recover(); r != nil {
				env.log().Debug(fmt.Sprintf("Nous auth keepalive: credential pool unavailable: %v", r))
				pool = nil
			}
		}()
		pool = nousKeepaliveLoadPoolFn(env)
	}()
	if pool == nil || !pool.HasCredentials() {
		return false, false
	}
	entry, ok := pool.Select("")
	if !ok {
		return false, true
	}
	if !hasMinAccessTTL {
		minAccessTTL = AccessTokenRefreshSkew
	}
	accessExpiring := IsExpiring(nilIfEmpty(entry.ExpiresAt), minAccessTTL)
	keyUsable := nousAgentKeyIsUsable(nousKeepaliveEntryState(entry), minKeyTTL)
	if accessExpiring || !keyUsable {
		if _, refreshed := pool.TryRefreshCurrent(); !refreshed {
			return false, true
		}
		env.log().Debug("Nous auth keepalive: refreshed credential pool entry")
	}
	return true, true
}

// NousKeepaliveOptions tune one keepalive pass
type NousKeepaliveOptions struct {
	MinKeyTTL       time.Duration
	MinAccessTTL    time.Duration
	HasMinAccessTTL bool
	Timeout         time.Duration
}

// RefreshNousAuthKeepaliveOnce refreshes Nous auth once when configured, pool entry first then the singleton
func RefreshNousAuthKeepaliveOnce(env PoolEnv, opts NousKeepaliveOptions) bool {
	minKeyTTL := opts.MinKeyTTL
	if minKeyTTL <= 0 {
		minKeyTTL = NousInvokeJWTMinTTL
	}
	if minKeyTTL < 60*time.Second {
		minKeyTTL = 60 * time.Second
	}
	scoped := env
	scoped.Ctx = withLaunchProfileScopeIfMultiplexed(env.ctx())
	if usable, known := nousRefreshSelectedPoolEntry(scoped, minKeyTTL, opts.MinAccessTTL, opts.HasMinAccessTTL); known {
		return usable
	}
	state, err := scoped.Store.GetProviderAuthState("nous")
	if err != nil || len(state) == 0 {
		return false
	}
	if isGuestState(state) {
		return false
	}
	background := scoped
	background.Ctx = withBackgroundCaller(scoped.ctx())
	if _, err := nousKeepaliveResolveFn(background, NousResolveOptions{Timeout: nousKeepaliveTimeout(opts.Timeout)}); err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) && authErr.ReloginRequired {
			scoped.log().Info(fmt.Sprintf("Nous auth keepalive requires re-login: %v", err))
		} else {
			scoped.log().Debug(fmt.Sprintf("Nous auth keepalive failed: %v", err))
		}
		return false
	}
	scoped.log().Debug("Nous auth keepalive: refreshed singleton auth state")
	return true
}

// NousAuthKeepalive is the process wide background refresher of a long lived Nous session
type NousAuthKeepalive struct {
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

var nousKeepalive = &NousAuthKeepalive{}

// Sleep that ends early when the stop channel closes, true when stopped
func nousKeepaliveWait(stop <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop:
		return true
	case <-timer.C:
		return false
	}
}

// Runs the keepalive ticks until stopped, the lifetime re read each pass
func nousKeepaliveLoop(env PoolEnv, stop <-chan struct{}, done chan<- struct{}, intervalSeconds, initialDelaySeconds int, minKeyTTL, timeout time.Duration) {
	defer close(done)
	if initialDelaySeconds > 0 && nousKeepaliveWait(stop, time.Duration(initialDelaySeconds)*time.Second) {
		return
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		lifetime, hasLifetime := nousObservedLifetimeSeconds(env)
		tick := nousKeepaliveTickSeconds(intervalSeconds, lifetime, hasLifetime)
		horizon := time.Duration(nousRefreshHorizonSeconds(tick, int(minKeyTTL/time.Second))) * time.Second
		RefreshNousAuthKeepaliveOnce(env, NousKeepaliveOptions{MinKeyTTL: horizon, MinAccessTTL: horizon, HasMinAccessTTL: true, Timeout: timeout})
		if nousKeepaliveWait(stop, time.Duration(tick)*time.Second) {
			return
		}
	}
}

// NousKeepaliveStartOptions tune the keepalive thread, a nil interval reads config.yaml
type NousKeepaliveStartOptions struct {
	IntervalSeconds     *int
	InitialDelaySeconds int
	HasInitialDelay     bool
	MinKeyTTL           time.Duration
	Timeout             time.Duration
}

// StartNousAuthKeepalive starts the process wide keepalive once, false when disabled or on the free tier
func StartNousAuthKeepalive(env PoolEnv, opts NousKeepaliveStartOptions) bool {
	guestHandoffStartWorker(env)
	interval := nousKeepaliveIntervalSeconds(env, opts.IntervalSeconds)
	if interval <= 0 {
		return false
	}
	if state, err := env.Store.GetProviderAuthState("nous"); err == nil && isGuestState(state) {
		env.log().Debug("Nous auth keepalive skipped: free tier has no refresh token")
		return false
	}
	initialDelay := NousAuthKeepaliveInitialDelay
	if opts.HasInitialDelay {
		initialDelay = opts.InitialDelaySeconds
	}
	if initialDelay < 0 {
		initialDelay = 0
	}
	minKeyTTL := opts.MinKeyTTL
	if minKeyTTL <= 0 {
		minKeyTTL = NousInvokeJWTMinTTL
	}
	if minKeyTTL < 60*time.Second {
		minKeyTTL = 60 * time.Second
	}
	k := nousKeepalive
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.done != nil {
		select {
		case <-k.done:
		default:
			return true
		}
	}
	k.stop = make(chan struct{})
	k.done = make(chan struct{})
	go nousKeepaliveLoop(env, k.stop, k.done, interval, initialDelay, minKeyTTL, opts.Timeout)
	env.log().Debug("Nous auth keepalive started")
	return true
}

// NousAuthKeepaliveRunning reports whether the keepalive goroutine is alive
func NousAuthKeepaliveRunning() bool {
	k := nousKeepalive
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.done == nil {
		return false
	}
	select {
	case <-k.done:
		return false
	default:
		return true
	}
}

// StopNousAuthKeepalive stops the keepalive goroutine, waiting up to the timeout for it to exit
func StopNousAuthKeepalive(timeout time.Duration) {
	guestHandoffStopWorker(timeout)
	k := nousKeepalive
	k.mu.Lock()
	stop, done := k.stop, k.done
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	k.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}
	k.mu.Lock()
	if k.done == done {
		k.stop, k.done = nil, nil
	}
	k.mu.Unlock()
}
