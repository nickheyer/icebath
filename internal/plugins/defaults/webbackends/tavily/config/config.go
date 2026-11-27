package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key     = webconfig.Key + ".tavily"
	APIKey  = Key + ".api_key"
	BaseURL = Key + ".base_url"

	// Tavily's API, called when no other address is set
	DefaultBaseURL = "https://api.tavily.com"
)
