package models

import (
	"github.com/nickheyer/icebath/internal/models"
	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/minimax/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: minimaxconfig.GlobalName})
	registry.Register(models.Listed{Provider: minimaxconfig.CNName})
	registry.Register(models.Listed{Provider: minimaxconfig.OAuthName})
}
