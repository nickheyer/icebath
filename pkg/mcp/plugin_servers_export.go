package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Whether the connection the current scope resolves for a server holds a session or was recycled on purpose
func ServerConnected(ctx context.Context, serverName string) bool {
	mcpMu.Lock()
	srv := mcpServers[resolveServerKey(ctx, serverName, "", true)]
	mcpMu.Unlock()
	return srv != nil && (srv.currentSession() != nil || srv.isRecycledStdio())
}

// Tool names the resolved connection of a server registered plus the ones its schema cache holds
func ServerToolNames(ctx context.Context, serverName string) []string {
	mcpMu.Lock()
	key := resolveServerKey(ctx, serverName, "", true)
	srv := mcpServers[key]
	lazy := append([]string(nil), lazyServerToolNames[key]...)
	mcpMu.Unlock()
	var names []string
	if srv != nil {
		names = srv.registeredToolNames()
	}
	return append(names, lazy...)
}

// Unavailable state and its sentence for a declared server, ok false when it declares no app
func ServerUnavailableState(ctx context.Context, serverName, displayName string) (state, sentence string, ok bool) {
	decl := platformApps().Declaration(serverName)
	current := livenessStatus(ctx, serverName)
	if decl == nil || current == nil {
		return "", "", false
	}
	return current.State, describeUnavailable(decl, current.Availability, current.State, displayName), true
}

// Server config with every environment reference expanded the way the connect path renders it
func InterpolateServerConfig(ctx context.Context, env *tools.Env, cfg map[string]any) (any, error) {
	return interpolateEnvVars(ctx, env, cfg)
}
