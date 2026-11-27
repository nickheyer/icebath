package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               bedrockconfig.Name,
		DisplayName:        bedrockconfig.Title,
		BaseURL:            bedrockconfig.BaseURL,
		BaseURLEnv:         bedrockconfig.BaseURLEnv,
		Mode:               provider.ModeBedrockConverse,
		Auth:               provider.AuthAWSSigV4,
		AuthType:           provider.AuthTypeAWSSDK,
		Aliases:            []string{"aws", "aws-bedrock", "amazon-bedrock", "amazon"},
		DefaultModel:       bedrockconfig.DefaultModel,
		HealthCheck:        true,
		VisionToolMessages: true,
		StaticModels:       []string{"us.anthropic.claude-sonnet-5", "us.anthropic.claude-opus-5-5", "us.anthropic.claude-sonnet-4-6", "us.anthropic.claude-opus-4-6-v1", "us.anthropic.claude-haiku-4-5-20251001-v1:0", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", "openai.gpt-6-astra", "openai.gpt-6.1-sol", "openai.gpt-6-sol", "openai.gpt-6-luna", "openai.gpt-5.5", "openai.gpt-5.6-sol", "openai.gpt-5.6-terra", "openai.gpt-5.6-luna", "us.amazon.nova-pro-v1:0", "us.amazon.nova-lite-v1:0", "us.amazon.nova-micro-v1:0", "deepseek.v3.2", "us.meta.llama4-maverick-17b-instruct-v1:0", "us.meta.llama4-scout-17b-instruct-v1:0"},
		ProbeAlways:        true,
		Prices:             prices,
		DottedModels:       true,
		AutoLast:           true,
		Route:              route,
		RequestHeaders:     guardrailHeaders,
		Shape:              guardrail,
		Settings: []config.Setting{
			{Key: bedrockconfig.Region, Title: "Bedrock region", About: "AWS region requests are signed for and sent to; AWS_REGION, AWS_DEFAULT_REGION " +
				"or the AWS profile's when empty", Default: ""},
			{Key: bedrockconfig.Profile, Title: "AWS profile", About: "Profile of ~/.aws/config the provider signs in with; AWS_PROFILE or default when empty",
				Default: ""},
			{Key: bedrockconfig.Guardrail, Title: "Guardrail ID", About: "ID of the Bedrock guardrail applied to the replies; the version must be set " +
				"too", Default: ""},
			{Key: bedrockconfig.GuardrailVersion, Title: "Guardrail version", About: "Version of the Bedrock guardrail applied to the replies",
				Default: ""},
			{Key: bedrockconfig.GuardrailStreaming, Title: "Guardrail streaming", About: "Whether the guardrail checks streamed replies before they are " +
				"shown", Default: "async", Options: config.Options(
				config.Option{Value: "async", Title: "Async", About: "Check without delaying replies"},
				config.Option{Value: "sync", Title: "Sync", About: "Check before text is shown"})},
			{Key: bedrockconfig.GuardrailTrace, Title: "Guardrail trace", About: "Whether Bedrock reports why the guardrail acted", Default: "disabled",
				Options: config.Options(config.Option{Value: "disabled", Title: "Off", About: "No guardrail trace"},
					config.Option{Value: "enabled", Title: "On", About: "Include guardrail trace"},
					config.Option{Value: "enabled_full", Title: "Full", About: "Include full guardrail trace"})},
		},
	}}
}
