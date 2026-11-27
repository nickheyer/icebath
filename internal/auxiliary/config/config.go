package config

const (
	Key              = "auxiliary"
	FreeOnly         = Key + ".free_only"
	OpenRouterModel  = Key + ".openrouter_model"
	TransientRetries = Key + ".transient_retries"
)

// Names of the settings every task keeps below its own key
const (
	Provider          = "provider"
	Model             = "model"
	BaseURL           = "base_url"
	APIKey            = "api_key"
	KeyEnv            = "key_env"
	APIMode           = "api_mode"
	Timeout           = "timeout"
	NoProgressTimeout = "no_progress_timeout"
	ReasoningEffort   = "reasoning_effort"
	ExtraBody         = "extra_body"
	FallbackChain     = "fallback_chain"
)

// Providers a task's provider setting names besides those of the profile
const (
	// Picks one: the main model of the session first
	Auto = "auto"
	// The session's main provider
	Main = "main"
	// The endpoint the task's base URL names
	Custom = "custom"
)

// Task that looks at images, the one every tool that hands the helper an image calls, and the setting of how varied its descriptions are
const (
	Vision      = "vision"
	Temperature = "temperature"
)
