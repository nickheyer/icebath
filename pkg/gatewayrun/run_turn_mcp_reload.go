package gatewayrun

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Refreshes cached agents with the reloaded MCP tools, under multiplex only the profile's own sessions
func (r *GatewayRunner) MCPReloadRefreshCachedAgents(multiplex bool, profile string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Failed to update cached agent tools after MCP reload: %v", rec))
		}
	}()
	if r.agentCacheLock == nil || r.agentCache == nil || r.agentCache.Len() == 0 {
		return
	}
	prefix := ""
	if multiplex {
		prefix = gateway.SessionKeyNamespace(profile) + ":"
	}
	r.agentCacheLock.Lock()
	defer r.agentCacheLock.Unlock()
	for _, key := range r.agentCache.Keys() {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, _ := r.agentCache.Get(key)
		if entry.agent != nil {
			mcp.RefreshAgentMCPTools(context.Background(), entry.agent, mcp.RefreshOptions{Quiet: true})
		}
	}
}

// Sorted names joined with commas
func sortedNames(names map[string]bool) string {
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Disconnects, rediscovers and reports MCP servers, under multiplex only the requesting profile's
func (r *GatewayRunner) executeMCPReload(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	multiplex := r.multiplexing()
	if multiplex && home.Override(ctx) == "" {
		profileHome, err := r.resolveProfileHomeForSource(ctx, event.Source)
		if err != nil {
			return "", err
		}
		return r.executeMCPReload(gateway.ProfileRuntimeScope(ctx, profileHome, false), event)
	}
	reply, err := r.mcpReloadBody(ctx, event, multiplex)
	if err != nil {
		runLog().Warn(fmt.Sprintf("MCP reload failed: %s", pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.reload_mcp.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	return reply, nil
}

// MCP reload whose error becomes the failure reply
func (r *GatewayRunner) mcpReloadBody(ctx context.Context, event *gateway.MessageEvent, multiplex bool) (string, error) {
	var reloadScope *string
	if multiplex {
		key := tools.ScopeKey(home.Dir(ctx))
		reloadScope = &key
	}
	oldServers := mcp.ScopedServerNames(reloadScope)
	if _, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) {
		mcp.ShutdownMCPServers(ctx, reloadScope, nil, 15*time.Second)
		return nil, nil
	}); err != nil {
		return "", err
	}
	mcp.ReprobeToolAvailability()
	raw, err := r.runInExecutorWithContext(mcp.SuppressInteractiveOAuth(ctx), func(ctx context.Context) (any, error) {
		return mcp.ConnectAll(ctx, nil, nil)
	})
	if err != nil {
		return "", err
	}
	newTools, _ := raw.([]string)
	connected := mcp.ScopedServerNames(reloadScope)
	if reloadScope != nil {
		kept := []string{}
		for _, name := range newTools {
			if server, ok := mcp.ToolServerName(name); ok && connected[server] {
				kept = append(kept, name)
			}
		}
		newTools = kept
	}
	diff := func(a, b map[string]bool, both bool) map[string]bool {
		out := map[string]bool{}
		for n := range a {
			if b[n] == both {
				out[n] = true
			}
		}
		return out
	}
	changes := []struct {
		label, key string
		names      map[string]bool
	}{{"Reconnected", "gateway.reload_mcp.reconnected", diff(connected, oldServers, true)},
		{"Added", "gateway.reload_mcp.added", diff(connected, oldServers, false)},
		{"Removed", "gateway.reload_mcp.removed", diff(oldServers, connected, false)}}
	lines := []string{i18n.T(ctx, "gateway.reload_mcp.header", "", nil)}
	for _, c := range changes {
		if len(c.names) > 0 {
			lines = append(lines, i18n.T(ctx, c.key, "", map[string]any{"names": sortedNames(c.names)}))
		}
	}
	rawFailed, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) {
		return mcp.ServersNotConnected(ctx, nil), nil
	})
	if err != nil {
		return "", err
	}
	failed, _ := rawFailed.([]string)
	if len(connected) == 0 {
		lines = append(lines, i18n.T(ctx, "gateway.reload_mcp.none_connected", "", nil))
	} else {
		lines = append(lines, i18n.T(ctx, "gateway.reload_mcp.tools_available", "", map[string]any{"tools": len(newTools), "servers": len(connected)}))
	}
	if len(failed) > 0 {
		lines = append(lines, i18n.T(ctx, "gateway.reload_mcp.failed", "", map[string]any{"error": "did not connect: " + strings.Join(failed, ", ")}))
	}
	profile := ""
	if event.Source.Profile != nil {
		profile = *event.Source.Profile
	}
	r.MCPReloadRefreshCachedAgents(multiplex, profile)
	var parts []string
	for _, c := range []int{1, 2, 0} {
		if len(changes[c].names) > 0 {
			parts = append(parts, changes[c].label+" servers: "+sortedNames(changes[c].names))
		}
	}
	toolSummary := "No MCP tools available"
	if len(newTools) > 0 {
		toolSummary = fmt.Sprintf("%d MCP tool(s) now available", len(newTools))
	}
	changeDetail := ""
	if len(parts) > 0 {
		changeDetail = strings.Join(parts, ". ") + ". "
	}
	reloadMsg := map[string]any{"role": "user", "content": "[IMPORTANT: MCP servers have been reloaded. " + changeDetail + toolSummary +
		". The tool list for this conversation has been updated accordingly.]"}
	if entry, err := r.sessionStore.GetOrCreateSession(ctx, event.Source, false, true); err == nil {
		_ = r.sessionStore.AppendToTranscript(ctx, entry.SessionID, reloadMsg, false)
	}
	return strings.Join(lines, "\n"), nil
}
