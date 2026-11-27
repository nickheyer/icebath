package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name           = "porcupine"
	Key            = speechconfig.Wake + "." + Name
	Keyword        = Key + ".keyword"
	DefaultKeyword = "jarvis"
	// Python set its library, model and keywords come in
	Extra = "wake-porcupine"
)
