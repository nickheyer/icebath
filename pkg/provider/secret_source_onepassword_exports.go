package provider

// Sanitized stderr text of an op invocation with escape sequences removed
func OpScrub(text string) string { return opScrub(text) }

// Copy of the environment names an op child process may inherit
func OpEnvAllowlist() []string { return append([]string(nil), opEnvAllowlist...) }
