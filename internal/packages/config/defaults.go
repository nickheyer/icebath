package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the package manager's settings
func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{
			Key:   Key,
			Title: "Downloads",
			About: "Whether tools may download what they need",
			Group: config.GroupApp,
			Order: 50,
			Settings: []config.Setting{
				{Key: LazyInstalls, Title: "Download when needed", About: "Let tools download what they are missing the first time they are used.",
					Help: "Setting ICEBATH_DISABLE_LAZY_INSTALLS=1 turns this off for every profile.", Default: true},
			},
		},
	})
}
