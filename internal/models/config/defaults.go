package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Model list",
		About: "Where the list of models and their limits comes from",
		Group: config.GroupModels,
		Order: 40,
		Settings: []config.Setting{
			{Key: Overrides, Title: "Limits and abilities", About: "Limits and abilities you set for models, listed by provider.",
				Help: "Each model, or _default for those nothing else knows, may set context_window, supports_tools, supports_vision, " +
					"supports_reasoning and model_family.", Default: map[string]any{}, Advanced: true,
				Entry: &config.Entry{Noun: "provider", Name: "Provider name"}},
			{Key: DevURL, Title: "Details address", About: "The address that details of models, such as limits and prices, are read from.",
				Default: "", Zero: "Public models.dev", Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    CatalogKey,
		Parent: Key,
		Title:  "Online list",
		About:  "The online list of the models that pickers offer",
		Order:  10,
		Settings: []config.Setting{
			{Key: CatalogEnabled, Title: "Fetch online", About: "Fetch the list of models that pickers offer from the internet.", Default: true},
			{Key: CatalogExcluded, Title: "Hidden providers", About: "Providers left out of the model picker.", Default: []string{}},
			{Key: CatalogURL, Title: "Fetch address", About: "The address the online list of models is fetched from.", Default: DefaultCatalogURL,
				Zero: "The built-in address", Advanced: true},
			{Key: CatalogTTL, Title: "Refresh every", About: "How long a fetched list is used before it is fetched again.", Default: DefaultCatalogTTL * time.Minute,
				Zero: "1 minute", Unit: time.Minute, Advanced: true},
			{Key: CatalogProviders, Title: "Provider catalogs", About: "Lists of models for single providers, used over the main list.",
				Default: map[string]any{}, Advanced: true, Entry: &config.Entry{Noun: "provider", Name: "Provider name", Fields: []config.Setting{
					{Key: "url", Title: "List address", About: "The address of the list of models for this provider.", Default: ""}}}},
		},
	}})
}
