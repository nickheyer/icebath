package models

import (
	"github.com/nickheyer/icebath/internal/models"
	gmiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: gmiconfig.Name})
}
