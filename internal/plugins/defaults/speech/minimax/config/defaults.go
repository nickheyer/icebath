package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "MiniMax speech", About: "MiniMax voices of its global or China service; needs " +
		GlobalKeyEnv + " or " + ChinaKeyEnv, Settings: []config.Setting{
		{Key: BaseURL, Title: "MiniMax endpoint", About: "Custom endpoint for MiniMax speech; empty uses the endpoint of the region", Default: ""},
		{Key: Region, Title: "MiniMax region", About: "Which MiniMax service region to use; empty takes global, or China when only a China key is set", Default: "",
			Options: config.Options(
				config.Option{Value: "", Title: "Automatic", About: "Global, or China when only a China key is set"},
				config.Option{Value: "global", Title: "Global", About: "International service"},
				config.Option{Value: "cn", Title: "China", About: "Mainland China service"},
			)},
		{Key: GroupID, Title: "MiniMax group ID", About: "Account group ID sent to MiniMax; empty uses " + GroupEnv + ", else none", Default: ""},
		{Key: Model, Title: "MiniMax model", About: "Speech model used with MiniMax", Default: DefaultModel},
		{Key: VoiceID, Title: "MiniMax voice", About: "Voice used with MiniMax, by its ID", Default: DefaultVoice},
		{Key: Emotion, Title: "MiniMax emotion", About: "Emotion MiniMax puts in its speech, such as neutral or happy", Default: "neutral"},
		{Key: Speed, Title: "MiniMax speed", About: "How fast MiniMax speaks, 1 being normal; 0 uses Speech speed", Default: 0.0},
		{Key: Volume, Title: "MiniMax volume", About: "How loud MiniMax speaks", Default: 1.0},
		{Key: Pitch, Title: "MiniMax pitch", About: "Raise or lower the MiniMax voice pitch", Default: 0},
		{Key: SampleRate, Title: "MiniMax sample rate", About: "Audio sample rate for MiniMax speech, in Hz", Default: 32000},
		{Key: Bitrate, Title: "MiniMax bitrate", About: "Audio quality of MiniMax speech, in bits per second", Default: 128000},
		{Key: MaxTextLength, Title: "MiniMax text limit", About: "Most characters one MiniMax request takes, longer text split into several; 0 uses 10000", Default: 0},
	}}})
}
