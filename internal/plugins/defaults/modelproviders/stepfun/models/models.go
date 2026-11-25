package models

import (
	"github.com/nickheyer/icebath/internal/models"
	stepfunconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: stepfunconfig.Name})
}
