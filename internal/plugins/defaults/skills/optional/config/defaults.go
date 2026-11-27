package config

import (
	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: skillsconfig.Key,
		Title:  "Official optional skills",
		About:  "The heavier skills icebath ships for installing on request from the hub's official source",
		Settings: []config.Setting{
			{Key: Path, Title: "Source folder", About: "Folder to offer official optional skills from instead of the copy built into icebath", Default: "", Env: []string{"ICEBATH_OPTIONAL_SKILLS"}},
		},
	}})
}
