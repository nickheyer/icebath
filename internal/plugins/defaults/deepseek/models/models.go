package models

import (
	"github.com/nickheyer/icebath/internal/models"
	deepseekconfig "github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: deepseekconfig.Name})
}
