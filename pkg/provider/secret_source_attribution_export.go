package provider

// Attributes an env name to an external secret source and returns the restore of its prior attribution
func AttributeSecretSourceForTests(name, source string) func() {
	secretSourceCacheMu.Lock()
	prior, had := secretSourceOfVar[name]
	secretSourceOfVar[name] = source
	secretSourceCacheMu.Unlock()
	return func() {
		secretSourceCacheMu.Lock()
		defer secretSourceCacheMu.Unlock()
		if had {
			secretSourceOfVar[name] = prior
		} else {
			delete(secretSourceOfVar, name)
		}
	}
}
