package config

// Makes method's update command read command until the returned restore runs
func PatchUpdateCommandForMethod(method, command string) func() {
	saved, had := updateCommandByMethod[method]
	updateCommandByMethod[method] = command
	return func() {
		if had {
			updateCommandByMethod[method] = saved
		} else {
			delete(updateCommandByMethod, method)
		}
	}
}
