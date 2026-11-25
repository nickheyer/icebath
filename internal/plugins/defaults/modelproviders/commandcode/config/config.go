package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	ChatName         = "commandcode"
	ChatKey          = pluginsconfig.Key + "." + ChatName
	ChatBaseURL      = ChatKey + ".base_url"
	ChatAPIKey       = ChatKey + ".api_key"
	ChatDefaultModel = ChatKey + ".default_model"
)

const (
	AnthropicName         = "commandcode-anthropic"
	AnthropicKey          = pluginsconfig.Key + "." + AnthropicName
	AnthropicBaseURL      = AnthropicKey + ".base_url"
	AnthropicAPIKey       = AnthropicKey + ".api_key"
	AnthropicDefaultModel = AnthropicKey + ".default_model"
)

const (
	APIKeyEnv           = "COMMANDCODE_API_KEY"
	ChatBaseURLEnv      = "COMMANDCODE_BASE_URL"
	AnthropicBaseURLEnv = "COMMANDCODE_ANTHROPIC_BASE_URL"
)

const (
	Endpoint  = "https://api.commandcode.ai/provider/v1"
	ModelsURL = Endpoint + "/models"
)
