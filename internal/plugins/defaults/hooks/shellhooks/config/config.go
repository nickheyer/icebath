package config

import hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"

const (
	Key = hooksconfig.Key + ".shell"
	// Commands to run on agent events, a list of definitions by event name
	Events = Key + ".events"
	// Every hook the events name runs without asking first
	AutoAccept = Key + ".auto_accept"
	// Addresses agent events are posted to, a table of targets by a key of their own
	Outbound = Key + ".outbound"
)

// Fields of one hook of the events setting
const (
	Command = "command"
	Matcher = "matcher"
	Timeout = "timeout"
	Block   = "fail_closed"
)

// Fields of one webhook of the outbound setting, besides the matcher and the time limit a hook has too
const (
	URL       = "url"
	Posted    = "events"
	Name      = "name"
	SecretEnv = "secret_env"
	Secret    = "secret"
)

// Kind of thing a value of tools names, a pattern of the names of the tools a hook or a webhook is for, by the name the hooks page registers
// its picker under
const PickTools = "tool-pattern"
