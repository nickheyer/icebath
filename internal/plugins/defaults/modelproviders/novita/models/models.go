package models

import (
	"github.com/nickheyer/icebath/internal/models"
	novitaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: novitaconfig.Name})
}
