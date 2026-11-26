package config

const (
	ChatName         = "commandcode"
	ChatTitle        = "CommandCode"
	ChatBaseURL      = Endpoint
	ChatDefaultModel = ""
)

const (
	AnthropicName         = "commandcode-anthropic"
	AnthropicTitle        = "CommandCode (Anthropic)"
	AnthropicBaseURL      = Endpoint
	AnthropicDefaultModel = ""
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
