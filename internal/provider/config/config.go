package config

import (
	"slices"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	Key     = "providers"
	Removed = Key + ".removed"
)

func IsRemoved(cfg *config.Config, name string) bool {
	return cfg != nil && slices.Contains(cfg.GetStringSlice(Removed), name)
}
