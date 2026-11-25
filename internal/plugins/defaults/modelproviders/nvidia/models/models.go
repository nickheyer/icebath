package models

import (
	"github.com/nickheyer/icebath/internal/models"
	nvidiaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: nvidiaconfig.Name})
}
