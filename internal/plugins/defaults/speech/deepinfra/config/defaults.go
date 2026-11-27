package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "DeepInfra speech", About: "Open speech models DeepInfra hosts, from its live catalog; needs " + KeyEnv,
			Settings: []config.Setting{
				{Key: TTSBaseURL, Title: "DeepInfra endpoint", About: "Custom endpoint for DeepInfra speech; empty uses " + BaseURLEnv + ", else DeepInfra's", Default: ""},
				{Key: TTSModel, Title: "DeepInfra model", About: "Speech model used with DeepInfra; empty takes the first of its catalog", Default: ""},
				{Key: Speed, Title: "DeepInfra speed", About: "How fast DeepInfra speaks, from 0.25 to 4, 1 being normal; 0 uses Speech speed", Default: 0.0},
				{Key: Voice, Title: "DeepInfra voice", About: "Voice used with DeepInfra", Default: DefaultVoice},
				{Key: MaxTextLength, Title: "DeepInfra text limit", About: "Most characters one DeepInfra request takes, longer text split into several; 0 uses 4000",
					Default: 0},
			}},
		{Key: STT, Parent: speechconfig.STT, Title: "DeepInfra recognition", About: "Whisper and other recognition models DeepInfra hosts; needs " + KeyEnv,
			Settings: []config.Setting{
				{Key: STTBaseURL, Title: "DeepInfra recognition endpoint", About: "Custom endpoint for DeepInfra speech recognition; empty uses " + BaseURLEnv +
					", else DeepInfra's", Default: ""},
				{Key: STTModel, Title: "DeepInfra model", About: "Speech recognition model used with DeepInfra; empty takes the first of its catalog", Default: ""},
			}},
	})
}
