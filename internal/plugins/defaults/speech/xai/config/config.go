package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name        = "xai"
	KeyEnv      = "XAI_API_KEY"
	DefaultBase = "https://api.x.ai/v1"
)

// Speech
const (
	TTS               = speechconfig.TTS + "." + Name
	TTSBaseURL        = TTS + "." + speechconfig.BaseURL
	StreamingURL      = TTS + ".streaming_url"
	VoiceID           = TTS + ".voice_id"
	TTSLanguage       = TTS + ".language"
	SampleRate        = TTS + ".sample_rate"
	BitRate           = TTS + ".bit_rate"
	Speed             = TTS + "." + speechconfig.ServiceSpeed
	Latency           = TTS + ".optimize_streaming_latency"
	AutoSpeechTags    = TTS + ".auto_speech_tags"
	TextNormalization = TTS + ".text_normalization"
	MaxTextLength     = TTS + "." + speechconfig.MaxTextLength
	DefaultVoice      = "eve"
	DefaultLanguage   = "en"
	DefaultRate       = 24000
	DefaultBitRate    = 128000
	DefaultStreaming  = "wss://api.x.ai/v1/tts"
	// Characters one request takes when its setting is unset
	DefaultLimit = 15000
)

// Speech recognition
const (
	STT           = speechconfig.STT + "." + Name
	STTBaseURL    = STT + "." + speechconfig.BaseURL
	STTModel      = STT + "." + speechconfig.ModelKey
	STTLanguage   = STT + ".language"
	Format        = STT + ".format"
	Diarize       = STT + ".diarize"
	STTBaseURLEnv = "XAI_STT_BASE_URL"
	// Model a blank setting or the retired grok-stt name stands for
	DefaultSTTModel = "grok-voice-transcribe-2.0"
)
