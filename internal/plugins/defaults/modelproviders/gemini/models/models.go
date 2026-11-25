package models

import (
	"github.com/nickheyer/icebath/internal/models"
	geminiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: geminiconfig.Name})
}
