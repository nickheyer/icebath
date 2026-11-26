package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the section of the providers, and the sections of each provider of the profile under it
func Register(cfg *config.Config, providers config.Registrant) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Providers",
		About: "Providers of this profile, with their accounts and models",
	}})
	cfg.Register(providers)
}
