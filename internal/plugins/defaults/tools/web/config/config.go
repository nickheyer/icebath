package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key              = toolsconfig.Key + ".web"
	ExtractCharLimit = Key + ".extract_char_limit"
	ExtractTimeout   = Key + ".extract_timeout"
	CacheEnabled     = Key + ".cache_enabled"
	CacheExemptHosts = Key + ".cache_exempt_hosts"
	CacheTTLMinutes  = Key + ".cache_ttl_minutes"
	Debug            = Key + ".debug"
)

// Bounds of the page text limit, the setting and the char_limit argument alike
const (
	MinCharLimit = 2000
	MaxCharLimit = 500000
)

// Bounds of the cache time in minutes, one minute to one day
const (
	MinCacheMinutes = 1
	MaxCacheMinutes = 24 * 60
)
