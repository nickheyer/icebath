package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.STT, Title: "Groq recognition", About: "Whisper on Groq, fast and with a free tier; needs " + KeyEnv,
		Settings: []config.Setting{
			{Key: BaseURL, Title: "Groq endpoint", About: "Custom endpoint for Groq speech recognition; empty for Groq's own", Default: "", Env: []string{BaseURLEnv}},
			{Key: Language, Title: "Groq language", About: "Language hint for Groq transcripts; empty detects it", Default: ""},
			{Key: Model, Title: "Groq model", About: "Speech recognition model used with Groq", Default: DefaultModel, Env: []string{ModelEnv}},
		}}})
}
