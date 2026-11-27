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
		Title:  "Your own command",
		About:  "Load API keys from a command of yours when icebath starts",
		Order:  30,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Load from a command", About: "Load API keys and other variables from your command when icebath starts.", Default: false,
				Apply: config.Restart},
			{Key: Command, Title: "Helper command", About: "The command that prints the variables, run through /bin/sh.",
				Help: "It must be quick, ask nothing and print KEY=VALUE lines only. Its errors are never shown.", Default: "", Apply: config.Restart},
			{Key: Override, Title: "Replace your values", About: "Replace values that your shell or keys file already set.", Default: false,
				Apply: config.Restart},
			{Key: Helper, Title: "Time per run", About: "How long one run of the command may take.", Default: 3 * time.Second, Zero: "3 seconds",
				Apply: config.Restart, Advanced: true},
			{Key: Timeout, Title: "Time limit", About: "How long loading may take before icebath starts without it.", Default: 2 * time.Minute,
				Zero: "2 minutes", Apply: config.Restart, Advanced: true},
		},
	}})
}
