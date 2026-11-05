package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Guards every connection ledger, the tool to server map and the loop state
var mcpMu sync.Mutex

var (
	mcpServers                 = keyedMap[*serverTask]{}
	serverScopeKeys            = keyedMap[string]{}
	serverToolScopes           = keyedMap[map[string]bool]{}
	serverConnectErrors        = keyedMap[string]{}
	lazyServerConfigs          = keyedMap[map[string]any]{}
	lazyServerFingerprints     = keyedMap[string]{}
	lazyServerToolNames        = keyedMap[[]string]{}
	serverConnectRetryAfter    = keyedMap[time.Time]{}
	serverConnectFailures      = keyedMap[int]{}
	serverErrorCounts          = keyedMap[int]{}
	serverBreakerOpenedAt      = keyedMap[time.Time]{}
	serverErrorsAllApplication = keyedMap[bool]{}
	serverTrustLevels          = keyedMap[string]{}
	toolReadOnlyHints          = keyedMap[map[string]bool]{}
	serverCostPolicies         = keyedMap[*CostPolicy]{}
	serverConnecting           = keyedSet{}
	parallelSafeServers        = keyedSet{}
)

// Registered MCP tool name to the server that registered it, guarded by mcpMu
var mcpToolServerNames = map[string]string{}

const (
	connectRetryBaseBackoff  = 30 * time.Second
	connectRetryMaxBackoff   = 600 * time.Second
	circuitBreakerThreshold  = 3
	circuitBreakerCooldown   = 60 * time.Second
	recycledReconnectTimeout = 15 * time.Second
	discoveryPassMax         = 300 * time.Second
	discoveryLockRetryDelay  = 500 * time.Millisecond
	discoveryLockMaxRetries  = int(discoveryPassMax/discoveryLockRetryDelay) + 20
	hydrateRetryInterval     = 30 * time.Second
)

// Monotonic clock of the cooldown, breaker and hydration deadlines, swapped by tests
var managerNow = time.Now

// Pause between discovery lock attempts, swapped by tests
var discoveryLockSleep = func() { time.Sleep(discoveryLockRetryDelay) }

// Counts a failure on the calling scope's connection and stamps the breaker open at the threshold
func bumpServerError(ctx context.Context, serverName string, application bool) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	key := resolveServerKey(ctx, serverName, "", true)
	n := serverErrorCounts[key] + 1
	serverErrorCounts[key] = n
	serverErrorsAllApplication[key] = application && (n == 1 || serverErrorsAllApplication[key])
	if n >= circuitBreakerThreshold {
		serverBreakerOpenedAt[key] = managerNow()
	}
}

// Closes the calling scope's breaker on any unambiguous success signal
func resetServerError(ctx context.Context, serverName string) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	key := resolveServerKey(ctx, serverName, "", true)
	serverErrorCounts[key] = 0
	delete(serverBreakerOpenedAt, key)
	delete(serverErrorsAllApplication, key)
}

// Session tool scope, else the served profile's registry key in a process serving routed profiles, else empty
func mcpRegistryScope(ctx context.Context) string {
	if s := tools.SessionToolScope(ctx); s != "" {
		return s
	}
	if !servesRoutedProfile(ctx) {
		return ""
	}
	return tools.ScopeKey(home.Dir(ctx))
}

// Scope owning the tools of the connection under key, captured at adoption, else the key's own, else the current, caller holds mcpMu
func serverRegistryScope(ctx context.Context, key ServerKey) string {
	if owner, ok := serverScopeKeys[key]; ok {
		return owner
	}
	if key.Scoped() {
		return key.Scope
	}
	return mcpRegistryScope(ctx)
}

// Reports whether the live connection under key serves scope without changing its owner, caller holds mcpMu
func serverVisibleInScope(key ServerKey, scope string) bool {
	if scope == "" {
		return true
	}
	return serverScopeKeys[key] == scope || serverToolScopes[key][scope]
}

// Stamps a doubling retry cooldown capped at ten minutes after a failed connect, caller holds mcpMu
func recordConnectFailure(ctx context.Context, serverName string) {
	key := serverKey(ctx, serverName, "", true)
	n := serverConnectFailures[key] + 1
	serverConnectFailures[key] = n
	backoff := connectRetryBaseBackoff << (n - 1)
	if n > 6 || backoff > connectRetryMaxBackoff {
		backoff = connectRetryMaxBackoff
	}
	serverConnectRetryAfter[key] = managerNow().Add(backoff)
}

// Clears the cooldown state after a successful connect, caller holds mcpMu
func clearConnectFailure(ctx context.Context, serverName string) {
	key := serverKey(ctx, serverName, "", true)
	delete(serverConnectFailures, key)
	delete(serverConnectRetryAfter, key)
}

// Reports whether this scope's connection is still inside its retry cooldown, caller holds mcpMu
func connectCooldownActive(ctx context.Context, serverName string) bool {
	deadline, ok := serverConnectRetryAfter[serverKey(ctx, serverName, "", true)]
	return ok && managerNow().Before(deadline)
}

// Profile home whose secrets the connection owner reads, empty for a single profile process
func ownerScopeHome(ctx context.Context) string {
	scope := mcpRegistryScope(ctx)
	if scope == "" {
		return ""
	}
	if h := currentSecretScopeHome(ctx); h != "" {
		return h
	}
	return scope
}

var (
	hydrateMu         sync.Mutex
	hydrateRetryAfter = map[string]time.Time{}
)

// Fresh owner secret mapping, retrying an incomplete external source hydration at most once per interval per home
func ownerSecretMapping(h string) map[string]string {
	key, err := filepath.Abs(h)
	if err != nil {
		key = h
	}
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	hydrateMu.Lock()
	due := !managerNow().Before(hydrateRetryAfter[key])
	hydrateMu.Unlock()
	if due && !hydrateSecretSources(h) {
		hydrateMu.Lock()
		hydrateRetryAfter[key] = managerNow().Add(hydrateRetryInterval)
		hydrateMu.Unlock()
	}
	return profileSecretMapping(h)
}

// Binds the connection owner's fresh profile secret scope, ctx unchanged for a single profile process
func installOwnerSecretScope(ctx context.Context) context.Context {
	h := ownerScopeHome(ctx)
	if h == "" {
		return ctx
	}
	return withSecretScope(ctx, ownerSecretMapping(h), h)
}

// Env whose home and secrets belong to the connection owner, env itself when it already does
func ownerEnv(ctx context.Context, env *tools.Env) *tools.Env {
	h := ownerScopeHome(ctx)
	if h == "" {
		return env
	}
	if env != nil && env.Secrets != nil && home.Key(filepath.Dir(env.Secrets.Path())) == home.Key(h) {
		return env
	}
	out := &tools.Env{}
	if env != nil {
		copied := *env
		out = &copied
	}
	out.Home = h
	out.Secrets = config.NewSecrets(h)
	return out
}

// Concurrent connects per discovery pass from mcp.discovery_concurrency, zero unlimited, invalid values warn and use four
func discoveryConnectConcurrency(ctx context.Context, env *tools.Env) int {
	const def = 4
	h := home.Dir(ctx)
	tree, err := config.Load(h, secretsLookup(env, h))
	if err != nil {
		return def
	}
	section, _ := tree["mcp"].(map[string]any)
	raw, ok := section["discovery_concurrency"]
	if !ok {
		return def
	}
	var n int64
	switch v := raw.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case uint64:
		n = int64(min(v, 1<<62))
	default:
		mcpLogger().Warn(fmt.Sprintf("mcp.discovery_concurrency=%s is not a non-negative integer; using %d", pyRepr(raw), def))
		return def
	}
	if n < 0 {
		mcpLogger().Warn(fmt.Sprintf("mcp.discovery_concurrency=%d is not a non-negative integer; using %d", n, def))
		return def
	}
	return int(min(n, 1<<31))
}

// Cross process discovery lock, waiting out another holder and running unguarded when locking is unavailable or the wait expires
func acquireDiscoveryLockWithRetry(ctx context.Context) *LockCookie {
	cookie, outcome := tryAcquireDiscoveryLock(ctx)
	if outcome != lockHeld {
		return cookie
	}
	mcpLogger().Debug("Another process holds MCP discovery lock -- retrying with backoff")
	for i := 0; i < discoveryLockMaxRetries; i++ {
		discoveryLockSleep()
		cookie, outcome = tryAcquireDiscoveryLock(ctx)
		if outcome != lockHeld {
			break
		}
	}
	switch outcome {
	case lockHeld:
		mcpLogger().Warn(fmt.Sprintf("MCP discovery lock still held after %d retries -- running discovery unguarded", discoveryLockMaxRetries))
	case lockAcquired:
		mcpLogger().Debug("Retry succeeded -- acquired MCP discovery lock")
	}
	return cookie
}
