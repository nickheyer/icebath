package models

import (
	"github.com/nickheyer/icebath/internal/models"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: githubcopilotconfig.CopilotName})
}
