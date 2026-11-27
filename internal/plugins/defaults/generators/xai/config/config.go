package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	ImageKey   = generationconfig.ImageKey + ".xai"
	ImageModel = ImageKey + ".model"
	Resolution = ImageKey + ".resolution"

	VideoKey = generationconfig.VideoKey + ".xai"
)

// Settings of xAI's storage of what it makes, below the image or video key
const (
	StorageEnabled = ".storage.enabled"
	StoragePublic  = ".storage.public_url"
	StorageExpires = ".storage.expires_after"
)

const Name = "xai"
