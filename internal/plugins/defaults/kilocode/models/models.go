package models

import (
	"github.com/nickheyer/icebath/internal/models"
	kilocodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: kilocodeconfig.Name})
}
