package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: secretsconfig.Key,
		Title:  "Command helper",
		About:  "A fast, non-interactive command of your own that prints KEY=VALUE lines, run through /bin/sh as icebath starts",
		Settings: []config.Setting{
			{Key: Enabled, Title: "Helper secrets", About: "Load secrets from the helper command at startup", Default: false},
			{Key: Command, Title: "Helper command", About: "Command run through /bin/sh -c; it must print KEY=VALUE lines and nothing else, its errors are never shown", Default: ""},
			{Key: Helper, Title: "Helper time limit", About: "Seconds one run of the helper may take", Default: 3 * time.Second},
			{Key: Override, Title: "Helper wins", About: "Helper values replace matching values your shell or the .env set", Default: false},
			{Key: Timeout, Title: "Helper fetch limit", About: "Seconds the whole fetch may take before icebath starts without it", Default: 2 * time.Minute},
		},
	}})
}
