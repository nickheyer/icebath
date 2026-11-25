package models

import (
	"github.com/nickheyer/icebath/internal/models"
	fireworksconfig "github.com/nickheyer/icebath/internal/plugins/defaults/fireworks/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: fireworksconfig.Name})
}
