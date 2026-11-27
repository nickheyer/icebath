package config

const (
	Name         = "openrouter"
	Title        = "OpenRouter"
	BaseURL      = "https://openrouter.ai/api/v1"
	DefaultModel = ""
)

const (
	APIKeyEnv  = "OPENROUTER_API_KEY"
	BaseURLEnv = "OPENROUTER_BASE_URL"
	ModelsURL  = "https://openrouter.ai/api/v1/models"
)

// Names of the settings of an OpenRouter provider, below its name in the profile
const (
	// Upstream providers requests may go to: only, ignore, order, sort, require_parameters and data_collection, and the same by model under models
	Routing = "routing"
	// Lowest coding score the pareto router picks a model with
	MinCodingScore = "min_coding_score"
	// Whether OpenRouter may answer from its cache of identical requests, and for how many seconds it keeps them
	ResponseCache    = "response_cache"
	ResponseCacheTTL = "response_cache_ttl"
)
