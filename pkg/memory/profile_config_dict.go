package memory

// Parsed config.yaml mapping of a profile dir, nil when absent, unreadable or not a mapping
func LoadProfileConfigDict(profileDir string) map[string]any {
	return loadProfileConfigDict(profileDir)
}
