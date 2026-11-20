package tools

// Resolves a model supplied path against the base directory of the named task
func ResolvePathForTaskID(env *Env, taskID, filePath string) (string, error) {
	return resolvePathForTaskID(env, taskID, filePath)
}

// Backend name of a task whose file paths belong to a container, empty otherwise
func ContainerBackendForTask(env *Env, taskID string) string {
	return containerBackendForTask(env, taskID)
}
