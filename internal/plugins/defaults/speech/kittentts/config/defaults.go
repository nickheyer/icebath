package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "KittenTTS speech", About: "A small voice model of about 25 MB that runs on this machine",
		Settings: []config.Setting{
			{Key: CleanText, Title: "Clean text", About: "Tidy text before KittenTTS speaks it", Default: true},
			{Key: Model, Title: "KittenTTS model", About: "On-device speech model used with KittenTTS", Default: DefaultModel},
			{Key: Speed, Title: "KittenTTS speed", About: "How fast KittenTTS speaks, 1 being normal; 0 uses Speech speed", Default: 0.0},
			{Key: Voice, Title: "KittenTTS voice", About: "Voice used with KittenTTS", Default: DefaultVoice},
			{Key: MaxTextLength, Title: "KittenTTS text limit", About: "Most characters one KittenTTS request takes, longer text split into several; 0 uses 2000",
				Default: 0},
		}}})
}
