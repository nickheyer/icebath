package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "NeuTTS", About: "Copies a voice from a sample, on this computer",
		Order: 110, Settings: []config.Setting{
			{Key: RefAudio, Title: "Sample recording", About: "The recording of the voice to copy.", Default: "", Zero: "The bundled sample",
				Path: &config.Path{Kind: config.PathFile, Exists: true}},
			{Key: RefText, Title: "Sample transcript", About: "The file that holds the words of the sample recording.", Default: "",
				Zero: "The bundled transcript", Path: &config.Path{Kind: config.PathFile, Exists: true}},
			{Key: Model, Title: "Model", About: "The NeuTTS model that speaks.", Default: DefaultModel},
			{Key: Device, Title: "Device", About: "The part of this computer that NeuTTS runs on.", Help: "Such as cpu, cuda or mps.", Default: DefaultDevice,
				Advanced: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to NeuTTS takes.", Help: "Longer text is split into several requests.",
				Default: 0, Zero: "2,000 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
		}}})
}
