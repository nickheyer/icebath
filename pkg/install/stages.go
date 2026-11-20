package install

// Stage names of the install ladder in run order
func StageNames() []string {
	return []string{"prerequisites", "repository", "config", "products", "runtimes", "setup", "gateway", "complete"}
}
