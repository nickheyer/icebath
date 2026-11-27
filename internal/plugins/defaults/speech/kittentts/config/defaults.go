package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "KittenTTS", About: "A small voice model that runs on this computer",
		Order: 100, Settings: []config.Setting{
			{Key: Voice, Title: "Voice", About: "The KittenTTS voice that speaks.", Default: DefaultVoice},
			{Key: Model, Title: "Model", About: "The KittenTTS model that speaks.", Default: DefaultModel},
			{Key: Speed, Title: "Speed", About: "The speed the KittenTTS voice speaks at.",
				Help: "1 is normal speed. The general speed is Speed under Text to speech.", Default: 0.0, Zero: "The general speed"},
			{Key: CleanText, Title: "Tidy text", About: "Tidy the text before KittenTTS speaks it.", Default: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to KittenTTS takes.",
				Help: "Longer text is split into several requests.", Default: 0, Zero: "2,000 characters", Counts: "characters", Least: config.Num(1),
				Advanced: true},
		}}})
}
