package config

import (
	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Files",
		About:  "Read, write, edit and search files",
		Settings: []config.Setting{
			{Key: ReadMaxChars, Title: "Max read size", About: "Max characters returned by one file read", Default: 100000},
		},
	}})
}
