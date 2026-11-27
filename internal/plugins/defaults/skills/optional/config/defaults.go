package config

import (
	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: skillsconfig.Key,
		Title:  "Optional skills",
		About:  "Larger skills from icebath that you install when you want",
		Order:  50,
		Settings: []config.Setting{
			{Key: Path, Title: "Source folder", About: "The folder that optional skills are offered from.", Default: "", Zero: "Built-in copy",
				Env: []string{"ICEBATH_OPTIONAL_SKILLS"}, Advanced: true},
		},
	}})
}
