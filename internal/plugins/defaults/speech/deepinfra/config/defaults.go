package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "DeepInfra", About: "Open voice models hosted by DeepInfra, with an API key", Order: 90,
			Settings: []config.Setting{
				{Key: Voice, Title: "Voice", About: "The DeepInfra voice that speaks.", Default: DefaultVoice},
				{Key: TTSModel, Title: "Model", About: "The DeepInfra model that speaks.", Default: "", Zero: "DeepInfra's first model"},
				{Key: Speed, Title: "Speed", About: "The speed the DeepInfra voice speaks at.", Help: "1 is normal speed. The general speed is Speed under Text to speech.",
					Default: 0.0, Zero: "The general speed", Least: config.Num(0.25), Most: config.Num(4)},
				{Key: TTSBaseURL, Title: "Base URL", About: "The address DeepInfra speech requests go to.", Help: "The variable " + BaseURLEnv +
					" wins over DeepInfra's own address when it is set.", Default: "", Zero: "DeepInfra's address", Advanced: true},
				{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to DeepInfra takes.",
					Help: "Longer text is split into several requests.", Default: 0, Zero: "4,000 characters", Counts: "characters", Least: config.Num(1),
					Advanced: true},
			}},
		{Key: STT, Parent: speechconfig.STT, Title: "DeepInfra", About: "Open speech to text models hosted by DeepInfra", Order: 70,
			Settings: []config.Setting{
				{Key: STTModel, Title: "Model", About: "The DeepInfra model that turns speech into text.", Default: "", Zero: "DeepInfra's first model"},
				{Key: STTBaseURL, Title: "Base URL", About: "The address DeepInfra speech to text requests go to.", Help: "The variable " + BaseURLEnv +
					" wins over DeepInfra's own address when it is set.", Default: "", Zero: "DeepInfra's address", Advanced: true},
			}},
	})
}
