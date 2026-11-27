package config

const (
	XAIName         = "xai"
	XAITitle        = "xAI"
	XAIBaseURL      = "https://api.x.ai/v1"
	XAIDefaultModel = ""
)

const (
	OAuthName         = "xai-oauth"
	OAuthTitle        = "xAI Grok OAuth (SuperGrok / Premium+)"
	OAuthBaseURL      = "https://api.x.ai/v1"
	OAuthDefaultModel = ""
)

const (
	APIKeyEnv  = "XAI_API_KEY"
	BaseURLEnv = "XAI_BASE_URL"
)

// Address of direct calls to xAI that wins over the provider's base URL
const IcebathBaseURLEnv = "ICEBATH_XAI_BASE_URL"

// Fields of a Grok sign-in's account, below its name
const (
	AccessToken  = "access_token"
	RefreshToken = "refresh_token"
	IDToken      = "id_token"
	// Token endpoint discovery gave, renewals post to
	TokenEndpoint = "token_endpoint"
	LastRefresh   = "last_refresh"
)

// Seconds a renewal of a Grok sign-in may take, and the variable that sets them
const (
	RefreshTimeout    = "refresh_timeout"
	RefreshTimeoutEnv = "ICEBATH_XAI_REFRESH_TIMEOUT_SECONDS"
)
