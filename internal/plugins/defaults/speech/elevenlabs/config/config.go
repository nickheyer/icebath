package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name   = "elevenlabs"
	KeyEnv = "ELEVENLABS_API_KEY"
	Link   = "https://elevenlabs.io/"
	// Page where a key is made
	KeysLink = "https://elevenlabs.io/app/settings/api-keys"
)

// Speech
const (
	TTS              = speechconfig.TTS + "." + Name
	TTSBaseURL       = TTS + "." + speechconfig.BaseURL
	ModelID          = TTS + ".model_id"
	StreamingModelID = TTS + ".streaming_model_id"
	VoiceID          = TTS + ".voice_id"
	MaxTextLength    = TTS + "." + speechconfig.MaxTextLength
	DefaultModel     = "eleven_multilingual_v2"
	DefaultVoice     = "pNInz6obpgDQGcFmaJgB"
	DefaultBase      = "https://api.elevenlabs.io"
	// Characters one request of a model not in the table takes
	DefaultLimit = 10000
)

// Speech recognition
const (
	STT             = speechconfig.STT + "." + Name
	STTBaseURL      = STT + "." + speechconfig.BaseURL
	STTModelID      = STT + ".model_id"
	LanguageCode    = STT + ".language_code"
	Diarize         = STT + ".diarize"
	TagAudioEvents  = STT + ".tag_audio_events"
	DefaultSTTModel = "scribe_v2"
	DefaultSTTBase  = "https://api.elevenlabs.io/v1"
	STTBaseURLEnv   = "ELEVENLABS_STT_BASE_URL"
	STTModelEnv     = "STT_ELEVENLABS_MODEL"
	// Model of live transcription
	RealtimeModel = "scribe_v2_realtime"
)
