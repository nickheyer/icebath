package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the package manager's settings
func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{
			Key:   Key,
			Title: "Packages",
			About: "Tools, runtimes and Python environments icebath downloads and keeps on this machine for its features",
			Settings: []config.Setting{
				{Key: LazyInstalls, Title: "Allow on-demand installs", About: "Let tools install missing add-ons the first time they are used. " +
					"ICEBATH_DISABLE_LAZY_INSTALLS=1 turns this off for every profile", Default: true},
			},
		},
	})
}
