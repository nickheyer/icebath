package tools

// Session derived key prefixed with the routed profile home of env, unchanged without one
func QualifyTaskKey(env *Env, key string) string { return qualifyTaskKey(env, key) }

// Drops the session workspace override registered for a task and the cwd record it seeded, keeping overrides others registered
func UnbindTurnWorkspace(env *Env, taskID string) {
	if taskID == "" {
		return
	}
	key := qualifyTaskKey(env, taskID)
	taskEnvOverrides.Lock()
	current := taskEnvOverrides.m[key]
	if _, tagged := current["workspace_key"]; !tagged {
		taskEnvOverrides.Unlock()
		return
	}
	delete(taskEnvOverrides.m, key)
	taskEnvOverrides.Unlock()
	var recorded any
	if cwd, ok := GetSessionCwd(env, taskID); ok {
		recorded = cwd
	}
	if recorded == current["cwd"] {
		ClearSessionCwd(env, taskID)
	}
}
