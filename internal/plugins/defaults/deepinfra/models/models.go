package models

import (
	"github.com/nickheyer/icebath/internal/models"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/deepinfra/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: deepinfraconfig.Name})
}
