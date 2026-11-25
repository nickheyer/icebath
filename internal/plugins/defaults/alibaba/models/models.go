package models

import (
	"github.com/nickheyer/icebath/internal/models"
	alibabaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: alibabaconfig.IntlName})
	registry.Register(models.Listed{Provider: alibabaconfig.CNName})
	registry.Register(models.Listed{Provider: alibabaconfig.TokenPlanName})
	registry.Register(models.Listed{Provider: alibabaconfig.TokenPlanCNName})
}
