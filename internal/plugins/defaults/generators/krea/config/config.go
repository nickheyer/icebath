package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	Key        = generationconfig.ImageKey + ".krea"
	APIKey     = Key + ".api_key"
	Model      = Key + ".model"
	Creativity = Key + ".creativity"
	// Whether Krea enlarges each picture: true, false, or empty for the model's default
	Upscale = Key + ".upscale"
)

const (
	Name    = "krea"
	KeyLink = "https://www.krea.ai/settings/api-tokens"
	BaseURL = "https://api.krea.ai"
)
