package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "edge"
	Key           = speechconfig.TTS + "." + Name
	Voice         = Key + ".voice"
	Speed         = Key + "." + speechconfig.ServiceSpeed
	MaxTextLength = Key + "." + speechconfig.MaxTextLength
	DefaultVoice  = "en-US-AriaNeural"
	// Characters one Edge request takes when its setting is unset
	DefaultLimit = 5000
)
