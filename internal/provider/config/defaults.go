package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the section of the providers, and the sections of each provider of the profile under it
func Register(cfg *config.Config, providers config.Registrant) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Providers",
		About: "Accounts and API keys for the AI services you use",
		Group: config.GroupModels,
		Order: 10,
	}})
	cfg.Register(providers)
}
