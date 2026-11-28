package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "OpenAI", About: "Natural voices from OpenAI or a server that works the same", Order: 20,
			Settings: []config.Setting{
				{Key: TTSVoice, Title: "Voice", About: "The OpenAI voice that speaks.", Help: "Such as alloy, nova or shimmer.", Default: DefaultVoice},
				{Key: TTSModel, Title: "Model", About: "The OpenAI model that speaks.", Default: DefaultModel},
				{Key: Speed, Title: "Speed", About: "The speed the OpenAI voice speaks at.", Help: "1 is normal speed.",
					Default: 0.0, Zero: speechconfig.SameAsVoice, Least: config.Num(0.25), Most: config.Num(4)},
				{Key: TTSBaseURL, Title: "Base URL", About: "The address of a speech server that works like OpenAI's.", Default: "", Zero: "OpenAI's address",
					Advanced: true},
				{Key: TTSLanguage, Title: "Language", About: "The language hint sent to servers that work like OpenAI's.", Help: "DeepInfra gets this hint too.",
					Default: "", Advanced: true},
				{Key: ConsentAttestation, Title: "Voice consent", About: "The consent statement sent with a cloned voice of your own.",
					Help: "DeepInfra gets this statement too.", Default: "", Advanced: true},
				{Key: PCMSampleRate, Title: "Live sample rate", About: "The sample rate of OpenAI speech that plays as it arrives.",
					Help: "A rate the server reports wins over this.", Default: DefaultRate, Counts: "Hz", Advanced: true},
				{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to OpenAI takes.", Help: "Longer text is split into several requests.",
					Default: 0, Zero: "4,096 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
			}},
		{Key: STT, Parent: speechconfig.STT, Title: "OpenAI", About: "Speech to text from OpenAI or a server that works the same", Order: 30,
			Settings: []config.Setting{
				{Key: STTModel, Title: "Model", About: "The OpenAI model that turns speech into text.", Help: "Such as whisper-1 or gpt-4o-transcribe.",
					Default: DefaultSTTModel, Env: []string{ModelEnv}},
				{Key: STTLanguage, Title: "Language", About: "The language of what you say to OpenAI.", Help: "A code such as en or de.", Default: "",
					Zero: speechconfig.SameAsVoice},
				{Key: StreamingModel, Title: "Live model", About: "The model that writes your words as you speak.", Default: DefaultStreamingModel, Advanced: true},
				{Key: STTBaseURL, Title: "Base URL", About: "The address of a speech to text server that works like OpenAI's.",
					Help: "Your general OpenAI key goes there only when " + BaseURLEnv + " sets the address. A private address needs no key.", Default: "",
					Zero: "OpenAI's address", Env: []string{BaseURLEnv}, Advanced: true},
				{Key: MaxRetries, Title: "Retries", About: "How many times a failed request is tried again.", Help: "Groq and DeepInfra use this too.", Default: 1,
					Counts: "retries", Least: config.Num(0), Advanced: true},
				{Key: Timeout, Title: "Time limit", About: "How long one request may take before it is given up.", Help: "Groq and DeepInfra use this too.",
					Default: 60, Zero: "10 minutes", Counts: "seconds", Advanced: true},
			}},
	})
}
