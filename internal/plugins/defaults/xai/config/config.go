package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	XAIName         = "xai"
	XAIKey          = pluginsconfig.Key + "." + XAIName
	XAIBaseURL      = XAIKey + ".base_url"
	XAIAPIKey       = XAIKey + ".api_key"
	XAIDefaultModel = XAIKey + ".default_model"
)

const (
	OAuthName         = "xai-oauth"
	OAuthKey          = pluginsconfig.Key + "." + OAuthName
	OAuthBaseURL      = OAuthKey + ".base_url"
	OAuthDefaultModel = OAuthKey + ".default_model"
)

const (
	APIKeyEnv  = "XAI_API_KEY"
	BaseURLEnv = "XAI_BASE_URL"
)
