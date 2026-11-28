package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	Key = generationconfig.ImageKey + ".fal"
	// Key of FAL, else the id and secret of one
	APIKey    = Key + ".api_key"
	KeyID     = Key + ".key_id"
	KeySecret = Key + ".key_secret"
	// Host of FAL's queue, fal.run when empty
	RunHost = Key + ".run_host"
	// Picture model FAL draws with
	Model = Key + ".model"

	VideoKey   = generationconfig.VideoKey + ".fal"
	VideoModel = VideoKey + ".model"
)

const (
	Name     = "fal"
	KeyLink  = "https://fal.ai/dashboard/keys"
	QueueRun = "fal.run"
)
