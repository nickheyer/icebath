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
		About:  "The skills icebath ships, copied into each profile's skills folder and kept up to date there",
		Settings: []config.Setting{
			{Key: Path, Title: "Source folder", About: "Folder to seed bundled skills from instead of the copy built into icebath", Default: "", Env: []string{"ICEBATH_BUNDLED_SKILLS"}},
		},
	}})
}
