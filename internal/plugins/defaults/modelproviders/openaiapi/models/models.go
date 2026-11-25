package models

import (
	"github.com/nickheyer/icebath/internal/models"
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: openaiapiconfig.Name})
}
