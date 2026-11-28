package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "SSH",
		About:  "Another computer that commands reach over SSH",
		Order:  30,
		Settings: []config.Setting{
			{Key: Host, Title: "Host", About: "The computer that the agent's commands run on.", Help: "A host name or an IP address.", Default: "",
				Env: []string{"TERMINAL_SSH_HOST"}},
			{Key: User, Title: "User", About: "The account that commands sign in as.", Default: "", Env: []string{"TERMINAL_SSH_USER"}},
			{Key: Port, Title: "Port", About: "The port that SSH connects to.", Default: 22, Counts: config.Plain, Least: config.Num(1),
				Most: config.Num(65535), Env: []string{"TERMINAL_SSH_PORT"}},
			{Key: KeyFile, Title: "Key file", About: "The private key file that SSH signs in with.", Default: "", Zero: "SSH's usual keys",
				Path: &config.Path{Kind: config.PathFile, Exists: true},
				Env:  []string{"TERMINAL_SSH_KEY"}},
			{Key: Persistent, Title: "Keep exported variables", About: "Keep the variables that a remote command exports for the commands after it.",
				Help: "When off, each command starts fresh.", Default: true, Env: []string{"TERMINAL_PERSISTENT_SHELL"}},
		},
	}})
}
