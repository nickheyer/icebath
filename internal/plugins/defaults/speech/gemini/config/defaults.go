package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "Google Gemini", About: "Expressive voices from Gemini, with a Gemini API key",
		Order: 50, Settings: []config.Setting{
			{Key: Voice, Title: "Voice", About: "The Gemini voice that speaks.", Help: "Such as Kore or Puck.", Default: DefaultVoice},
			{Key: Model, Title: "Model", About: "The Gemini model that speaks.", Default: DefaultModel},
			{Key: AudioTags, Title: "Tone tags", About: "Add tone cues to the text before Gemini speaks it.",
				Help: "A helper model adds the cues. Only Gemini 3.1 speech models understand tone cues.", Default: false},
			{Key: PersonaFile, Title: "Persona file", About: "The text file that describes how the Gemini voice should sound.",
				Help: "A path inside the profile folder, or a full path. Write {transcript} in the file where the words go.", Default: "", Advanced: true},
			{Key: BaseURL, Title: "Base URL", About: "The address Gemini speech requests go to.",
				Help: "The variable " + BaseURLEnv + " wins over Google's address when it is set.", Default: "", Zero: "Google's address", Advanced: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to Gemini takes, the persona included.",
				Help: "Longer text is split into several requests.", Default: 0, Zero: "32,000 characters", Counts: "characters", Least: config.Num(1),
				Advanced: true},
		}}})
}
