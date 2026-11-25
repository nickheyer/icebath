package models

import (
	"github.com/nickheyer/icebath/internal/models"
	upstageconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: upstageconfig.Name})
}
