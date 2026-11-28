package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	Key     = generationconfig.ImageKey + ".deepinfra"
	Model   = Key + ".model"
	BaseURL = Key + ".base_url"

	VideoKey   = generationconfig.VideoKey + ".deepinfra"
	VideoModel = VideoKey + ".model"
)

const Name = "deepinfra"
