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
		Description:        "Claude and other models on AWS, with AWS credentials or a Bedrock key",
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
			{Key: bedrockconfig.Region, Title: "AWS region", About: "Which AWS region requests are signed for and sent to.",
				Help: "Pick the region where your account has the models turned on.", Default: "", Options: regions},
			{Key: bedrockconfig.Profile, Title: "AWS profile", About: "Which profile of your AWS settings icebath signs in with.",
				Help: "The profiles are those in ~/.aws/config and ~/.aws/credentials.", Default: "", Options: profiles},
			{Key: bedrockconfig.Guardrail, Title: "Guardrail ID", About: "The ID of the Bedrock guardrail that checks replies.",
				Help: "Set the guardrail version as well.", Default: "", Advanced: true},
			{Key: bedrockconfig.GuardrailVersion, Title: "Guardrail version", About: "The version of the Bedrock guardrail that checks replies.",
				Default: "", Advanced: true},
			{Key: bedrockconfig.GuardrailStreaming, Title: "Guardrail streaming", About: "When the guardrail checks a reply that streams in.",
				Default: "async", Advanced: true, Options: config.Options(
					config.Option{Value: "async", Title: "Without delay", About: "Check while the reply is shown"},
					config.Option{Value: "sync", Title: "Before showing", About: "Check text before it is shown"})},
			{Key: bedrockconfig.GuardrailTrace, Title: "Guardrail trace", About: "Whether Bedrock reports why the guardrail acted.", Default: "disabled",
				Advanced: true, Options: config.Options(config.Option{Value: "disabled", Title: "Off", About: "No report"},
					config.Option{Value: "enabled", Title: "On", About: "Report why the guardrail acted"},
					config.Option{Value: "enabled_full", Title: "Full", About: "Report why the guardrail acted, in full detail"})},
		},
	}}
}
