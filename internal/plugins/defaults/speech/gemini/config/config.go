package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "gemini"
	Key           = speechconfig.TTS + "." + Name
	AudioTags     = Key + ".audio_tags"
	BaseURL       = Key + "." + speechconfig.BaseURL
	Model         = Key + "." + speechconfig.ModelKey
	PersonaFile   = Key + ".persona_prompt_file"
	Voice         = Key + ".voice"
	MaxTextLength = Key + "." + speechconfig.MaxTextLength
	DefaultModel  = "gemini-2.5-flash-preview-tts"
	DefaultVoice  = "Kore"
	KeyEnv        = "GEMINI_API_KEY"
	GoogleKeyEnv  = "GOOGLE_API_KEY"
	BaseURLEnv    = "GEMINI_BASE_URL"
	Link          = "https://aistudio.google.com/app/apikey"
	// Characters one request takes when its setting is unset
	DefaultLimit = 32000
)
