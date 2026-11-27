package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".ssh"
	// Machine commands run on, and the account they log in as
	Host = Key + ".host"
	User = Key + ".user"
	Port = Key + ".port"
	// Private key file, the SSH agent and default keys when empty
	KeyFile = Key + ".key"
	// Exported variables carry from one command to the next
	Persistent = Key + ".persistent"
)
