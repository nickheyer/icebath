package tools

// RegisterWebProvider adds or replaces a web provider by name in the global layer
func RegisterWebProvider(p WebProvider) {
	if err := webRegistry.Register(p, ""); err != nil {
		panic(err)
	}
}

// Clears every web provider registration, for tests only
func ResetWebRegistryForTests() { webRegistry.ResetForTests() }
