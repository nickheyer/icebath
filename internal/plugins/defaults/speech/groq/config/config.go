package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name         = "groq"
	Key          = speechconfig.STT + "." + Name
	BaseURL      = Key + "." + speechconfig.BaseURL
	Language     = Key + ".language"
	Model        = Key + "." + speechconfig.ModelKey
	DefaultModel = "whisper-large-v3-turbo"
	DefaultBase  = "https://api.groq.com/openai/v1"
	KeyEnv       = "GROQ_API_KEY"
	BaseURLEnv   = "GROQ_BASE_URL"
	ModelEnv     = "STT_GROQ_MODEL"
)
