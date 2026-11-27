package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	Key         = generationconfig.ImageKey + ".openrouter"
	Model       = Key + ".model"
	AspectRatio = Key + ".aspect_ratio"
	// Endpoint image requests go to: images, chat, or picked per model when empty
	Surface           = Key + ".surface"
	Timeout           = Key + ".timeout"
	Resolution        = Key + ".resolution"
	Quality           = Key + ".quality"
	Background        = Key + ".background"
	OutputFormat      = Key + ".output_format"
	OutputCompression = Key + ".output_compression"
	Seed              = Key + ".seed"
	Count             = Key + ".count"
)

const Name = "openrouter"

// Value of a number setting that leaves the model to its own default
const Unset = -1
