package provider

// Something besides the welcome trial carries inference, asked of the resolver with the free tier hidden
func WelcomeProviderConfigured(env PoolEnv) (bool, error) { return welcomeProviderConfigured(env) }

// The profile's Nous identity is a guest whose claimed welcome trial carries inference here
func FreeInferenceActive(env PoolEnv) bool { return profileFreeInferenceActive(env) }
