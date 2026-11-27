package config

import secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"

// Name of the store and of its section
const Name = "command"

var (
	Key      = secretsconfig.Section(Name)
	Enabled  = secretsconfig.Of(Name, secretsconfig.Enabled)
	Override = secretsconfig.Of(Name, secretsconfig.Override)
	Timeout  = secretsconfig.Of(Name, secretsconfig.Timeout)
	Command  = Key + ".command"
	Helper   = Key + ".helper_timeout_seconds"
)
