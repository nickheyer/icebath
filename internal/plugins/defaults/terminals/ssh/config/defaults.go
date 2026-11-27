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
		About:  "Commands run on a remote machine over SSH",
		Settings: []config.Setting{
			{Key: Host, Title: "SSH host", About: "Machine the agent's commands run on when they run over SSH, a hostname or an IP address", Default: "", Env: []string{"TERMINAL_SSH_HOST"}},
			{Key: User, Title: "SSH user", About: "Account to log in as over SSH", Default: "", Env: []string{"TERMINAL_SSH_USER"}},
			{Key: Port, Title: "SSH port", About: "Port to connect to over SSH, 1 to 65535", Default: 22, Env: []string{"TERMINAL_SSH_PORT"}},
			{Key: KeyFile, Title: "SSH key file", About: "Private key file used to log in over SSH; empty uses your SSH agent and default keys", Default: "", Env: []string{"TERMINAL_SSH_KEY"}},
			{Key: Persistent, Title: "Persistent SSH shell", About: "Keep the variables a remote command exports for the commands after it; off starts each command fresh", Default: true, Env: []string{"TERMINAL_PERSISTENT_SHELL"}},
		},
	}})
}
