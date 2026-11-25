package provider

import (
	"cmp"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/internal/config"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	entries := customconfig.Entries(p.config)
	list := make([]provider.Profile, 0, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		list = append(list, provider.Profile{
			Name:         name,
			DisplayName:  cmp.Or(customconfig.Field(entry, customconfig.DisplayName), name),
			Description:  "A user configured local or relay endpoint",
			BaseURL:      customconfig.Field(entry, customconfig.BaseURL),
			Mode:         provider.APIMode(cmp.Or(customconfig.Field(entry, customconfig.APIMode), string(provider.ModeChatCompletions))),
			Auth:         provider.AuthNone,
			DefaultModel: customconfig.Field(entry, customconfig.DefaultModel),
			Local:        true,
		})
	}
	return list
}
