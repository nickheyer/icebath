package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "Microsoft Edge", About: "Free voices from Microsoft Edge, no API key needed",
		Order: 10, Settings: []config.Setting{
			{Key: Voice, Title: "Voice", About: "The Edge voice that speaks.", Help: "A name such as en-US-AriaNeural or en-GB-RyanNeural.", Default: DefaultVoice},
			{Key: Speed, Title: "Speed", About: "The speed the Edge voice speaks at.", Help: "1 is normal speed.",
				Default: 0.0, Zero: speechconfig.SameAsVoice},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to Edge takes.", Help: "Longer text is split into several requests.",
				Default: 0, Zero: "5,000 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
		}}})
}
