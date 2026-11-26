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
