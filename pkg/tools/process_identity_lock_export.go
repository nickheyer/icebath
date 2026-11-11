package tools

// Pids claimed by valid Desktop SSH backend lock records under baseDir or both home roots when empty
func LockOwnedServePIDs(baseDir string) map[int]bool { return lockOwnedServePids(baseDir) }
