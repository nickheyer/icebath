package plugins

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Route key of the session whose turn runs in ctx, empty outside one
func SessionKey(ctx context.Context) string {
	return tools.SessionEnv(tools.ToolEnvFrom(ctx), "ICEBATH_SESSION_KEY")
}

// Frozen launch policy platform and cwd of the turn running in ctx, false outside a policy
func activeSessionPolicy(ctx context.Context) (string, string, bool) {
	s := installedServices()
	if s == nil || s.ActiveSessionPolicy == nil {
		return "", "", false
	}
	return s.ActiveSessionPolicy(ctx)
}

// Client surface of a local session turn or the messaging platform of a chat turn, empty outside any session
func SessionSurface(ctx context.Context) string {
	if platform, _, ok := activeSessionPolicy(ctx); ok {
		return platform
	}
	return tools.SessionEnv(tools.ToolEnvFrom(ctx), "ICEBATH_SESSION_PLATFORM")
}

// Recorded working directory of the calling session, then the policy cwd, then the profile terminal cwd
func SessionCwd(ctx context.Context) (string, error) {
	env := tools.ToolEnvFrom(ctx)
	if cwd := tools.ScopedSessionCwd(env); cwd != "" {
		return cwd, nil
	}
	if _, cwd, ok := activeSessionPolicy(ctx); ok && cwd != "" {
		return cwd, nil
	}
	cwd, err := tools.ScopeTerminalCwd(env)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cwd), nil
}

// Session surface for a plugin context, cli for the classic CLI process itself
func contextSurface(ctx context.Context, m *Manager) (string, error) {
	m.mu.RLock()
	cli := m.cliRef
	m.mu.RUnlock()
	if cli != nil {
		return "cli", nil
	}
	return SessionSurface(ctx), nil
}

// Session cwd for a plugin context, the launch directory for the classic CLI process
func contextCwd(ctx context.Context, m *Manager) (string, error) {
	m.mu.RLock()
	cli := m.cliRef
	m.mu.RUnlock()
	if cli != nil {
		return os.Getwd()
	}
	return SessionCwd(ctx)
}
