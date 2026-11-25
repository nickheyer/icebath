package models

import (
	"github.com/nickheyer/icebath/internal/models"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: openaicodexconfig.Name})
}
