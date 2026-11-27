package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name        = "mistral"
	KeyEnv      = "MISTRAL_API_KEY"
	Link        = "https://console.mistral.ai/"
	DefaultBase = "https://api.mistral.ai"
)

// Speech
const (
	TTS           = speechconfig.TTS + "." + Name
	TTSBaseURL    = TTS + "." + speechconfig.BaseURL
	TTSModel      = TTS + "." + speechconfig.ModelKey
	VoiceID       = TTS + ".voice_id"
	MaxTextLength = TTS + "." + speechconfig.MaxTextLength
	DefaultModel  = "voxtral-mini-tts-2603"
	DefaultVoice  = "c69964a6-ab8b-4f8a-9465-ec0925096ec8"
	// Characters one request takes when its setting is unset
	DefaultLimit = 4000
)

// Speech recognition
const (
	STT             = speechconfig.STT + "." + Name
	STTModel        = STT + "." + speechconfig.ModelKey
	STTLanguage     = STT + ".language"
	DefaultSTTModel = "voxtral-mini-latest"
)
