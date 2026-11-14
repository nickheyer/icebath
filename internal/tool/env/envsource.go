package env

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/tool"
)

var _ tool.EnvSource = (*Manager)(nil)

func (m *Manager) Active(ctx context.Context, taskID string) (tool.Environment, bool, error) {
	sh, ok, err := m.ActiveEnv(ctx, taskID)
	if err != nil || !ok || sh == nil {
		return nil, false, err
	}
	return sh, true, nil
}

func (m *Manager) Kind(ctx context.Context, taskID string) string {
	return m.BackendKind(ctx, taskID)
}

func (m *Manager) BackendKind(ctx context.Context, taskID string) string {
	key, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		key = taskID
	}
	m.mu.Lock()
	sh := m.active[key]
	if sh == nil {
		sh = m.active[taskID]
	}
	m.mu.Unlock()
	if sh != nil {
		if k := sh.EnvType(); k != "" {
			return strings.ToLower(k)
		}
	}
	m.settings.ensureBridged(ctx)
	kind, err := m.settings.tenv(ctx, "TERMINAL_ENV", "local")
	if err != nil {
		kind = os.Getenv("TERMINAL_ENV")
	}
	if kind == "" {
		kind = os.Getenv("TERMINAL_ENV")
	}
	if kind == "" {
		kind = "local"
	}
	return strings.ToLower(kind)
}
