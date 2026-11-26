package config

import transportconfig "github.com/nickheyer/icebath/internal/transport/config"

const (
	Key       = transportconfig.Key + ".anthropicmessages"
	MaxTokens = Key + ".max_tokens"
	Fallbacks = Key + ".fallbacks"
)
