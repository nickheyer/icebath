package models

import (
	"github.com/nickheyer/icebath/internal/models"
	opencodezenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/opencodezen/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: opencodezenconfig.ZenName})
	registry.Register(models.Listed{Provider: opencodezenconfig.GoName})
}
