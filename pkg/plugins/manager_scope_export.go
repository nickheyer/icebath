package plugins

import "github.com/nickheyer/icebath/pkg/home"

// Home key the manager is scoped to
func (m *Manager) ScopeKey() string { return home.Key(m.home) }
