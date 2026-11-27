package config

import browserconfig "github.com/nickheyer/icebath/internal/browser/config"

const (
	Key = browserconfig.Key + ".firecrawl"
	// Seconds a Firecrawl browser session lives, one at the least
	TTL = Key + ".ttl"
)

// Variables of the Firecrawl key and of the address of a self-hosted Firecrawl, which the Firecrawl account's settings read, shared with
// Firecrawl web search
const (
	APIKeyEnv = "FIRECRAWL_API_KEY"
	APIURLEnv = "FIRECRAWL_API_URL"
)
