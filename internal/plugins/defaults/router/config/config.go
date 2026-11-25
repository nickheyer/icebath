package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "router"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv       = "RAMP_ROUTER_API_KEY"
	RouterAPIKeyEnv = "ROUTER_API_KEY"
	BaseURLEnv      = "RAMP_ROUTER_BASE_URL"
)
