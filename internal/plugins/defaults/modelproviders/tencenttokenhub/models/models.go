package models

import (
	"github.com/nickheyer/icebath/internal/models"
	tencenttokenhubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: tencenttokenhubconfig.TokenHubName})
	registry.Register(models.Listed{Provider: tencenttokenhubconfig.TokenPlanName})
}
