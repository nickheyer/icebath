package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	return []provider.Profile{{
		Name:               bedrockconfig.Name,
		DisplayName:        "AWS Bedrock",
		BaseURL:            p.config.GetString(bedrockconfig.BaseURL),
		BaseURLEnv:         bedrockconfig.BaseURLEnv,
		Mode:               provider.ModeBedrockConverse,
		Auth:               provider.AuthAWSSigV4,
		AuthType:           provider.AuthTypeAWSSDK,
		Aliases:            []string{"aws", "aws-bedrock", "amazon-bedrock", "amazon"},
		DefaultModel:       p.config.GetString(bedrockconfig.DefaultModel),
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        bedrockconfig.Name,
	}}
}
