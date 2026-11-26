package provider

import (
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
		ModelsDevID:        bedrockconfig.Name,
	}}
}
