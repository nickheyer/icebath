package models

import (
	"github.com/nickheyer/icebath/internal/models"
	xiaomiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: xiaomiconfig.Name})
}
