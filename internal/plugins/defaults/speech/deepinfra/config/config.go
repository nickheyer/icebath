package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name       = "deepinfra"
	KeyEnv     = "DEEPINFRA_API_KEY"
	BaseURLEnv = "DEEPINFRA_BASE_URL"
	Link       = "https://deepinfra.com/dash/api_keys"
)

// Speech
const (
	TTS           = speechconfig.TTS + "." + Name
	TTSBaseURL    = TTS + "." + speechconfig.BaseURL
	TTSModel      = TTS + "." + speechconfig.ModelKey
	Speed         = TTS + "." + speechconfig.ServiceSpeed
	Voice         = TTS + ".voice"
	MaxTextLength = TTS + "." + speechconfig.MaxTextLength
	DefaultVoice  = "default"
)

// Speech recognition
const (
	STT        = speechconfig.STT + "." + Name
	STTBaseURL = STT + "." + speechconfig.BaseURL
	STTModel   = STT + "." + speechconfig.ModelKey
)
