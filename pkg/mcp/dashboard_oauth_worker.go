package mcp

import (
	"context"
)

// Tool name and description rows the flow approved, guarded for the status poller
func (f *DashboardOAuthFlow) ToolsSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Tools
}

// Records the name and description rows of the tools an approved flow found
func (f *DashboardOAuthFlow) SetTools(summaries []ToolSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tools = summaryDicts(summaries)
}

// Evicts the provider and deletes the server's OAuth files, returning the evicted entry
func (m *OAuthManager) Remove(ctx context.Context, serverName, homeDir string) (*providerEntry, error) {
	return m.remove(ctx, serverName, homeDir)
}

// Puts back an entry removed for a failed reauthorization unless a newer one took its place
func (m *OAuthManager) RestoreEntry(ctx context.Context, serverName string, e *providerEntry, homeDir string) {
	m.restoreEntry(ctx, serverName, e, homeDir)
}
