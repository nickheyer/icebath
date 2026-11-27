package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "Piper speech", About: "Piper's voices on this machine, in over forty languages, no key needed",
		Settings: []config.Setting{
			{Key: Voice, Title: "Piper voice", About: "Voice used with Piper: a catalog name such as en_US-lessac-medium, fetched on first use, or the path of an .onnx " +
				"voice file", Default: DefaultVoice},
			{Key: VoicesDir, Title: "Piper voices folder", About: "Folder where your own Piper voices are found by name; empty for the voices folder in the profile's " +
				"cache", Default: ""},
			{Key: LengthScale, Title: "Piper pace", About: "Stretches or shortens Piper speech, higher being slower; unset uses the voice's own value", Default: 0.0},
			{Key: NoiseScale, Title: "Piper variation", About: "How much natural variation Piper adds to the voice; unset uses the voice's own value", Default: 0.0},
			{Key: NoiseWScale, Title: "Piper timing variation", About: "How much Piper varies the timing of words; unset uses the voice's own value", Default: 0.0},
			{Key: Volume, Title: "Piper volume", About: "How loud Piper speaks, 1 being unchanged", Default: 1.0},
			{Key: Normalize, Title: "Even out volume", About: "Level out loudness in Piper speech", Default: true},
			{Key: CUDA, Title: "Piper on GPU", About: "Run Piper on the graphics card instead of the processor", Default: false},
			{Key: SpeakerID, Title: "Piper speaker", About: "Which speaker to use in a multi-speaker Piper voice", Default: 0},
			{Key: MaxTextLength, Title: "Piper text limit", About: "Most characters one Piper request takes, longer text split into several; 0 uses 5000", Default: 0},
		}}})
}
