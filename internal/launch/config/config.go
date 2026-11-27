package config

const (
	Key = "launch"
	// Troubleshooting mode a launch runs its sessions in: no customization at all, the profile's config, its project rules and memory, its
	// MCP servers, fallback models, plugins and hooks left out
	SafeMode = Key + ".safe_mode"
	// Longest the process's cleanup may take before it is ended anyway, doubled from a stop signal; zero for no watchdog
	ExitWatchdog = Key + ".exit_watchdog"
	// Wait after a stop signal interrupts a one-shot run before the run ends without its turn
	SignalGrace = Key + ".signal_grace"
)
