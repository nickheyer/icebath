package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "Edge speech", About: "Microsoft Edge's free read aloud voices, no key needed",
		Settings: []config.Setting{
			{Key: Voice, Title: "Edge voice", About: "Edge voice name, such as en-US-AriaNeural or en-GB-RyanNeural", Default: DefaultVoice},
			{Key: Speed, Title: "Edge speed", About: "How fast the Edge voice speaks, 1 being normal; 0 uses Speech speed", Default: 0.0},
			{Key: MaxTextLength, Title: "Edge text limit", About: "Most characters one Edge request takes, longer text split into several; 0 uses 5000", Default: 0},
		}}})
}
