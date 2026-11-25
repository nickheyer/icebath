package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "openai-codex"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	DefaultModel = Key + ".default_model"
)

const BaseURLEnv = "ICEBATH_CODEX_BASE_URL"
