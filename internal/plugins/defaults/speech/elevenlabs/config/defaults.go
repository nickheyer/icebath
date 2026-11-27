package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "ElevenLabs speech", About: "ElevenLabs voices, which the model may pick per call; needs " + KeyEnv, Settings: []config.Setting{
			{Key: TTSBaseURL, Title: "ElevenLabs endpoint", About: "Custom endpoint for ElevenLabs speech; empty for ElevenLabs' own", Default: ""},
			{Key: ModelID, Title: "ElevenLabs model", About: "Speech model used with ElevenLabs", Default: DefaultModel},
			{Key: StreamingModelID, Title: "ElevenLabs live model", About: "Speech model for ElevenLabs when speaking as text arrives; empty uses ElevenLabs model",
				Default: ""},
			{Key: VoiceID, Title: "ElevenLabs voice", About: "Voice used with ElevenLabs, by its ID", Default: DefaultVoice},
			{Key: MaxTextLength, Title: "ElevenLabs text limit", About: "Most characters one ElevenLabs request takes, longer text split into several; 0 uses the " +
				"model's own limit", Default: 0},
		}},
		{Key: STT, Parent: speechconfig.STT, Title: "ElevenLabs recognition", About: "ElevenLabs Scribe transcription; needs " + KeyEnv, Settings: []config.Setting{
			{Key: STTBaseURL, Title: "ElevenLabs endpoint", About: "Custom endpoint for ElevenLabs speech recognition; empty for ElevenLabs' own", Default: "",
				Env: []string{STTBaseURLEnv}},
			{Key: STTModelID, Title: "ElevenLabs model", About: "Speech recognition model used with ElevenLabs", Default: DefaultSTTModel, Env: []string{STTModelEnv}},
			{Key: LanguageCode, Title: "ElevenLabs language", About: "Language code for ElevenLabs transcripts; empty detects it", Default: ""},
			{Key: Diarize, Title: "Label speakers", About: "Have ElevenLabs mark who is speaking in transcripts", Default: false},
			{Key: TagAudioEvents, Title: "Tag sounds", About: "Mark non-speech sounds such as laughter in ElevenLabs transcripts", Default: false},
		}},
	})
}
