package config

const (
	GlobalName         = "minimax"
	GlobalTitle        = "MiniMax"
	GlobalBaseURL      = "https://api.minimax.io/anthropic"
	GlobalDefaultModel = "MiniMax-M3"
)

const (
	CNName         = "minimax-cn"
	CNTitle        = "MiniMax (China)"
	CNBaseURL      = "https://api.minimaxi.com/anthropic"
	CNDefaultModel = "MiniMax-M3"
)

const (
	OAuthName         = "minimax-oauth"
	OAuthTitle        = "MiniMax (OAuth)"
	OAuthBaseURL      = "https://api.minimax.io/anthropic"
	OAuthDefaultModel = "MiniMax-M2.7"
)

const (
	GlobalAPIKeyEnv  = "MINIMAX_API_KEY"
	GlobalBaseURLEnv = "MINIMAX_BASE_URL"
	CNAPIKeyEnv      = "MINIMAX_CN_API_KEY"
	CNBaseURLEnv     = "MINIMAX_CN_BASE_URL"
)

const (
	GlobalModelsURL = "https://api.minimax.io/anthropic/models"
	CNModelsURL     = "https://api.minimaxi.com/anthropic/models"
	OAuthModelsURL  = "https://api.minimax.io/anthropic/models"
)

// Portals and client of the MiniMax OAuth sign-in, global and China
const (
	OAuthPortal    = "https://api.minimax.io"
	OAuthCNPortal  = "https://api.minimaxi.com"
	OAuthCNBaseURL = "https://api.minimaxi.com/anthropic"
	OAuthClientID  = "78257093-7e40-4613-99e0-527b14b39113"
	OAuthScope     = "group_id profile model.completion"
)

// Fields of a MiniMax OAuth account, below its name
const (
	AccessToken  = "access_token"
	RefreshToken = "refresh_token"
	// When the access token expires, in Unix milliseconds
	Expiry = "expiry"
	// Portal the sign-in was made at and renews at, with its client, region and the address of its inference API
	Portal   = "portal_base_url"
	Client   = "client_id"
	Region   = "region"
	Endpoint = "base_url"
)
