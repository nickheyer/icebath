package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key     = webconfig.Key + ".perplexity"
	APIKey  = Key + ".api_key"
	BaseURL = Key + ".base_url"

	// Perplexity's API, called when no other address is set
	DefaultBaseURL = "https://api.perplexity.ai"
)
