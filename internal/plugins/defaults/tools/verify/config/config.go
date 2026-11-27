package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".verify"
	// Whether the agent is sent back to check the code it changed before it finishes a turn, the advice added to that, and how often a turn
	// may be sent back by the hooks that check its work
	OnStop    = Key + ".on_stop"
	Guidance  = Key + ".guidance"
	MaxNudges = Key + ".max_nudges"
)

// Values of on_stop
const (
	Auto = "auto"
	On   = "on"
	Off  = "off"
)
