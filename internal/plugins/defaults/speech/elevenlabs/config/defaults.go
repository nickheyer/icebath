package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "ElevenLabs", About: "Lifelike voices from ElevenLabs, with an ElevenLabs API key", Order: 30,
			Settings: []config.Setting{
				{Key: VoiceID, Title: "Voice", About: "The ElevenLabs voice that speaks, by its ID.", Default: DefaultVoice},
				{Key: ModelID, Title: "Model", About: "The ElevenLabs model that speaks.", Default: DefaultModel},
				{Key: StreamingModelID, Title: "Live model", About: "The model that speaks while a reply is still being written.", Default: "",
					Zero: "Same as Model", Advanced: true},
				{Key: TTSBaseURL, Title: "Base URL", About: "The address ElevenLabs speech requests go to.", Default: "", Zero: "ElevenLabs' address",
					Advanced: true},
				{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to ElevenLabs takes.",
					Help: "Longer text is split into several requests.", Default: 0, Zero: "The model's limit", Counts: "characters", Least: config.Num(1),
					Advanced: true},
			}},
		{Key: STT, Parent: speechconfig.STT, Title: "ElevenLabs", About: "Speech to text that tells speakers apart, with an API key", Order: 60,
			Settings: []config.Setting{
				{Key: STTModelID, Title: "Model", About: "The ElevenLabs model that turns speech into text.", Default: DefaultSTTModel, Env: []string{STTModelEnv}},
				{Key: LanguageCode, Title: "Language", About: "The language code of what you say.", Help: "Such as en or de.", Default: "",
					Zero: "Your language"},
				{Key: Diarize, Title: "Label speakers", About: "Mark who is speaking in transcripts.", Default: false},
				{Key: TagAudioEvents, Title: "Tag sounds", About: "Mark sounds that are not speech, such as laughter, in transcripts.", Default: false},
				{Key: STTBaseURL, Title: "Base URL", About: "The address ElevenLabs speech to text requests go to.", Default: "", Zero: "ElevenLabs' address",
					Env: []string{STTBaseURLEnv}, Advanced: true},
			}},
	})
}
