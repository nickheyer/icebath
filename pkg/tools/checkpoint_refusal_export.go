package tools

// Explains why host checkpoints are refused for a container backed task read through the given env, empty otherwise
func (m *CheckpointManager) UnsupportedBackendReasonIn(env *Env, taskID string) string {
	if taskID == "" {
		taskID = "default"
	}
	backend := containerBackendForTask(env, taskID)
	if backend == "" {
		return ""
	}
	return "Checkpoints are not taken for terminal.backend=" + backend + ": file paths belong to the container, not this host."
}
