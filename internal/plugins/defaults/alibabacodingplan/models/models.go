package models

import (
	"github.com/nickheyer/icebath/internal/models"
	alibabacodingplanconfig "github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: alibabacodingplanconfig.IntlName})
	registry.Register(models.Listed{Provider: alibabacodingplanconfig.CNName})
}
