package models

import (
	"github.com/nickheyer/icebath/internal/models"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: azurefoundryconfig.Name})
}
