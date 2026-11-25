package models

import (
	"github.com/nickheyer/icebath/internal/models"
	arceeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: arceeconfig.Name})
}
