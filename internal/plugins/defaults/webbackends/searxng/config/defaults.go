package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "SearXNG",
		About:  "Free search through a SearXNG server that you run",
		Order:  70,
		Settings: []config.Setting{
			{Key: URL, Title: "Server address", About: "The address of your SearXNG server.", Help: "Such as http://localhost:8080.", Default: "",
				Env: []string{"SEARXNG_URL"}},
		},
	}})
}
