package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.STT, Title: "Groq", About: "Fast speech to text with a free tier and a Groq API key",
		Order: 20, Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The Groq model that turns speech into text.", Default: DefaultModel, Env: []string{ModelEnv}},
			{Key: Language, Title: "Language", About: "The language of what you say.", Help: "A code such as en or de.", Default: "", Zero: "Your language"},
			{Key: BaseURL, Title: "Base URL", About: "The address Groq requests go to.", Default: "", Zero: "Groq's address", Env: []string{BaseURLEnv},
				Advanced: true},
		}}})
}
