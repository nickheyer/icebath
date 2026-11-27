package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name     = "sherpa"
	Key      = speechconfig.Wake + "." + Name
	ModelDir = Key + ".model_dir"
)
