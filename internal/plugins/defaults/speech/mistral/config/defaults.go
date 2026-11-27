package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "Mistral", About: "Voices in many languages, with a Mistral API key", Order: 80, Settings: []config.Setting{
			{Key: VoiceID, Title: "Voice", About: "The Mistral voice that speaks, by its ID.", Default: DefaultVoice},
			{Key: TTSModel, Title: "Model", About: "The Mistral model that speaks.", Default: DefaultModel},
			{Key: TTSBaseURL, Title: "Base URL", About: "The address Mistral speech requests go to.", Default: "", Zero: "Mistral's address", Advanced: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to Mistral takes.", Help: "Longer text is split into several requests.",
				Default: 0, Zero: "4,000 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
		}},
		{Key: STT, Parent: speechconfig.STT, Title: "Mistral", About: "Speech to text from Mistral, with a Mistral API key", Order: 40, Settings: []config.Setting{
			{Key: STTModel, Title: "Model", About: "The Mistral model that turns speech into text.", Default: DefaultSTTModel},
			{Key: STTLanguage, Title: "Language", About: "The language of what you say.", Help: "A code such as en or de.", Default: "", Zero: "Your language"},
		}},
	})
}
