package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "piper"
	Key           = speechconfig.TTS + "." + Name
	Voice         = Key + ".voice"
	VoicesDir     = Key + ".voices_dir"
	LengthScale   = Key + ".length_scale"
	NoiseScale    = Key + ".noise_scale"
	NoiseWScale   = Key + ".noise_w_scale"
	Volume        = Key + ".volume"
	Normalize     = Key + ".normalize_audio"
	CUDA          = Key + ".use_cuda"
	SpeakerID     = Key + ".speaker_id"
	MaxTextLength = Key + "." + speechconfig.MaxTextLength
	DefaultVoice  = "en_US-lessac-medium"
	// Characters one request takes when its setting is unset
	DefaultLimit = 5000
)
