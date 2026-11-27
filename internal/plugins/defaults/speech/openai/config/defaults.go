package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "OpenAI speech", About: "OpenAI's voices, and those of servers that copy its speech API", Settings: []config.Setting{
			{Key: TTSAPIKey, Title: "OpenAI speech key", About: "Key for OpenAI speech; empty uses VOICE_TOOLS_OPENAI_KEY, then the OpenAI key of the environment or of /providers",
				Default: "", Secret: true},
			{Key: TTSBaseURL, Title: "OpenAI speech endpoint", About: "Custom endpoint of an OpenAI compatible speech server; empty for OpenAI's", Default: ""},
			{Key: TTSModel, Title: "OpenAI model", About: "Speech model used with OpenAI", Default: DefaultModel},
			{Key: TTSVoice, Title: "OpenAI voice", About: "Voice used with OpenAI, such as alloy, nova or shimmer", Default: DefaultVoice},
			{Key: TTSLanguage, Title: "OpenAI language", About: "Language hint sent as lang_code with OpenAI compatible speech; empty sends none", Default: ""},
			{Key: ConsentAttestation, Title: "Voice consent", About: "Consent statement sent when using a custom cloned voice", Default: ""},
			{Key: Speed, Title: "OpenAI speed", About: "How fast OpenAI speaks, from 0.25 to 4, 1 being normal; 0 uses Speech speed", Default: 0.0},
			{Key: PCMSampleRate, Title: "Live sample rate", About: "Sample rate in Hz of OpenAI speech that plays as it arrives, unless the server reports its own",
				Default: DefaultRate},
			{Key: MaxTextLength, Title: "OpenAI text limit", About: "Most characters one OpenAI request takes, longer text split into several; 0 uses 4096", Default: 0},
		}},
		{Key: STT, Parent: speechconfig.STT, Title: "OpenAI recognition", About: "OpenAI's transcription, and that of servers that copy its API", Settings: []config.Setting{
			{Key: STTAPIKey, Title: "OpenAI recognition key", About: "Key for OpenAI speech recognition; empty uses VOICE_TOOLS_OPENAI_KEY, then the OpenAI key of the " +
				"environment or of /providers", Default: "", Secret: true},
			{Key: STTBaseURL, Title: "OpenAI recognition endpoint", About: "Custom endpoint of an OpenAI compatible transcription server; empty for OpenAI's. A key " +
				"of the environment goes to it only when it is set by " + BaseURLEnv + " or is a private address", Default: "", Env: []string{BaseURLEnv}},
			{Key: STTModel, Title: "OpenAI model", About: "Speech recognition model used with OpenAI, such as whisper-1 or gpt-4o-transcribe", Default: DefaultSTTModel,
				Env: []string{ModelEnv}},
			{Key: StreamingModel, Title: "OpenAI live model", About: "Model used for live transcription with OpenAI", Default: DefaultStreamingModel},
			{Key: STTLanguage, Title: "OpenAI language", About: "Language hint for OpenAI transcripts; empty detects it", Default: ""},
			{Key: MaxRetries, Title: "Retry attempts", About: "How many times to retry a failed recognition request", Default: 1},
			{Key: Timeout, Title: "OpenAI timeout", About: "How long to wait for OpenAI to transcribe before giving up: seconds, or a length such as 90s or 2m",
				Default: 60},
		}},
	})
}
