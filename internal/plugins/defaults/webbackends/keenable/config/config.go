package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key    = webconfig.Key + ".keenable"
	APIKey = Key + ".api_key"

	// Environment variable of the key, named in the texts that say what to set
	APIKeyEnv = "KEENABLE_API_KEY"
)
