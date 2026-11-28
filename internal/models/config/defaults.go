package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Registers the settings of the model list; providers gives the providers of the profile by name with their titles, and picks names the
// kind of thing a provider is to the pickers of the app
func Register(cfg *config.Config, providers func() []config.Option, picks string) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Model list",
		About: "Where the list of models and their limits comes from",
		Group: config.GroupModels,
		Order: 40,
		Settings: []config.Setting{
			{Key: Overrides, Title: "Limits and abilities", About: "Limits and abilities you set for models, listed by provider.",
				Help: "Each model of a provider, or every model of it, may set its context window, whether it uses tools, sees images and " +
					"reasons, and its model family.", Default: map[string]any{}, Custom: true, Picks: picks,
				Entry: &config.Entry{Noun: "provider", Name: "Provider"}},
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
			{Key: CatalogExcluded, Title: "Hidden providers", About: "Providers whose models the model list leaves out.", Default: []string{},
				Names: providers, Custom: true, Picks: picks},
			{Key: CatalogURL, Title: "Fetch address", About: "The address the online list of models is fetched from.", Default: DefaultCatalogURL,
				Zero: "The built-in address", Advanced: true},
			{Key: CatalogTTL, Title: "Refresh every", About: "How long a fetched list is used before it is fetched again.", Default: DefaultCatalogTTL * time.Minute,
				Zero: "1 minute", Unit: time.Minute},
			{Key: CatalogProviders, Title: "Provider catalogs", About: "Lists of models for single kinds of provider, used over the main list.",
				Default: map[string]any{}, Advanced: true, Custom: true, Entry: &config.Entry{Noun: "kind of provider", Name: "Kind of provider",
					Fields: []config.Setting{{Key: "url", Title: "List address", About: "The address of the list of models for this kind of provider.",
						Help: "It starts with http:// or https://.", Default: ""}}}},
		},
	}})
}
