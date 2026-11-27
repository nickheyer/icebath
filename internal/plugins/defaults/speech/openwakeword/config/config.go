package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name  = "openwakeword"
	Key   = speechconfig.Wake + "." + Name
	Model = Key + ".model"
	// Python set its models and TensorFlow Lite library come in
	Extra = "wake-openwakeword"
)
