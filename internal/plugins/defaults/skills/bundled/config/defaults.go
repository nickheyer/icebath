package config

import (
	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: skillsconfig.Key,
		Title:  "Bundled skills",
		About:  "Skills that come with icebath and stay up to date",
		Order:  40,
		Settings: []config.Setting{
			{Key: Path, Title: "Source folder", About: "The folder that bundled skills are copied from.", Default: "", Zero: "Built-in copy",
				Env: []string{"ICEBATH_BUNDLED_SKILLS"}, Advanced: true},
		},
	}})
}
