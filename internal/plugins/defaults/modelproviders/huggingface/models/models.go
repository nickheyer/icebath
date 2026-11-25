package models

import (
	"github.com/nickheyer/icebath/internal/models"
	huggingfaceconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/config"
)

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: huggingfaceconfig.Name})
}
