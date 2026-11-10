package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Reports whether the server's run goroutine is still alive
func (s *serverTask) runAlive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// TimeoutError is a connect that outlived its connect_timeout, reported by class name like the bare Python exception
type TimeoutError struct{}

func (*TimeoutError) Error() string { return "" }

// Marks the error as a timeout for callers that test for one
func (*TimeoutError) Timeout() bool { return true }

// Connect timeout of one server entry
func connectTimeoutOf(cfg map[string]any) time.Duration {
	return time.Duration(floatOr(cfg, "connect_timeout", defaultConnectTimeout) * float64(time.Second))
}

// Builds the server task a connect starts, the seam tests replace to observe or wrap every task
var newServerTaskFn = newServerTask

// Starts a server task under the owner's secret scope and returns it once ready, shutting it down on failure unless claimed
func connectServer(ctx context.Context, env *tools.Env, name string, r *Rendered, claim func(*serverTask)) (*serverTask, error) {
	srv := newServerTaskFn(name, ownerEnv(ctx, env))
	if claim != nil {
		claim(srv)
	}
	scoped := installOwnerSecretScope(ctx)
	rendered, err := r.Rerender(scoped, srv.env)
	if err == nil {
		err = requireRenderedRemote(name, rendered.Config)
	}
	if err == nil {
		err = srv.start(scoped, rendered)
	}
	if err != nil {
		if claim == nil && !errors.Is(err, context.Canceled) {
			srv.shutdown(context.WithoutCancel(ctx))
		}
		return nil, err
	}
	return srv, nil
}

// Wakes a recycled stdio server and waits briefly for its fresh session
func requestLazyReconnect(ctx context.Context, serverName string, srv *serverTask) bool {
	var l *mcpLoop
	if srv.isRecycledStdio() {
		l = runningLoop()
	}
	if l == nil {
		return false
	}
	if err := l.callSoon(func() {
		srv.ready.Clear()
		srv.reconnect.Set()
	}); err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': lazy reconnect after stdio recycle failed: %s", serverName, excStr(err)))
		return false
	}
	ok, err := runOnLoop(ctx, recycledReconnectTimeout, func(c context.Context) (bool, error) {
		deadline := managerNow().Add(recycledReconnectTimeout)
		for managerNow().Before(deadline) {
			if srv.currentSession() != nil && srv.ready.IsSet() {
				return true, nil
			}
			select {
			case <-c.Done():
				return false, c.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
		return false, nil
	})
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': lazy reconnect after stdio recycle failed: %s", serverName, excStr(err)))
		return false
	}
	return ok
}

// Reports whether the entry defers its connect to first tool use
func resolveServerLazy(cfg map[string]any) bool { return parseBoolish(cfg["lazy"], false) }

// Records a failed connect for status and stamps its cooldown, returning the status text
func noteConnectFailure(ctx context.Context, name string, err error) string {
	message := formatConnectError(err)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	key := serverKey(ctx, name, "", true)
	delete(serverConnecting, key)
	serverConnectErrors[key] = message
	recordConnectFailure(ctx, name)
	return message
}

// Indented child stderr tail quoted on a connect failure log line, empty when the child wrote nothing
func stderrTailNote(ctx context.Context, name string) string {
	tail := tailServerStderr(ctx, name, 8192, 8)
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return "\n  child stderr tail (full log: mcp-stderr.log):\n" + strings.Join(lines, "\n")
}

// Clears connecting, error and cooldown state after a successful connect
func noteConnectSuccess(ctx context.Context, name string) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	key := serverKey(ctx, name, "", true)
	delete(serverConnecting, key)
	delete(serverConnectErrors, key)
	clearConnectFailure(ctx, name)
}

// Publishes the server under the connecting scope's key with that scope as its owner
func adoptServer(ctx context.Context, name string, srv *serverTask) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	key := serverKey(ctx, name, "", true)
	mcpServers[key] = srv
	serverScopeKeys[key] = mcpRegistryScope(ctx)
}

// Connects a lazily registered server on first use, honouring cooldown and dedup, and drops cached tools the live server lacks
func ensureLazyServerConnected(ctx context.Context, env *tools.Env, serverName string) bool {
	mcpMu.Lock()
	key := resolveServerKey(ctx, serverName, "", true)
	if srv := mcpServers[key]; srv != nil && srv.currentSession() != nil {
		mcpMu.Unlock()
		return true
	}
	cfg := lazyServerConfigs[key]
	_, connecting := serverConnecting[key]
	if len(cfg) == 0 || connectCooldownActive(ctx, serverName) || connecting {
		mcpMu.Unlock()
		return false
	}
	serverConnecting[key] = struct{}{}
	delete(serverConnectErrors, key)
	mcpMu.Unlock()
	mcpLogger().Info(fmt.Sprintf("MCP server '%s': lazy start on first use", serverName))
	ensureMCPLoop()
	_, err := runOnLoop(ctx, connectTimeoutOf(cfg)+30*time.Second, func(c context.Context) ([]string, error) {
		return discoverAndRegisterServer(c, env, serverName, &Rendered{Config: cfg})
	})
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("Lazy MCP connect failed for '%s': %s%s", serverName, noteConnectFailure(ctx, serverName, err), stderrTailNote(ctx, serverName)))
		return false
	}
	noteConnectSuccess(ctx, serverName)
	mcpMu.Lock()
	delete(lazyServerConfigs, key)
	staleFingerprint := lazyServerFingerprints[key]
	delete(lazyServerFingerprints, key)
	cachedNames := lazyServerToolNames[key]
	delete(lazyServerToolNames, key)
	srv := mcpServers[key]
	mcpMu.Unlock()
	live := map[string]bool{}
	if srv != nil {
		for _, n := range srv.registeredToolNames() {
			live[n] = true
		}
	}
	var phantom []string
	for _, n := range cachedNames {
		if !live[n] {
			phantom = append(phantom, n)
		}
	}
	if len(phantom) > 0 {
		for _, n := range phantom {
			deregisterMCPToolForKey(ctx, key, n)
		}
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': deregistered %d phantom cached tool(s) not served live (stale schema-cache fingerprint %s): %s",
			serverName, len(phantom), pyNoneOr(staleFingerprint), strings.Join(phantom, ", ")))
	}
	return srv != nil && srv.currentSession() != nil
}

// Python str of an optional string, None when empty
func pyNoneOr(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Connected server for a tool call, spawning a lazy server or waking a recycled stdio one first
func getConnectedServerForCall(ctx context.Context, env *tools.Env, serverName string) *serverTask {
	mcpMu.Lock()
	key := resolveServerKey(ctx, serverName, "", true)
	srv := mcpServers[key]
	_, isLazy := lazyServerConfigs[key]
	mcpMu.Unlock()
	switch {
	case isLazy && (srv == nil || srv.currentSession() == nil):
		ensureLazyServerConnected(ctx, env, serverName)
	case srv != nil && srv.currentSession() == nil && srv.isRecycledStdio():
		requestLazyReconnect(ctx, serverName, srv)
	default:
		return srv
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	return mcpServers[key]
}

// Connects one server and registers its tools, adopting a parked claimed task that outlived its connect deadline
func discoverAndRegisterServer(ctx context.Context, env *tools.Env, name string, r *Rendered) ([]string, error) {
	var claimed *serverTask
	cctx, cancel := context.WithTimeout(ctx, connectTimeoutOf(r.Config))
	srv, err := connectServer(cctx, env, name, r, func(s *serverTask) { claimed = s })
	cancelling := cctx.Err() != nil
	cancel()
	if err != nil {
		if claimed != nil {
			claimed.mu.Lock()
			parked := claimed.err != nil
			claimed.mu.Unlock()
			if parked && !cancelling && claimed.runAlive() {
				adoptServer(ctx, name, claimed)
			} else {
				claimed.shutdown(context.WithoutCancel(ctx))
			}
		}
		if cancelling && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = &TimeoutError{}
		}
		return nil, err
	}
	mcpMu.Lock()
	key := serverKey(ctx, name, "", true)
	delete(serverConnecting, key)
	delete(serverConnectErrors, key)
	mcpMu.Unlock()
	adoptServer(ctx, name, srv)
	names, err := registerServerTools(ctx, name, srv, r.Config)
	if err != nil {
		return nil, err
	}
	srv.mu.Lock()
	srv.toolNames = slices.Clone(names)
	srv.toolNamesSet = true
	srv.mu.Unlock()
	transport := "stdio"
	if _, ok := r.Config["url"]; ok {
		transport = "HTTP"
	}
	mcpLogger().Info(fmt.Sprintf("MCP server '%s' (%s): registered %d tool(s): %s", name, transport, len(names), strings.Join(names, ", ")))
	return names, nil
}

// Picks connect candidates, refreshes parallel safety and nudges known servers that lost their session
func selectNewServers(ctx context.Context, servers map[string]*Rendered) map[string]*Rendered {
	mcpMu.Lock()
	scope := mcpRegistryScope(ctx)
	keys := map[string]ServerKey{}
	for name := range servers {
		keys[name] = resolveServerKey(ctx, name, scope, false)
	}
	fresh := map[string]*Rendered{}
	var stale []*serverTask
	for name, r := range servers {
		k := keys[name]
		srv, live := mcpServers[k]
		_, connecting := serverConnecting[k]
		_, lazy := lazyServerConfigs[k]
		enabled := ServerEnabled(r.Config)
		if !live && !connecting && !lazy && enabled && !connectCooldownActive(ctx, name) {
			fresh[name] = r
		}
		if live && enabled && srv.currentSession() == nil {
			stale = append(stale, srv)
		}
	}
	for name := range fresh {
		serverConnecting[keys[name]] = struct{}{}
		serverScopeKeys[keys[name]] = scope
		delete(serverConnectErrors, keys[name])
	}
	for name, r := range servers {
		own := serverKey(ctx, name, scope, false)
		if parseBoolish(r.Config["supports_parallel_tool_calls"], false) {
			parallelSafeServers[own] = struct{}{}
		} else {
			delete(parallelSafeServers, own)
		}
	}
	mcpMu.Unlock()
	for _, srv := range stale {
		srv.mu.Lock()
		park := srv.authPark
		srv.mu.Unlock()
		if park == nil || !park.storage.CredentialStamp().Equal(park.stamp) {
			signalReconnect(srv)
		}
	}
	return fresh
}

// Registers lazy servers from a valid schema cache entry without connecting, returning the eager rest and the lazy counts
func registerLazyFromCache(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) (map[string]*Rendered, int, int) {
	eager := maps.Clone(fresh)
	lazyTools, lazyServers := 0, 0
	for _, name := range slices.Sorted(maps.Keys(fresh)) {
		r := fresh[name]
		if !resolveServerLazy(r.Config) {
			continue
		}
		entry := GetCachedEntry(ctx, name, ConfigFingerprint(r.Config))
		if len(entry) == 0 {
			continue
		}
		mcpMu.Lock()
		delete(serverConnecting, serverKey(ctx, name, "", true))
		mcpMu.Unlock()
		names, err := registerFromCacheSync(ctx, env, name, r.Config, entry)
		if err != nil {
			mcpLogger().Warn(fmt.Sprintf("Failed lazy MCP registration for '%s': %s", name, excStr(err)))
			mcpMu.Lock()
			serverConnecting[serverKey(ctx, name, "", true)] = struct{}{}
			mcpMu.Unlock()
			continue
		}
		delete(eager, name)
		lazyTools += len(names)
		lazyServers++
	}
	return eager, lazyTools, lazyServers
}

// Connects every candidate with bounded concurrency and records each outcome unless the pass itself was cancelled
func discoverAll(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) {
	limit := discoveryConnectConcurrency(ctx, env)
	var sem chan struct{}
	if limit > 0 {
		sem = make(chan struct{}, limit)
	}
	names := slices.Sorted(maps.Keys(fresh))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					errs[i] = ctx.Err()
					return
				}
			}
			_, errs[i] = discoverAndRegisterServer(ctx, env, name, fresh[name])
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	for i, name := range names {
		if errs[i] == nil {
			noteConnectSuccess(ctx, name)
			continue
		}
		command := ""
		if c, ok := fresh[name].Config["command"]; ok && c != nil && c != "" {
			command = fmt.Sprintf(" (command=%s)", pyStr(c))
		}
		message := noteConnectFailure(ctx, name, errs[i])
		mcpLogger().Warn(fmt.Sprintf("Failed to connect to MCP server '%s'%s: %s%s", name, command, message, stderrTailNote(ctx, name)))
	}
}

// Time one connect wave of a discovery pass may take, swapped by tests
var discoveryWaveTimeout = 120 * time.Second

// Discovery pass the register entry point runs, swapped by tests that adopt servers in place of connecting
var discoveryPass = runDiscoveryPass

// Pass budget of one wave per connect batch, capped so a slow pass never outlives the lock waiter
func discoveryPassTimeout(servers, limit int) time.Duration {
	if limit == 0 {
		limit = max(servers, 1)
	}
	waves := max(1, (servers+limit-1)/limit)
	return min(time.Duration(waves)*discoveryWaveTimeout, discoveryPassMax)
}

// Runs one bounded discovery pass on the MCP loop with the caller's interrupt parked, clearing stale connecting marks when it dies early
func runDiscoveryPass(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) error {
	worker := tools.WorkerOf(ctx)
	if tools.IsWorkerInterrupted(worker) {
		tools.SetInterrupt(worker, false, "")
		defer tools.SetInterrupt(worker, true, "")
	}
	timeout := discoveryPassTimeout(len(fresh), discoveryConnectConcurrency(ctx, env))
	_, err := runOnLoop(ctx, timeout, func(c context.Context) (struct{}, error) {
		discoverAll(c, env, fresh)
		return struct{}{}, nil
	})
	var timeoutErr *LoopTimeoutError
	how := ""
	switch {
	case errors.As(err, &timeoutErr):
		how = "timed out"
	case errors.Is(err, ErrInterrupted):
		how = "interrupted"
	default:
		return err
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	var stale []string
	for _, name := range slices.Sorted(maps.Keys(fresh)) {
		if _, ok := serverConnecting[serverKey(ctx, name, "", true)]; ok {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		mcpLogger().Warn(fmt.Sprintf("MCP discovery %s while %d server(s) were still connecting; clearing stale connecting set: %s", how, len(stale), strings.Join(stale, ", ")))
		for _, name := range stale {
			key := serverKey(ctx, name, "", true)
			delete(serverConnecting, key)
			if _, ok := serverConnectErrors[key]; !ok {
				serverConnectErrors[key] = "Connection attempt " + how + " during discovery"
			}
			recordConnectFailure(ctx, name)
		}
	}
	return err
}

// One failed candidate and why, the recorded connect error or the cooldown skip
type failedServer struct {
	name, reason string
}

// Tool count, connected count and failures of the candidates, lazy servers counted in
func connectedSummary(ctx context.Context, names []string, lazyTools, lazyServers int) (int, int, []failedServer) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	toolCount, connected := 0, 0
	var failed []failedServer
	for _, name := range names {
		key := serverKey(ctx, name, "", true)
		srv, live := mcpServers[key]
		_, errored := serverConnectErrors[key]
		if live && !errored {
			connected++
			toolCount += len(srv.registeredToolNames())
			continue
		}
		reason := serverConnectErrors[key]
		if reason == "" {
			reason = "not attempted (in retry cooldown)"
		}
		failed = append(failed, failedServer{name, reason})
	}
	return toolCount + lazyTools, connected + lazyServers, failed
}

// Logs the prefixed tool and server counts naming every failure when anything happened
func logSummary(ctx context.Context, prefix string, names []string, lazyTools, lazyServers int) {
	toolCount, connected, failed := connectedSummary(ctx, names, lazyTools, lazyServers)
	if toolCount == 0 && len(failed) == 0 && lazyServers == 0 {
		return
	}
	summary := fmt.Sprintf("%s %d tool(s) from %d server(s)", prefix, toolCount, connected)
	if len(failed) > 0 {
		parts := make([]string, len(failed))
		for i, f := range failed {
			parts[i] = f.name + " (" + f.reason + ")"
		}
		summary += fmt.Sprintf(" (%d failed: %s)", len(failed), strings.Join(parts, "; "))
	}
	if lazyServers > 0 {
		summary += fmt.Sprintf(" (%d lazy, not spawned yet)", lazyServers)
	}
	mcpLogger().Info(summary)
}

// Connects the given servers and registers their tools, logging a failed pass for the adopter re-registration caller
func registerMCPServers(ctx context.Context, env *tools.Env, servers map[string]*Rendered) []string {
	names, err := Connect(ctx, env, servers)
	if err != nil {
		mcpLogger().Debug(fmt.Sprintf("MCP: re-registration for profile scope %s failed: %s", pyNoneOr(mcpRegistryScope(ctx)), excStr(err)))
	}
	return names
}

// Connect registers the tools of every enabled, not yet connected server, idempotent for connected names, and returns every MCP tool name
func Connect(ctx context.Context, env *tools.Env, servers map[string]*Rendered) ([]string, error) {
	if SafeWorkerEnabled != nil && SafeWorkerEnabled(ctx) {
		return []string{}, nil
	}
	if !ensureMCPSDK() {
		mcpLogger().Debug("MCP SDK not available -- skipping explicit MCP registration")
		return []string{}, nil
	}
	raw := map[string]any{}
	for name, r := range servers {
		raw[name] = r.Config
	}
	kept := map[string]*Rendered{}
	for name := range filterSuspiciousServers(raw) {
		kept[name] = servers[name]
	}
	defer reregisterOrphanedAdopters(ctx, env)
	return connectServers(ctx, env, kept)
}

// Selects, lazily registers and connects candidates in one discovery pass, logging the summary line
func connectServers(ctx context.Context, env *tools.Env, servers map[string]*Rendered) ([]string, error) {
	healed, err := registerConnectedIntoCurrentScope(ctx, env, servers)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		mcpLogger().Debug("No explicit MCP servers provided")
		if mcpRegistryScope(ctx) != "" {
			return existingToolNames(ctx), nil
		}
		return []string{}, nil
	}
	fresh := selectNewServers(ctx, servers)
	if healed > 0 {
		mcpLogger().Info(fmt.Sprintf("MCP: registered %d already-connected server(s) into this profile scope", healed))
	}
	if len(fresh) == 0 {
		return existingToolNames(ctx), nil
	}
	fresh, lazyTools, lazyServers := registerLazyFromCache(ctx, env, fresh)
	if len(fresh) == 0 {
		if lazyTools > 0 {
			mcpLogger().Info(fmt.Sprintf("MCP: registered %d lazy tool(s) from schema cache (no processes spawned)", lazyTools))
		}
		return existingToolNames(ctx), nil
	}
	ensureMCPLoop()
	if err := discoveryPass(ctx, env, fresh); err != nil {
		return nil, err
	}
	logSummary(ctx, "MCP: registered", slices.Sorted(maps.Keys(fresh)), lazyTools, lazyServers)
	return existingToolNames(ctx), nil
}

// Configured servers loaded under the connection owner's secret scope
func ownerServerConfigs(ctx context.Context, env *tools.Env) map[string]*Rendered {
	return LoadServerConfigs(installOwnerSecretScope(ctx), ownerEnv(ctx, env))
}

// ConnectAll loads config and connects every configured server, or only the allowed names when allowed is non nil
func ConnectAll(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
	if SafeWorkerEnabled != nil && SafeWorkerEnabled(ctx) {
		return []string{}, nil
	}
	servers := ownerServerConfigs(ctx, env)
	if len(servers) == 0 {
		mcpLogger().Debug("No MCP servers configured")
		return []string{}, nil
	}
	if allowed != nil {
		allowedSet := map[string]bool{}
		for _, n := range allowed {
			allowedSet[n] = true
		}
		filtered := map[string]*Rendered{}
		var skipped []string
		for name, r := range servers {
			if allowedSet[name] {
				filtered[name] = r
			} else {
				skipped = append(skipped, name)
			}
		}
		if len(skipped) > 0 {
			sort.Strings(skipped)
			mcpLogger().Debug(fmt.Sprintf("MCP discovery filter: spawning %d/%d configured server(s) per --toolsets filter (skipped: %s)",
				len(filtered), len(servers), strings.Join(skipped, ",")))
		}
		servers = filtered
		if len(servers) == 0 {
			mcpLogger().Debug("No MCP servers in --toolsets filter; skipping MCP load entirely")
			return []string{}, nil
		}
	}
	if !ensureMCPSDK() {
		mcpLogger().Debug("MCP SDK not available -- skipping MCP tool discovery")
		return []string{}, nil
	}
	cookie := acquireDiscoveryLockWithRetry(ctx)
	defer cookie.Release()
	mcpMu.Lock()
	keys := map[string]ServerKey{}
	var newNames []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		k := resolveServerKey(ctx, name, "", true)
		keys[name] = k
		_, live := mcpServers[k]
		_, connecting := serverConnecting[k]
		if !live && !connecting && ServerEnabled(servers[name].Config) {
			newNames = append(newNames, name)
		}
	}
	priorLazy := map[ServerKey]bool{}
	for k := range lazyServerConfigs {
		priorLazy[k] = true
	}
	mcpMu.Unlock()
	toolNames, err := Connect(ctx, env, servers)
	if err != nil {
		return nil, err
	}
	if len(newNames) > 0 {
		mcpMu.Lock()
		lazyNow := map[string]bool{}
		lazyTools, newlyLazy := 0, 0
		for _, n := range newNames {
			if _, ok := lazyServerConfigs[keys[n]]; ok {
				lazyNow[n] = true
				if !priorLazy[keys[n]] {
					newlyLazy++
					lazyTools += len(lazyServerToolNames[keys[n]])
				}
			}
		}
		mcpMu.Unlock()
		var eager []string
		for _, n := range newNames {
			if !lazyNow[n] {
				eager = append(eager, n)
			}
		}
		logSummary(ctx, "  MCP:", eager, lazyTools, newlyLazy)
	}
	return toolNames, nil
}

// ReconcileResult names the servers a reconcile tore down, connected and could not yet tear down
type ReconcileResult struct {
	Removed []string `json:"removed"`
	Added   []string `json:"added"`
	Pending []string `json:"pending"`
}

// Reconcile tears down this scope's servers removed or disabled in config and connects enabled ones not yet live
func Reconcile(ctx context.Context, env *tools.Env) (ReconcileResult, error) {
	servers := ownerServerConfigs(ctx, env)
	wanted := map[string]bool{}
	for name, r := range servers {
		if ServerEnabled(r.Config) {
			wanted[name] = true
		}
	}
	scope := mcpRegistryScope(ctx)
	mcpMu.Lock()
	live, connecting := map[string]bool{}, map[string]bool{}
	for key, owner := range serverScopeKeys {
		if owner != scope {
			continue
		}
		if _, ok := mcpServers[key]; ok {
			live[key.Name] = true
		}
		if _, ok := serverConnecting[key]; ok {
			connecting[key.Name] = true
		}
	}
	var lazy []ServerKey
	for key := range lazyServerConfigs {
		if key.Scope == scope && !wanted[key.Name] {
			lazy = append(lazy, key)
		}
	}
	mcpMu.Unlock()
	var stale []string
	for name := range live {
		if !wanted[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		mcpLogger().Info(fmt.Sprintf("MCP server(s) %s no longer in config (or disabled); disconnecting", strings.Join(stale, ", ")))
		ShutdownMCPServers(ctx, &scope, stale, shutdownTimeout)
	}
	for _, key := range lazy {
		forgetLazyServer(ctx, key)
	}
	added := awaitingConnect(ctx, wanted, scope)
	if len(added) > 0 {
		if _, err := ConnectAll(ctx, env, nil); err != nil {
			return ReconcileResult{}, err
		}
	}
	removed := slices.Clone(stale)
	var lazyNames []string
	for _, key := range lazy {
		lazyNames = append(lazyNames, key.Name)
	}
	sort.Strings(lazyNames)
	removed = append(removed, lazyNames...)
	var pending []string
	for name := range connecting {
		if !wanted[name] {
			pending = append(pending, name)
		}
	}
	sort.Strings(pending)
	return ReconcileResult{Removed: nonNil(removed), Added: nonNil(added), Pending: nonNil(pending)}, nil
}

// Empty slice in place of nil so a result serializes as a list
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Grace each server gets to close when config changes tear it down
const shutdownTimeout = 15 * time.Second

// Wanted names with no live, connecting or lazy registration in scope
func unregistered(ctx context.Context, wanted map[string]bool, scope string) map[string]bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	out := map[string]bool{}
	for name := range wanted {
		key := resolveServerKey(ctx, name, scope, false)
		_, live := mcpServers[key]
		_, connecting := serverConnecting[key]
		_, lazy := lazyServerConfigs[key]
		if !live && !connecting && !lazy {
			out[name] = true
		}
	}
	return out
}

// Sorted unregistered names whose connect cooldown has lapsed
func awaitingConnect(ctx context.Context, wanted map[string]bool, scope string) []string {
	missing := unregistered(ctx, wanted, scope)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	var out []string
	for name := range missing {
		if !connectCooldownActive(ctx, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Enabled server names of the owner's config
func enabledConfigNames(ctx context.Context, env *tools.Env) map[string]bool {
	out := map[string]bool{}
	for name, r := range ownerServerConfigs(ctx, env) {
		if ServerEnabled(r.Config) {
			out[name] = true
		}
	}
	return out
}

// Wanted names that are unregistered, or registered but parked or failed, lazy included
func notConnected(ctx context.Context, wanted map[string]bool, scope string) map[string]bool {
	out := unregistered(ctx, wanted, scope)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	for name := range wanted {
		key := resolveServerKey(ctx, name, scope, false)
		_, errored := serverConnectErrors[key]
		if srv, live := mcpServers[key]; live {
			srv.mu.Lock()
			parked := srv.parkReason != ""
			srv.mu.Unlock()
			if errored || parked {
				out[name] = true
			}
			continue
		}
		if _, lazy := lazyServerConfigs[key]; lazy && errored {
			out[name] = true
		}
	}
	return out
}

// ServersNotConnected lists enabled servers this scope does not serve after a reload, parked and failed ones included
func ServersNotConnected(ctx context.Context, env *tools.Env) []string {
	out := slices.Collect(maps.Keys(notConnected(ctx, enabledConfigNames(ctx, env), mcpRegistryScope(ctx))))
	sort.Strings(out)
	return nonNil(out)
}

// ServersReady lists enabled servers this scope serves now, live or lazily registered and not failed
func ServersReady(ctx context.Context, env *tools.Env) []string {
	wanted, scope := enabledConfigNames(ctx, env), mcpRegistryScope(ctx)
	mcpMu.Lock()
	registered := map[string]bool{}
	for name := range wanted {
		key := resolveServerKey(ctx, name, scope, false)
		_, live := mcpServers[key]
		_, lazy := lazyServerConfigs[key]
		if live || lazy {
			registered[name] = true
		}
	}
	mcpMu.Unlock()
	failing := notConnected(ctx, wanted, scope)
	var out []string
	for name := range registered {
		if !failing[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return nonNil(out)
}

// ServersAwaitingConnect lists enabled servers the next ConnectAll would connect
func ServersAwaitingConnect(ctx context.Context, env *tools.Env) []string {
	return nonNil(awaitingConnect(ctx, enabledConfigNames(ctx, env), mcpRegistryScope(ctx)))
}

// Serializes restarts so a later save compares against what the earlier reconnect connected with
var restartMu sync.Mutex

// Restart reconnects each named live server whose resolved launch inputs changed, detaching borrowed connections, and returns the names restarted
func Restart(ctx context.Context, env *tools.Env, names []string) ([]string, error) {
	restartMu.Lock()
	defer restartMu.Unlock()
	scope := mcpRegistryScope(ctx)
	type target struct {
		name string
		key  ServerKey
	}
	var owned, borrowed []target
	scoped := installOwnerSecretScope(ctx)
	senv := ownerEnv(ctx, env)
	servers := LoadServerConfigs(scoped, senv)
	sorted := slices.Clone(names)
	sort.Strings(sorted)
	for _, name := range slices.Compact(sorted) {
		r := servers[name]
		mcpMu.Lock()
		key := resolveServerKey(ctx, name, scope, false)
		srv := mcpServers[key]
		mcpMu.Unlock()
		recorded := ""
		if srv != nil {
			srv.mu.Lock()
			recorded = srv.resolvedIdentity
			srv.mu.Unlock()
		}
		if r == nil || recorded == "" || !ServerEnabled(r.Config) {
			continue
		}
		rendered, err := r.Rerender(scoped, senv)
		if err != nil {
			return nil, err
		}
		if digest, ok := adopterIdentityDigest(scoped, senv, name, rendered); ok && digest == recorded {
			continue
		}
		if key == serverKey(ctx, name, scope, false) {
			owned = append(owned, target{name, key})
		} else {
			borrowed = append(borrowed, target{name, key})
		}
	}
	if len(owned) == 0 && len(borrowed) == 0 {
		return []string{}, nil
	}
	var changed, ownedNames []string
	for _, t := range owned {
		changed = append(changed, t.name)
		ownedNames = append(ownedNames, t.name)
	}
	for _, t := range borrowed {
		changed = append(changed, t.name)
	}
	sort.Strings(changed)
	mcpLogger().Info(fmt.Sprintf("MCP server(s) %s: launch inputs changed; reconnecting", strings.Join(changed, ", ")))
	if len(ownedNames) > 0 {
		ShutdownMCPServers(ctx, &scope, ownedNames, shutdownTimeout)
	}
	for _, t := range borrowed {
		removeServerScope(ctx, t.key, scope)
	}
	mcpMu.Lock()
	for _, name := range changed {
		clearConnectFailure(ctx, name)
	}
	mcpMu.Unlock()
	if _, err := ConnectAll(ctx, env, nil); err != nil {
		return changed, err
	}
	return changed, nil
}

// Drops a lazy registration whose config entry is gone so its cached tools stop spawning the server
func forgetLazyServer(ctx context.Context, key ServerKey) {
	mcpMu.Lock()
	delete(lazyServerConfigs, key)
	delete(lazyServerFingerprints, key)
	cached := lazyServerToolNames[key]
	delete(lazyServerToolNames, key)
	mcpMu.Unlock()
	for _, n := range cached {
		deregisterMCPToolForKey(ctx, key, n)
	}
}

// IsToolParallelSafe reports whether the tool's registering server opted into parallel tool calls
func IsToolParallelSafe(ctx context.Context, toolName string) bool {
	if !strings.HasPrefix(toolName, ToolNamePrefix) {
		return false
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	serverName := mcpToolServerNames[toolName]
	if serverName == "" {
		return false
	}
	_, ok := parallelSafeServers[serverKey(ctx, serverName, "", true)]
	return ok
}

// Lets the tool batch planner run calls of a parallel opted in MCP server concurrently
func init() { tools.MCPToolParallelSafe = IsToolParallelSafe }

// ServerStatus is one configured server's state for banners and the TUI
type ServerStatus struct {
	Name      string         `json:"name"`
	Transport string         `json:"transport"`
	Tools     int            `json:"tools"`
	Connected bool           `json:"connected"`
	Disabled  bool           `json:"disabled"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	Sampling  map[string]any `json:"sampling,omitempty"`
}

// samplingMetricser is a sampler that reports its request metrics
type samplingMetricser interface {
	Metrics() map[string]any
}

// Status reports each configured server as connected, disabled, connecting, failed, lazy or configured from cached state, never connecting
func Status(ctx context.Context, env *tools.Env, configured map[string]*Rendered, includeRuntime bool) []ServerStatus {
	if configured == nil {
		configured = ownerServerConfigs(ctx, env)
	}
	if len(configured) == 0 {
		return []ServerStatus{}
	}
	scope := mcpRegistryScope(ctx)
	mcpMu.Lock()
	visible := func(k ServerKey) bool { return includeRuntime && serverVisibleInScope(k, scope) }
	active := map[string]*serverTask{}
	for k, s := range mcpServers {
		if visible(k) {
			active[k.Name] = s
		}
	}
	connecting := map[string]bool{}
	for k := range serverConnecting {
		if visible(k) {
			connecting[k.Name] = true
		}
	}
	errs := map[string]string{}
	for k, e := range serverConnectErrors {
		if visible(k) {
			errs[k.Name] = e
		}
	}
	lazyTools := map[string]int{}
	for k, v := range lazyServerToolNames {
		if _, ok := lazyServerConfigs[k]; ok && includeRuntime && keyVisibleInScope(k, scope) {
			lazyTools[k.Name] = len(v)
		}
	}
	mcpMu.Unlock()
	out := make([]ServerStatus, 0, len(configured))
	for _, name := range slices.Sorted(maps.Keys(configured)) {
		cfg := configured[name].Config
		enabled := ServerEnabled(cfg)
		srv := active[name]
		live := srv != nil && srv.currentSession() != nil
		_, failed := errs[name]
		_, lazy := lazyTools[name]
		status := "configured"
		switch {
		case live:
			status = "connected"
		case !enabled:
			status = "disabled"
		case connecting[name]:
			status = "connecting"
		case failed:
			status = "failed"
		case lazy:
			status = "lazy"
		}
		transport := "stdio"
		if _, ok := cfg["url"]; ok {
			transport = "http"
			if t, ok := cfg["transport"]; ok {
				transport = pyStr(t)
			}
		}
		entry := ServerStatus{Name: name, Transport: transport, Disabled: status == "disabled", Status: status}
		switch {
		case live:
			entry.Connected = true
			entry.Tools = len(srv.registeredToolNames())
			srv.mu.Lock()
			sampler := srv.sampler
			srv.mu.Unlock()
			if m, ok := sampler.(samplingMetricser); ok {
				entry.Sampling = maps.Clone(m.Metrics())
			}
		case status == "failed":
			entry.Error = errs[name]
		case status == "lazy":
			entry.Tools = lazyTools[name]
		}
		out = append(out, entry)
	}
	return out
}

// ServerReconnecting reports whether this scope's connection was up once and is now healing from a transient outage
func ServerReconnecting(ctx context.Context, name string) bool {
	mcpMu.Lock()
	srv := mcpServers[resolveServerKey(ctx, name, "", true)]
	mcpMu.Unlock()
	if srv == nil || srv.currentSession() != nil {
		return false
	}
	srv.mu.Lock()
	ever, reason := srv.everConnected, srv.parkReason
	srv.mu.Unlock()
	if !ever || strings.Contains(reason, "permanent") {
		return false
	}
	return srv.runAlive()
}

// ToolSummary is one probed tool's name and description
type ToolSummary struct {
	Name        string
	Description string
}

// Probe connects each enabled server, lists its tools and disconnects, registering nothing and omitting failed servers
func Probe(ctx context.Context, env *tools.Env) map[string][]ToolSummary {
	if !ensureMCPSDK() {
		return map[string][]ToolSummary{}
	}
	enabled := map[string]*Rendered{}
	for name, r := range ownerServerConfigs(ctx, env) {
		if ServerEnabled(r.Config) {
			enabled[name] = r
		}
	}
	result := map[string][]ToolSummary{}
	if len(enabled) == 0 {
		return result
	}
	ensureMCPLoop()
	defer stopMCPLoopIfIdle()
	var mu sync.Mutex
	_, err := runOnLoop(ctx, 120*time.Second, func(c context.Context) (struct{}, error) {
		var wg sync.WaitGroup
		var probed []*serverTask
		for name, r := range enabled {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(c, connectTimeoutOf(r.Config))
				defer cancel()
				srv, err := connectServer(cctx, env, name, r, nil)
				if err != nil {
					mcpLogger().Debug(fmt.Sprintf("Probe: failed to connect to '%s': %s", name, excStr(err)))
					return
				}
				srv.mu.Lock()
				summaries := make([]ToolSummary, 0, len(srv.tools))
				for _, t := range srv.tools {
					summaries = append(summaries, ToolSummary{Name: t.Name, Description: t.Description})
				}
				srv.mu.Unlock()
				mu.Lock()
				probed = append(probed, srv)
				result[name] = summaries
				mu.Unlock()
			}()
		}
		wg.Wait()
		for _, srv := range probed {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.shutdown(context.WithoutCancel(c))
			}()
		}
		wg.Wait()
		return struct{}{}, nil
	})
	if err != nil {
		mcpLogger().Debug(fmt.Sprintf("MCP probe failed: %s", excStr(err)))
	}
	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(result)
}

// HasRegisteredTools reports whether any MCP server registered tools, not merely connected
func HasRegisteredTools() bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	return len(mcpToolServerNames) > 0
}

// RegisteredServerNames lists the servers that registered at least one tool
func RegisteredServerNames() map[string]bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	out := map[string]bool{}
	for _, s := range mcpToolServerNames {
		out[s] = true
	}
	return out
}

// ServerKeys lists the connection keys of every live MCP server task
func ServerKeys() []ServerKey {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	out := make([]ServerKey, 0, len(mcpServers))
	for k := range mcpServers {
		out = append(out, k)
	}
	return out
}

// Disconnect closes the named connections this scope owns and deregisters their tools
func Disconnect(ctx context.Context, names []string) {
	scope := mcpRegistryScope(ctx)
	ShutdownMCPServers(ctx, &scope, names, shutdownTimeout)
}
