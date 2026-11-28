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
		About:  "Load API keys from a command of yours",
		Order:  30,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Load from a command", About: "Load API keys and other variables from your command.", Default: false, Custom: true},
			{Key: Command, Title: "Command", About: "The command that prints the variables, run through /bin/sh.",
				Help: "It must be quick, ask nothing and print NAME=value lines only, such as ~/bin/keys.sh. Its errors are never shown.", Default: ""},
			{Key: Override, Title: "Replace your values", About: "Replace values that your shell or keys file already set.", Default: false},
			{Key: Helper, Title: "Time per run", About: "How long one run of the command may take.", Default: 3 * time.Second, Zero: "3 seconds",
				Advanced: true},
			{Key: Timeout, Title: "Time limit", About: "How long loading may take before icebath goes on without it.", Default: 2 * time.Minute,
				Zero: "2 minutes", Advanced: true},
		},
	}})
}
