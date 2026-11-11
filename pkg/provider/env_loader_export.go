package provider

// Routing keys the env loader scrubs when the profile .env leaves them undefined
func ProfileManagedEnvKeys() []string { return profileManagedEnvKeys() }

// Names a dotenv file assigns, empty keys included
func EnvKeysDefinedInDotenv(path string) map[string]bool { return envKeysDefinedInDotenv(path) }
