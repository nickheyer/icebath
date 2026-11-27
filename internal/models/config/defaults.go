package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Models",
		About: "What icebath knows of models: their limits, abilities and prices, and the catalogs the pickers offer",
		Settings: []config.Setting{
			{Key: DevURL, Title: "Model details address", About: "Web address for model details such as limits and prices; empty uses the public models.dev registry",
				Default: ""},
			{Key: Overrides, Title: "Model overrides", About: "Per-model limits such as context size, by provider: each model, or _default for models nothing else knows, " +
				"may set context_window, supports_tools, supports_vision, supports_reasoning and model_family", Default: map[string]any{}},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    CatalogKey,
		Parent: Key,
		Title:  "Model catalog",
		About:  "Online list of the models the pickers offer, refreshed while icebath runs",
		Settings: []config.Setting{
			{Key: CatalogEnabled, Title: "Online model list", About: "Fetch the list of available models from the online catalog", Default: true},
			{Key: CatalogURL, Title: "Model list address", About: "Web address the online model list is fetched from; empty uses the built-in one", Default: DefaultCatalogURL},
			{Key: CatalogTTL, Title: "Model list refresh", About: "Minutes between refreshes of the online model list, at least one", Default: DefaultCatalogTTL * time.Minute,
				Unit: time.Minute},
			{Key: CatalogProviders, Title: "Provider overrides", About: "Per-provider changes applied to the online model list: a provider's url names a catalog of its own",
				Default: map[string]any{}},
			{Key: CatalogExcluded, Title: "Hidden providers", About: "Providers left out of the model picker", Default: []string{}},
		},
	}})
}
