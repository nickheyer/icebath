package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "MiniMax", About: "Voices with emotion and pitch control, with an API key",
		Order: 70, Settings: []config.Setting{
			{Key: VoiceID, Title: "Voice", About: "The MiniMax voice that speaks, by its ID.", Default: DefaultVoice},
			{Key: Model, Title: "Model", About: "The MiniMax model that speaks.", Default: DefaultModel},
			{Key: Emotion, Title: "Emotion", About: "The emotion MiniMax puts in its speech.", Help: "Such as neutral or happy.", Default: "neutral", Zero: "Neutral"},
			{Key: Speed, Title: "Speed", About: "The speed the MiniMax voice speaks at.", Help: "1 is normal speed. The general speed is Speed under Text to speech.",
				Default: 0.0, Zero: "The general speed"},
			{Key: Volume, Title: "Volume", About: "The volume MiniMax speaks at.", Help: "1 is normal volume.", Default: 1.0},
			{Key: Pitch, Title: "Pitch", About: "The steps the MiniMax voice is raised or lowered by.", Default: 0, Zero: "Unchanged",
				Least: config.Num(-12), Most: config.Num(12)},
			{Key: Region, Title: "Region", About: "Which MiniMax service your API key belongs to.", Default: "",
				Options: config.Options(
					config.Option{Value: "", Title: "Automatic", About: "Global, or China when only a China API key is set"},
					config.Option{Value: "global", Title: "Global", About: "The international service"},
					config.Option{Value: "cn", Title: "China", About: "The mainland China service"},
				), Advanced: true},
			{Key: GroupID, Title: "Group ID", About: "The account group ID sent to MiniMax.", Default: "", Zero: "From " + GroupEnv, Advanced: true},
			{Key: BaseURL, Title: "Base URL", About: "The address MiniMax speech requests go to.", Default: "", Zero: "The region's address", Advanced: true},
			{Key: SampleRate, Title: "Sample rate", About: "The sample rate of MiniMax speech.", Default: 32000, Counts: "Hz", Advanced: true},
			{Key: Bitrate, Title: "Bitrate", About: "The audio quality of MiniMax speech.", Default: 128000, Counts: "bps", Advanced: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to MiniMax takes.", Help: "Longer text is split into several requests.",
				Default: 0, Zero: "10,000 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
		}}})
}
