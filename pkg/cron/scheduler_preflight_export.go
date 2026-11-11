package cron

// True when the profile has a bot of its own, a local only adapter map holds no platform credential
func HasCredentialPlatform(adapters any, owned any) bool { return hasCredentialPlatform(adapters, owned) }
