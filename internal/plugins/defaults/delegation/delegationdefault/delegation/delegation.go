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
		Title:   "Default",
		Summary: "Every subagent uses delegation.model / delegation.provider, else the parent's model.",
		Route:   route,
	}}
}

// The delegation settings as the route of every task, the fallbacks inherited while they are unset
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
	if cfg.Stored(delegationconfig.Fallbacks) != nil {
		r.Fallbacks = cast.ToStringSlice(cfg.Get(delegationconfig.Fallbacks))
		if r.Fallbacks == nil {
			r.Fallbacks = []string{}
		}
	}
	return r, nil
}
