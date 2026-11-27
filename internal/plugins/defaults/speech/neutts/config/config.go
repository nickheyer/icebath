package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "neutts"
	Key           = speechconfig.TTS + "." + Name
	Device        = Key + ".device"
	Model         = Key + "." + speechconfig.ModelKey
	RefAudio      = Key + ".ref_audio"
	RefText       = Key + ".ref_text"
	MaxTextLength = Key + "." + speechconfig.MaxTextLength
	DefaultModel  = "neuphonic/neutts-air-q4-gguf"
	DefaultDevice = "cpu"
	// Python set the engine runs in
	Extra = "neutts"
	// Characters one request takes when its setting is unset
	DefaultLimit = 2000
)
