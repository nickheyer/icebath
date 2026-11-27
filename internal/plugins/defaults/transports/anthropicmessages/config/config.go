package config

import transportconfig "github.com/nickheyer/icebath/internal/transport/config"

const (
	Key = transportconfig.Key + ".anthropic_messages"
	// Output cap of a request that asks none, 0 for the model's own ceiling
	MaxTokens = Key + ".max_tokens"
	// Lets the Anthropic API answer a refused request with its fallback model
	Fallbacks = Key + ".fallbacks"
	// Address Claude on Bedrock is reached at in place of the region's runtime endpoint, such as a VPC endpoint
	BedrockBaseURL = Key + ".bedrock_base_url"
)

const BedrockBaseURLEnv = "ANTHROPIC_BEDROCK_BASE_URL"
