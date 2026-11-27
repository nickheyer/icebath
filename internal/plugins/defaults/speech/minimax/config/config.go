package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "minimax"
	Key           = speechconfig.TTS + "." + Name
	BaseURL       = Key + "." + speechconfig.BaseURL
	Region        = Key + ".region"
	GroupID       = Key + ".group_id"
	Model         = Key + "." + speechconfig.ModelKey
	VoiceID       = Key + ".voice_id"
	Emotion       = Key + ".emotion"
	Speed         = Key + "." + speechconfig.ServiceSpeed
	Volume        = Key + ".vol"
	Pitch         = Key + ".pitch"
	SampleRate    = Key + ".sample_rate"
	Bitrate       = Key + ".bitrate"
	MaxTextLength = Key + "." + speechconfig.MaxTextLength
	DefaultModel  = "speech-02-hd"
	DefaultVoice  = "English_expressive_narrator"
	GlobalBase    = "https://api.minimax.io/v1/t2a_v2"
	ChinaBase     = "https://api.minimaxi.com/v1/t2a_v2"
	GlobalKeyEnv  = "MINIMAX_API_KEY"
	ChinaKeyEnv   = "MINIMAX_CN_API_KEY"
	GroupEnv      = "MINIMAX_GROUP_ID"
	// Characters one request takes when its setting is unset
	DefaultLimit = 10000
)
