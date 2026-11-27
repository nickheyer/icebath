package provider

import (
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               lmstudioconfig.Name,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning"},
		DisplayName:        lmstudioconfig.Title,
		Description:        "Models that run on this computer in LM Studio",
		BaseURL:            lmstudioconfig.BaseURL,
		BaseURLEnv:         lmstudioconfig.BaseURLEnv,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{lmstudioconfig.APIKeyEnv},
		Aliases:            []string{"lm-studio", "lm_studio"},
		DefaultModel:       lmstudioconfig.DefaultModel,
		Local:              true,
		VisionToolMessages: true,
		ProbeAlways:        true,
	}}
}
