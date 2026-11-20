package config

// Env text of a terminal config value, JSON for a list or mapping and the Python str otherwise
func TerminalEnvValueText(v any) string { return terminalEnvValueText(v) }
