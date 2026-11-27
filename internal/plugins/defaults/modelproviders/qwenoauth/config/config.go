package config

const (
	Name         = "qwen-oauth"
	Title        = "Qwen OAuth"
	BaseURL      = "https://portal.qwen.ai/v1"
	DefaultModel = ""
)

const (
	APIKeyEnv  = "QWEN_API_KEY"
	BaseURLEnv = "ICEBATH_QWEN_BASE_URL"
)

// Fields of an account, below its name
const (
	AccessToken  = "access_token"
	RefreshToken = "refresh_token"
	ResourceURL  = "resource_url"
	// When the access token expires, in Unix milliseconds
	Expiry = "expiry"
)

// Settings of a provider, below its name
const AdoptExternalLogins = "adopt_external_logins"
