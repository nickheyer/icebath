package app

// Persisted fast mode preference parsed like the CLI, empty when unset or unknown
func ParseServiceTierConfig(raw string) string { return parseServiceTierConfig(raw) }
