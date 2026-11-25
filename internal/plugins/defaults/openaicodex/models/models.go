package models

import (
	"github.com/nickheyer/icebath/internal/models"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/openaicodex/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: openaicodexconfig.Name})
}
