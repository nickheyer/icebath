package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	GlobalName         = "minimax"
	GlobalKey          = pluginsconfig.Key + "." + GlobalName
	GlobalBaseURL      = GlobalKey + ".base_url"
	GlobalAPIKey       = GlobalKey + ".api_key"
	GlobalDefaultModel = GlobalKey + ".default_model"
)

const (
	CNName         = "minimax-cn"
	CNKey          = pluginsconfig.Key + "." + CNName
	CNBaseURL      = CNKey + ".base_url"
	CNAPIKey       = CNKey + ".api_key"
	CNDefaultModel = CNKey + ".default_model"
)

const (
	OAuthName         = "minimax-oauth"
	OAuthKey          = pluginsconfig.Key + "." + OAuthName
	OAuthBaseURL      = OAuthKey + ".base_url"
	OAuthDefaultModel = OAuthKey + ".default_model"
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
