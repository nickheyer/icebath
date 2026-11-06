package plugins

import (
	"context"
	"os"
)

// Session surface for a plugin context, cli for the classic CLI process itself
func contextSurface(ctx context.Context, m *Manager) (string, error) {
	m.mu.RLock()
	cli := m.cliRef
	m.mu.RUnlock()
	if cli != nil {
		return "cli", nil
	}
	fn, err := needService("SessionSurface", func(s *Services) func(context.Context, string) string { return s.SessionSurface })
	if err != nil {
		return "", err
	}
	return fn(ctx, m.home), nil
}

// Session cwd for a plugin context, the launch directory for the classic CLI process
func contextCwd(ctx context.Context, m *Manager) (string, error) {
	m.mu.RLock()
	cli := m.cliRef
	m.mu.RUnlock()
	if cli != nil {
		return os.Getwd()
	}
	fn, err := needService("SessionCwd", func(s *Services) func(context.Context, string) string { return s.SessionCwd })
	if err != nil {
		return "", err
	}
	return fn(ctx, m.home), nil
}
