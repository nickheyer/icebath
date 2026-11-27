package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "Gemini speech", About: "Google Gemini's voices, directed by a persona; needs " + KeyEnv,
		Settings: []config.Setting{
			{Key: AudioTags, Title: "Gemini tone tags", About: "Add tone cues to text before Gemini speaks it; Gemini 3.1 speech models only", Default: false},
			{Key: BaseURL, Title: "Gemini endpoint", About: "Custom endpoint for Gemini speech; empty uses " + BaseURLEnv + ", else Google's", Default: ""},
			{Key: Model, Title: "Gemini model", About: "Speech model used with Gemini", Default: DefaultModel},
			{Key: PersonaFile, Title: "Gemini persona file", About: "Path to a text file describing how Gemini should sound, relative to the profile's folder; " +
				"{transcript} in it marks where the words go", Default: ""},
			{Key: Voice, Title: "Gemini voice", About: "Voice used with Gemini, such as Kore or Puck", Default: DefaultVoice},
			{Key: MaxTextLength, Title: "Gemini text limit", About: "Most characters one Gemini request takes, persona included, longer text split into several; " +
				"0 uses 32000", Default: 0},
		}}})
}
