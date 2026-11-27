package config

import browserconfig "github.com/nickheyer/icebath/internal/browser/config"

const (
	Key    = browserconfig.Key + ".browser_use"
	APIKey = Key + ".api_key"
	// Variable of the key, which the code the browser runs also reads
	APIKeyEnv = "BROWSER_USE_API_KEY"
)
