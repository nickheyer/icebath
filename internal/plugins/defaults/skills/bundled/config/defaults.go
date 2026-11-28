package config

import (
	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: skillsconfig.Key,
		Into:   skillsconfig.Key,
		Title:  "Bundled skills",
		About:  "Skills that come with icebath and stay up to date",
		Order:  40,
		Settings: []config.Setting{
			{Key: Path, Title: "Bundled skills folder", About: "The folder that the skills icebath comes with are copied from.",
				Default: "", Zero: "Built-in copy",
				Env: []string{"ICEBATH_BUNDLED_SKILLS"}, Path: &config.Path{Kind: config.PathFolder, Exists: true}, Advanced: true},
		},
	}})
}
