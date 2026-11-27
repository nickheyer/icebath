package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "kittentts"
	Key           = speechconfig.TTS + "." + Name
	Model         = Key + "." + speechconfig.ModelKey
	Voice         = Key + ".voice"
	Speed         = Key + "." + speechconfig.ServiceSpeed
	CleanText     = Key + ".clean_text"
	MaxTextLength = Key + "." + speechconfig.MaxTextLength
	DefaultModel  = "KittenML/kitten-tts-nano-0.8-int8"
	DefaultVoice  = "Jasper"
	// Python set the engine runs in
	Extra = "kittentts"
	// Characters one request takes when its setting is unset
	DefaultLimit = 2000
)
