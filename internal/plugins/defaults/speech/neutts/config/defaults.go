package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "NeuTTS speech", About: "A voice cloning model that runs on this machine; copies a sample " +
		"recording you give it", Settings: []config.Setting{
		{Key: Device, Title: "NeuTTS device", About: "Where NeuTTS runs, such as cpu, cuda or mps", Default: DefaultDevice},
		{Key: Model, Title: "NeuTTS model", About: "On-device speech model used with NeuTTS", Default: DefaultModel},
		{Key: RefAudio, Title: "NeuTTS sample audio", About: "Path to a sample recording of the voice to copy; empty uses the bundled sample", Default: ""},
		{Key: RefText, Title: "NeuTTS sample text", About: "Path to the transcript of the sample recording; empty uses the bundled sample's", Default: ""},
		{Key: MaxTextLength, Title: "NeuTTS text limit", About: "Most characters one NeuTTS request takes, longer text split into several; 0 uses 2000", Default: 0},
	}}})
}
