package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Sorted names in from that are absent from keep
func namesMissingFrom(from []string, keep map[string]bool) []string {
	var out []string
	for _, name := range from {
		if !keep[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Set view of a name list
func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// Deregisters names this server's toolset still owns, never a colliding name another server owns
func (s *serverTask) deregisterOwned(ctx context.Context, names []string) {
	toolset := "mcp-" + s.name
	scope := mcpRegistryScope(ctx)
	for _, name := range names {
		if d := tools.Default.Lookup(ctx, scope, name); d != nil && d.Toolset == toolset {
			deregisterMCPToolAllScopes(ctx, s, name)
		}
	}
}

// Re-lists tools after a list changed notification and swaps registry entries in place so live handlers survive
func (s *serverTask) refreshTools(ctx context.Context) error {
	if editorManifestFrozen(s) || !s.advertisesTools() {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	old := s.registeredToolNames()
	if err := s.rpc.Lock(ctx); err != nil {
		return err
	}
	session := s.currentSession()
	if session == nil {
		s.rpc.Unlock()
		mcpLogger().Debug(fmt.Sprintf("MCP server '%s': skipping dynamic tool refresh; session not connected", s.name))
		return nil
	}
	fresh, err := session.ListTools(ctx, nil)
	s.rpc.Unlock()
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, tool := range fresh {
		listed[PrefixedToolName(s.name, tool.Name)] = true
	}
	s.deregisterOwned(ctx, namesMissingFrom(old, listed))
	s.mu.Lock()
	s.tools = fresh
	s.mu.Unlock()
	registered, err := registerServerTools(ctx, s.name, s, s.currentConfig())
	if err != nil {
		return err
	}
	newNames := nameSet(registered)
	s.deregisterOwned(ctx, namesMissingFrom(old, newNames))
	s.mu.Lock()
	s.toolNames = registered
	s.mu.Unlock()
	var changes []string
	if added := namesMissingFrom(registered, nameSet(old)); len(added) > 0 {
		changes = append(changes, "added: "+strings.Join(added, ", "))
	}
	if removed := namesMissingFrom(old, newNames); len(removed) > 0 {
		changes = append(changes, "removed: "+strings.Join(removed, ", "))
	}
	if len(changes) > 0 {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': tools changed dynamically — %s. Verify these changes are expected.", s.name, strings.Join(changes, "; ")))
	} else {
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': dynamically refreshed %d tool(s) (no changes)", s.name, len(registered)))
	}
	return nil
}
