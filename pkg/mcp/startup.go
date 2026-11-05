package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultDiscoveryWait            = 1500 * time.Millisecond
	defaultSingleQueryDiscoveryWait = 15 * time.Second
	defaultDiscoveryThreadName      = "cli-mcp-discovery"
)

// Discovery slots per profile home key, the armed deferred start and the toolsets spawn allowlist
var (
	discoveryMu       sync.Mutex
	discoveryStarted  = map[string]bool{}
	discoveryRuns     = map[string]*discoveryRun{}
	discoveryDeferred *deferredDiscovery
	serverFilter      []string
)

// Discovery entry point the background goroutine runs, swapped by tests
var discoverMCPTools = ConnectAll

// discoveryRun is one background discovery goroutine whose done channel closes when it returns
type discoveryRun struct {
	done chan struct{}
}

// Reports whether the goroutine is still running
func (r *discoveryRun) alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// Blocks until the run ends, the timeout lapses or ctx ends, nil timeout waits without bound
func (r *discoveryRun) join(ctx context.Context, timeout *time.Duration) {
	var expired <-chan time.Time
	if timeout != nil {
		t := time.NewTimer(max(*timeout, 0))
		defer t.Stop()
		expired = t.C
	}
	select {
	case <-r.done:
	case <-expired:
	case <-ctx.Done():
	}
}

// deferredDiscovery holds the start an armed deferral fires, with its timer when a delay was given
type deferredDiscovery struct {
	fire  func()
	timer *time.Timer
}

// Profile home key the caller is scoped to
func discoveryHomeKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

// Stores the MCP spawn allowlist from a toolsets value, all or star or empty clears it
func SetServerFilter(toolsets any) []string {
	var names []string
	split := func(s string) {
		for _, t := range strings.Split(s, ",") {
			if t = strings.TrimSpace(t); t != "" {
				names = append(names, t)
			}
		}
	}
	switch v := toolsets.(type) {
	case string:
		split(v)
	case []string:
		for _, item := range v {
			split(item)
		}
	case []any:
		for _, item := range v {
			split(pyStr(item))
		}
	}
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	if len(names) == 0 || containsString(names, "all") || containsString(names, "*") {
		serverFilter = nil
	} else {
		serverFilter = names
	}
	return serverFilter
}

// Current MCP spawn allowlist, nil spawns every configured server
func ServerFilter() []string {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	return serverFilter
}

// Reports configured MCP servers or enabled plugin MCP packages, true when the config cannot be read
func hasConfiguredServers(ctx context.Context) bool {
	h := home.Dir(ctx)
	raw, err := config.ReadUser(h)
	if err != nil {
		return true
	}
	if servers, ok := raw["mcp_servers"].(map[string]any); ok && len(servers) > 0 {
		return true
	}
	portable, err := pluginPortableServers(h)
	if err != nil {
		return true
	}
	return len(portable) > 0
}

// Reports whether a discovery run left a live session or a lazy registration behind
func discoveryRegisteredServers(status []ServerStatus) bool {
	for _, entry := range status {
		if entry.Connected || entry.Status == "lazy" {
			return true
		}
	}
	return false
}

// Reports whether any configured server is connected or lazily registered
func anyMCPConnected(ctx context.Context, env *tools.Env) bool {
	return discoveryRegisteredServers(Status(ctx, env, nil, true))
}

// Enabled servers still awaiting a connect, limited to the spawn allowlist
func serversAwaitingConnectFiltered(ctx context.Context, env *tools.Env, filter []string) []string {
	pending := ServersAwaitingConnect(ctx, env)
	if filter == nil {
		return pending
	}
	var kept []string
	for _, n := range pending {
		if containsString(filter, n) {
			kept = append(kept, n)
		}
	}
	return kept
}

// Starts one background discovery per profile home, retrying when the last run connected nothing or missed new servers
func StartBackgroundDiscovery(ctx context.Context, env *tools.Env, logger *slog.Logger, threadName string) {
	if logger == nil {
		logger = mcpLogger()
	}
	if threadName == "" {
		threadName = defaultDiscoveryThreadName
	}
	key := discoveryHomeKey(ctx)
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	if discoveryStarted[key] {
		if run := discoveryRuns[key]; run != nil && run.alive() {
			return
		}
		connected := anyMCPConnected(ctx, env)
		var pending []string
		if connected {
			pending = serversAwaitingConnectFiltered(ctx, env, serverFilter)
		}
		if connected && len(pending) == 0 {
			return
		}
		if connected {
			logger.Info(fmt.Sprintf("MCP server(s) %s not connected yet; running discovery", strings.Join(pending, ", ")))
		} else {
			logger.Warn("Background MCP discovery previously exited with no connected servers; retrying discovery thread")
		}
		delete(discoveryStarted, key)
		delete(discoveryRuns, key)
	}
	discoveryStarted[key] = true
	if !hasConfiguredServers(ctx) {
		return
	}
	run := &discoveryRun{done: make(chan struct{})}
	discoveryRuns[key] = run
	go runBackgroundDiscovery(context.WithoutCancel(ctx), env, logger.With("thread", threadName), key, run, serverFilter)
}

// Runs discovery without interactive OAuth, warns when nothing connected and frees the home slot
func runBackgroundDiscovery(ctx context.Context, env *tools.Env, logger *slog.Logger, key string, run *discoveryRun, filter []string) {
	defer func() {
		discoveryMu.Lock()
		if discoveryRuns[key] == run {
			delete(discoveryRuns, key)
		}
		discoveryMu.Unlock()
		close(run.done)
	}()
	defer func() {
		if r := recover(); r != nil {
			logger.Debug("Background MCP tool discovery failed", "error", fmt.Sprint(r))
		}
	}()
	if _, err := discoverWithoutInteractiveOAuth(ctx, env, filter); err != nil {
		logger.Debug("Background MCP tool discovery failed", "error", err)
		return
	}
	if !anyMCPConnected(ctx, env) {
		logger.Warn("Background MCP discovery completed with zero connected servers")
	}
}

// Runs MCP discovery with OAuth barred from prompting on stdin, honouring the spawn allowlist
func discoverWithoutInteractiveOAuth(ctx context.Context, env *tools.Env, filter []string) ([]string, error) {
	return discoverMCPTools(SuppressInteractiveOAuth(ctx), env, filter)
}

// Discovery wait bound from the explicit value, then config.yaml, then the shipped default
func resolveDiscoveryTimeout(ctx context.Context, env *tools.Env, explicit *time.Duration, singleQuery bool) time.Duration {
	if explicit != nil {
		return *explicit
	}
	key, fallback := "mcp_discovery_timeout", defaultDiscoveryWait
	if singleQuery {
		key, fallback = "mcp_single_query_discovery_timeout", defaultSingleQueryDiscoveryWait
	}
	def := fallback
	if raw, ok := config.Defaults()[key]; ok {
		f, ok := pyFloat(raw)
		if !ok {
			return fallback
		}
		def = secondsDuration(f)
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, secretsLookup(env, h))
	if err != nil {
		return def
	}
	raw, ok := tree[key]
	if !ok {
		return def
	}
	f, ok := pyFloat(raw)
	if !ok || f <= 0 {
		return def
	}
	return secondsDuration(f)
}

// Arms a background discovery start after delay, nil delay arms it for the first client or agent build
func DeferBackgroundDiscovery(ctx context.Context, env *tools.Env, logger *slog.Logger, threadName string, delay *time.Duration) {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	if discoveryStarted[discoveryHomeKey(ctx)] || discoveryDeferred != nil {
		return
	}
	startCtx := context.WithoutCancel(ctx)
	d := &deferredDiscovery{}
	d.fire = func() {
		discoveryMu.Lock()
		if discoveryDeferred == d {
			discoveryDeferred = nil
		}
		discoveryMu.Unlock()
		StartBackgroundDiscovery(startCtx, env, logger, threadName)
	}
	discoveryDeferred = d
	if delay != nil {
		d.timer = time.AfterFunc(max(*delay, 0), d.fire)
	}
}

// Fires an armed deferred discovery start at once, a racing second caller finds the slot empty
func StartDeferredDiscoveryNow() {
	discoveryMu.Lock()
	d := discoveryDeferred
	discoveryDeferred = nil
	discoveryMu.Unlock()
	if d == nil {
		return
	}
	if d.timer != nil {
		d.timer.Stop()
	}
	d.fire()
}

// Fires a deferred start then waits for the home's discovery up to the resolved bound
func WaitForDiscovery(ctx context.Context, env *tools.Env, timeout *time.Duration, singleQuery bool) {
	StartDeferredDiscoveryNow()
	run := currentHomeRun(ctx)
	if run == nil || !run.alive() {
		return
	}
	bound := resolveDiscoveryTimeout(ctx, env, timeout, singleQuery)
	run.join(ctx, &bound)
}

// Discovery goroutine of the caller's profile home, nil when none was started
func currentHomeRun(ctx context.Context) *discoveryRun {
	key := discoveryHomeKey(ctx)
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	return discoveryRuns[key]
}

// Reports whether this home's discovery goroutine is still running
func DiscoveryInFlight(ctx context.Context) bool {
	run := currentHomeRun(ctx)
	return run != nil && run.alive()
}

// Waits up to timeout for this home's discovery, true once complete and false while it still runs
func JoinDiscovery(ctx context.Context, timeout *time.Duration) bool {
	run := currentHomeRun(ctx)
	if run == nil {
		return true
	}
	run.join(ctx, timeout)
	return !run.alive()
}

// Starts discovery when needed and gives configured MCP tools a bounded chance to register before an agent builds
func EnsureDiscoveryBeforeAgentBuild(ctx context.Context, env *tools.Env, logger *slog.Logger, timeout *time.Duration, singleQuery bool, threadName string) {
	if logger == nil {
		logger = mcpLogger()
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Debug("MCP discovery readiness check failed before agent build", "error", fmt.Sprint(r))
		}
	}()
	StartBackgroundDiscovery(ctx, env, logger, threadName)
	waitForDiscoveryHook(ctx, env, timeout, singleQuery)
}

// Wait step the readiness helper runs, swapped by tests to observe the single query flag
var waitForDiscoveryHook = WaitForDiscovery
