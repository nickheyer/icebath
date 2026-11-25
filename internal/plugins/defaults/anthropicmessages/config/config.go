package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Key       = pluginsconfig.Key + ".anthropicmessages"
	MaxTokens = Key + ".max_tokens"
	Fallbacks = Key + ".fallbacks"
)
