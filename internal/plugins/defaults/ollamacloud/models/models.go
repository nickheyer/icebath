package models

import (
	"github.com/nickheyer/icebath/internal/models"
	ollamacloudconfig "github.com/nickheyer/icebath/internal/plugins/defaults/ollamacloud/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: ollamacloudconfig.Name})
}
