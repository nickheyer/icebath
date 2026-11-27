package config

const (
	Key = "bot_desktop"
	// Starts a hidden desktop on its own when a tool needs a screen, on a Linux machine with no display and the packages installed
	AutoStart = Key + ".auto_start"
	// Size of the screen, width x height
	Geometry = Key + ".geometry"
	// Minutes without use after which the screen stops, zero or less keeping it
	IdleStop = Key + ".idle_stop_minutes"
	// Free memory a start needs, in MB, zero turning the check off
	MinFreeMemory = Key + ".min_free_memory_mb"
	// Where the screen runs: decided by where commands run, with them, or on this machine
	Placement = Key + ".placement"
)

// Placements
const (
	Auto     = "auto"
	Terminal = "terminal"
	Gateway  = "gateway"
)

// Defaults the code falls back to for a value that is no number
const (
	DefaultGeometry      = "1440x900"
	DefaultIdleStop      = 30
	DefaultMinFreeMemory = 1536
)
