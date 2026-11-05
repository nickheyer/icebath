package tools

// Child env for a delegated subprocess, the exported face for pkg/mcp
func DelegatedChildSubprocessEnv(env *Env, child map[string]string) map[string]string {
	return delegatedChildSubprocessEnv(env, child)
}

// Workspace root the session treats as authoritative, the exported face for pkg/mcp
func AuthoritativeWorkspaceRoot(env *Env) (string, error) { return authoritativeWorkspaceRoot(env) }

// Refusal text for an image embedded past its repeat cap, the exported face for pkg/mcp
func RepeatRefusal(env *Env, sessionID, imageURL string) string {
	return repeatRefusal(env, sessionID, imageURL)
}

// Start time fingerprint of a live process, the exported face for pkg/mcp
func ProcessStartTime(pid int) (int64, bool) { return processStartTime(pid) }

// Reports whether a recorded start time still names the same process, the exported face for pkg/mcp
func StartTimeFingerprintsMatch(recorded, current int64) bool {
	return startTimeFingerprintsMatch(recorded, current)
}

// Session key of the call's session with the default fallback, the exported face for pkg/mcp
func CurrentSessionKey(env *Env) string { return getCurrentSessionKey(env, "default") }

// Reports whether a field name denotes a secret, the exported face for pkg/mcp
func IsSecretFieldName(key string) bool { return isPythonReprSecretKey(key) }

// Header names that carry API credentials and are redacted from MCP displays
var SecretHeaderNameList = []string{"x-api-key", "x-goog-api-key", "api-key", "apikey", "x-api-token", "x-auth-token", "x-access-token"}

// Error text clamped to the display bound, the exported face for pkg/mcp
func BoundErrorText(text string) string { return boundErrorText(text) }
