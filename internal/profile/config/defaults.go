package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, profiles func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "Profiles",
		About:  "Separate sets of settings, providers and sessions",
		Group:  config.GroupApp,
		Order:  30,
		Settings: []config.Setting{
			{Key: Active, Title: "In use", About: "Which profile icebath uses now and the next time it starts.", Default: "default", Env: []string{PinnedEnv, ActiveEnv},
				Options: profiles, Custom: true},
		},
	}})
}
