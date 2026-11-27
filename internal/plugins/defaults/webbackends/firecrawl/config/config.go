package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key    = webconfig.Key + ".firecrawl"
	APIKey = Key + ".api_key"
	APIURL = Key + ".api_url"

	// Environment variables of the key and the address, named in the texts that say what to set
	APIKeyEnv = "FIRECRAWL_API_KEY"
	APIURLEnv = "FIRECRAWL_API_URL"
)
