package models

import (
	"github.com/nickheyer/icebath/internal/models"
	zaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/zai/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: zaiconfig.Name})
}
