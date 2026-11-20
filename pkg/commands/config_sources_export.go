package commands

import "github.com/nickheyer/icebath/pkg/config"

// Readers and writers a config set reaches outside the config package for a home
func ConfigCommandSources(h string) config.ConfigCommandSources { return configCommandSources(h) }
