package models

import (
	"github.com/nickheyer/icebath/internal/models"
	qwenoauthconfig "github.com/nickheyer/icebath/internal/plugins/defaults/qwenoauth/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: qwenoauthconfig.Name})
}
