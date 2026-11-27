package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{
		{Key: TTS, Parent: speechconfig.TTS, Title: "Mistral speech", About: "Mistral's Voxtral voices; needs " + KeyEnv, Settings: []config.Setting{
			{Key: TTSBaseURL, Title: "Mistral endpoint", About: "Custom endpoint for Mistral speech; empty for Mistral's own", Default: ""},
			{Key: TTSModel, Title: "Mistral model", About: "Speech model used with Mistral", Default: DefaultModel},
			{Key: VoiceID, Title: "Mistral voice", About: "Voice used with Mistral, by its ID", Default: DefaultVoice},
			{Key: MaxTextLength, Title: "Mistral text limit", About: "Most characters one Mistral request takes, longer text split into several; 0 uses 4000", Default: 0},
		}},
		{Key: STT, Parent: speechconfig.STT, Title: "Mistral recognition", About: "Mistral's Voxtral Transcribe; needs " + KeyEnv, Settings: []config.Setting{
			{Key: STTLanguage, Title: "Mistral language", About: "Language hint for Mistral transcripts; empty detects it", Default: ""},
			{Key: STTModel, Title: "Mistral model", About: "Speech recognition model used with Mistral", Default: DefaultSTTModel},
		}},
	})
}
