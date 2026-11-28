package delegation

import (
	"strings"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/delegation"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
)

// The default strategy: every task runs where the delegation settings say, else where the agent that delegated runs
type backend struct{}

func Register(registry *delegation.Registry) {
	registry.Register(backend{})
}

func (backend) Backends() []delegation.Backend {
	return []delegation.Backend{{
		Name:    delegation.Default,
		Title:   "Built-in",
		Summary: "Run subagents on the subagent settings, else on the agent's model",
		Route:   route,
	}}
}

// The delegation settings as the route of every task, the agent's fallback models while they name none of their own
func route(cfg *config.Config, _ delegation.Task) (delegation.Route, error) {
	text := func(key string) string { return strings.TrimSpace(cfg.GetString(key)) }
	r := delegation.Route{
		Model:           text(delegationconfig.Model),
		Provider:        text(delegationconfig.Provider),
		BaseURL:         text(delegationconfig.BaseURL),
		APIKey:          text(delegationconfig.APIKey),
		APIMode:         strings.ToLower(text(delegationconfig.APIMode)),
		ReasoningEffort: text(delegationconfig.ReasoningEffort),
	}
	if overrides, ok := cfg.Get(delegationconfig.RequestOverrides).(map[string]any); ok && len(overrides) > 0 {
		r.RequestOverrides = overrides
	}
	// No fallback models of their own leaves the agent's
	if fallbacks := cast.ToStringSlice(cfg.Get(delegationconfig.Fallbacks)); len(fallbacks) > 0 {
		r.Fallbacks = fallbacks
	}
	return r, nil
}
