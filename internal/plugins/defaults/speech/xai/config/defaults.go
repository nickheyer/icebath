package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "xAI speech", About: "Grok's voices, with expressive speech tags; the xAI key or the Grok sign in", Settings: []config.Setting{
			{Key: TTSBaseURL, Title: "xAI speech endpoint", About: "Custom endpoint for xAI speech; empty for the provider endpoint", Default: ""},
			{Key: StreamingURL, Title: "xAI live endpoint", About: "Custom endpoint for xAI speech that plays as it arrives; empty for the provider's", Default: ""},
			{Key: VoiceID, Title: "xAI voice", About: "Voice used with xAI, such as eve", Default: DefaultVoice},
			{Key: TTSLanguage, Title: "xAI language", About: "Language xAI speaks in, as a language code", Default: DefaultLanguage},
			{Key: SampleRate, Title: "xAI sample rate", About: "Audio sample rate for xAI speech, in Hz", Default: DefaultRate},
			{Key: BitRate, Title: "xAI bitrate", About: "Audio quality of xAI speech, in bits per second", Default: DefaultBitRate},
			{Key: Speed, Title: "xAI speed", About: "How fast xAI speaks, 1 being normal, held between 0.7 and 1.5; 0 uses Speech speed", Default: 0.0},
			{Key: Latency, Title: "xAI latency boost", About: "Trade quality for faster start in xAI speech, 0 to 2, 0 turning it off; unset uses Live latency boost",
				Default: 0},
			{Key: AutoSpeechTags, Title: "xAI auto tags", About: "Add expressive speech tags to text before xAI speaks it", Default: false},
			{Key: TextNormalization, Title: "xAI text cleanup", About: "Have xAI spell out numbers and symbols before speaking", Default: false},
			{Key: MaxTextLength, Title: "xAI text limit", About: "Most characters one xAI request takes, longer text split into several; 0 uses 15000", Default: 0},
		}},
		{Key: STT, Parent: speechconfig.STT, Title: "xAI recognition", About: "Grok's transcription; the xAI key or the Grok sign in", Settings: []config.Setting{
			{Key: STTBaseURL, Title: "xAI recognition endpoint", About: "Custom endpoint for xAI speech recognition; empty for the provider endpoint", Default: "",
				Env: []string{STTBaseURLEnv}},
			{Key: Diarize, Title: "xAI speakers", About: "Have xAI mark who is speaking in transcripts", Default: false},
			{Key: Format, Title: "xAI formatting", About: "Have xAI tidy numbers and punctuation in transcripts, when a language is set", Default: true},
			{Key: STTLanguage, Title: "xAI language", About: "Language hint for xAI transcripts; empty detects it", Default: ""},
			{Key: STTModel, Title: "xAI model", About: "Speech recognition model used with xAI; empty uses " + DefaultSTTModel, Default: ""},
		}},
	})
}
