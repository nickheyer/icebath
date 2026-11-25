package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	ZenName         = "opencode-zen"
	ZenKey          = pluginsconfig.Key + "." + ZenName
	ZenBaseURL      = ZenKey + ".base_url"
	ZenAPIKey       = ZenKey + ".api_key"
	ZenDefaultModel = ZenKey + ".default_model"
)

const (
	GoName         = "opencode-go"
	GoKey          = pluginsconfig.Key + "." + GoName
	GoBaseURL      = GoKey + ".base_url"
	GoAPIKey       = GoKey + ".api_key"
	GoDefaultModel = GoKey + ".default_model"
)

const (
	ZenAPIKeyEnv  = "OPENCODE_ZEN_API_KEY"
	ZenBaseURLEnv = "OPENCODE_ZEN_BASE_URL"
	GoAPIKeyEnv   = "OPENCODE_GO_API_KEY"
	GoBaseURLEnv  = "OPENCODE_GO_BASE_URL"
)
