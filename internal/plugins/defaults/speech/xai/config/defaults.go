package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "xAI", About: "Grok's voices, with an xAI API key or a Grok sign-in", Order: 60, Settings: []config.Setting{
			{Key: VoiceID, Title: "Voice", About: "The xAI voice that speaks.", Help: "Such as eve.", Default: DefaultVoice},
			{Key: TTSLanguage, Title: "Language", About: "The language code xAI speaks in.", Help: "Such as en or de.", Default: DefaultLanguage, Zero: "English"},
			{Key: Speed, Title: "Speed", About: "The speed the xAI voice speaks at.", Help: "1 is normal speed.",
				Default: 0.0, Zero: speechconfig.SameAsVoice, Least: config.Num(0.7), Most: config.Num(1.5)},
			{Key: AutoSpeechTags, Title: "Speech tags", About: "Add expressive speech tags to the text before xAI speaks it.", Default: false},
			{Key: TextNormalization, Title: "Spell out numbers", About: "Have xAI spell out numbers and symbols before it speaks.", Default: false},
			{Key: Latency, Title: "Latency boost", About: "The sound quality xAI gives up to start sooner.", Help: "2 starts soonest.",
				Default: 0, Zero: speechconfig.SameAsVoice, Least: config.Num(0), Most: config.Num(2), Advanced: true},
			{Key: TTSBaseURL, Title: "Base URL", About: "The address xAI speech requests go to.", Help: "A Grok sign-in always uses its own address.", Default: "",
				Zero: "xAI's address", Advanced: true},
			{Key: StreamingURL, Title: "Live address", About: "The address of xAI speech that plays as it arrives.", Default: "", Zero: "xAI's live address",
				Advanced: true},
			{Key: SampleRate, Title: "Sample rate", About: "The sample rate of xAI speech.", Default: DefaultRate, Zero: "xAI's default", Counts: "Hz",
				Advanced: true},
			{Key: BitRate, Title: "Bitrate", About: "The audio quality of xAI speech.", Default: DefaultBitRate, Zero: "xAI's default", Counts: "bps",
				Advanced: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to xAI takes.", Help: "Longer text is split into several requests.",
				Default: 0, Zero: "15,000 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
		}},
		{Key: STT, Parent: speechconfig.STT, Title: "xAI", About: "Speech to text from Grok, with an xAI API key or sign-in", Order: 50, Settings: []config.Setting{
			{Key: STTModel, Title: "Model", About: "The xAI model that turns speech into text.", Default: "", Zero: "Voice Transcribe 2.0"},
			{Key: STTLanguage, Title: "Language", About: "The language of what you say.", Help: "A code such as en or de.", Default: "", Zero: speechconfig.SameAsVoice},
			{Key: Diarize, Title: "Label speakers", About: "Mark who is speaking in transcripts.", Default: false},
			{Key: Format, Title: "Tidy transcripts", About: "Have xAI tidy numbers and punctuation in transcripts.", Help: "Tidying happens only when a language is set.",
				Default: true},
			{Key: STTBaseURL, Title: "Base URL", About: "The address xAI speech to text requests go to.", Help: "A Grok sign-in always uses its own address.",
				Default: "", Zero: "xAI's address", Env: []string{STTBaseURLEnv}, Advanced: true},
		}},
	})
}
