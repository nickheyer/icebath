package config

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Main config file of the resolved home
func GetConfigPath(h string) string { return home.ConfigPath(h) }

// Secrets file holding the API keys of the resolved home
func GetEnvPath(h string) string { return home.EnvPath(h) }

// Reports whether a package manager manages this install
func IsManaged(ctx context.Context, h string) bool { return ManagedSystem(ctx, h) != "" }

// Frozen config of a safe worker, nil outside one
func workerConfigSnapshot() map[string]any { return WorkerConfigSnapshot() }
