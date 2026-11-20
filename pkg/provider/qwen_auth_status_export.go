package provider

// Qwen OAuth status of the status command, refresh validated so a stale CLI token never reads as logged in
func GetQwenAuthStatus(env PoolEnv) map[string]any { return getQwenAuthStatus(env) }
