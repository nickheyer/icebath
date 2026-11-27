package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key        = webconfig.Key + ".parallel"
	APIKey     = Key + ".api_key"
	BaseURL    = Key + ".base_url"
	SearchMode = Key + ".search_mode"
	Headers    = Key + ".custom_headers"

	// Address of the API when none is set
	DefaultBaseURL = "https://api.parallel.ai"
)

// Search modes of the API
const (
	Agentic = "agentic"
	OneShot = "one-shot"
	Fast    = "fast"
)
