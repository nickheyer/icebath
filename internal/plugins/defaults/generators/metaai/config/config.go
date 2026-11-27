package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	Key   = generationconfig.ImageKey + ".meta-ai"
	Model = Key + ".model"
)

const (
	Name         = "meta-ai"
	DefaultModel = "muse-image-1.0"
)
