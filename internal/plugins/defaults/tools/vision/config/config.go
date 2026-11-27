package config

import (
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

const (
	Key              = toolsconfig.Key + ".vision"
	EmbedTargetBytes = Key + ".embed_target_bytes"
	MaxCallsPerImage = Key + ".max_calls_per_image"
	Video            = Key + ".video"
	Debug            = Key + ".debug"
)

// Helper tasks the tools run on
const (
	Task      = auxiliaryconfig.Vision
	VideoTask = "video"
)

// Settings the vision task keeps below its key
const (
	DownloadTimeout = "download_timeout"
	Temperature     = auxiliaryconfig.Temperature
	MaxConcurrency  = "max_concurrency"
)
