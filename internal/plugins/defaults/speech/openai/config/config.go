package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const Name = "openai"

// Speech
const (
	TTS                = speechconfig.TTS + "." + Name
	TTSBaseURL         = TTS + "." + speechconfig.BaseURL
	TTSModel           = TTS + "." + speechconfig.ModelKey
	TTSVoice           = TTS + ".voice"
	TTSLanguage        = TTS + ".language"
	ConsentAttestation = TTS + ".consent_attestation"
	Speed              = TTS + "." + speechconfig.ServiceSpeed
	PCMSampleRate      = TTS + ".pcm_sample_rate"
	MaxTextLength      = TTS + "." + speechconfig.MaxTextLength
	DefaultModel       = "gpt-4o-mini-tts"
	DefaultVoice       = "alloy"
	DefaultRate        = 24000
	// Characters one speech request takes when its setting is unset
	DefaultLimit = 4096
)

// Speech recognition
const (
	STT                   = speechconfig.STT + "." + Name
	STTBaseURL            = STT + "." + speechconfig.BaseURL
	STTModel              = STT + "." + speechconfig.ModelKey
	StreamingModel        = STT + ".streaming_model"
	STTLanguage           = STT + ".language"
	MaxRetries            = STT + ".max_retries"
	Timeout               = STT + ".timeout"
	DefaultSTTModel       = "whisper-1"
	DefaultStreamingModel = "gpt-live-transcribe"
	BaseURLEnv            = "STT_OPENAI_BASE_URL"
	ModelEnv              = "STT_OPENAI_MODEL"
)
