package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, profiles func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "Profiles",
		About:  "Sets of providers, accounts and settings to switch between",
		Settings: []config.Setting{
			{Key: Active, Title: "Profile", About: "Profile in use", Default: "default", Env: []string{ActiveEnv}, Options: profiles, Internal: true},
		},
	}})
}
