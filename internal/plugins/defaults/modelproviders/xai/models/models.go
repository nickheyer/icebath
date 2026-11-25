package models

import (
	"github.com/nickheyer/icebath/internal/models"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: xaiconfig.XAIName})
	registry.Register(models.Listed{Provider: xaiconfig.OAuthName})
}
