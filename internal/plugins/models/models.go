package models

import "github.com/nickheyer/icebath/internal/models"

func Register(host *models.Models) *models.Registry {
	registry := new(models.Registry)
	host.Register(registry)
	return registry
}
