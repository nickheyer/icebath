package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.TTS, Title: "Piper", About: "Free voices in many languages that run on this computer",
		Order: 40, Settings: []config.Setting{
			{Key: Voice, Title: "Voice", About: "The Piper voice that speaks.",
				Help: "A name such as en_US-lessac-medium, downloaded on first use, or the path of an .onnx voice file.", Default: DefaultVoice},
			{Key: Volume, Title: "Volume", About: "The volume Piper speaks at.", Help: "1 leaves the volume as it is.", Default: 1.0},
			{Key: LengthScale, Title: "Pace", About: "The pace Piper speaks at, higher being slower.", Default: 0.0, Zero: "The voice's own"},
			{Key: NoiseScale, Title: "Variation", About: "The natural variation Piper adds to the voice.", Help: "A 0 you set yourself turns the variation off.",
				Default: 0.0, Zero: "The voice's own", Advanced: true},
			{Key: NoiseWScale, Title: "Timing variation", About: "The variation Piper adds to the timing of words.",
				Help: "A 0 you set yourself turns the variation off.", Default: 0.0, Zero: "The voice's own", Advanced: true},
			{Key: Normalize, Title: "Even out volume", About: "Level out the loudness of Piper speech.", Default: true, Advanced: true},
			{Key: CUDA, Title: "Use the graphics card", About: "Run Piper on the graphics card instead of the processor.", Default: false, Advanced: true},
			{Key: SpeakerID, Title: "Speaker", About: "The number of the speaker that talks, in a voice with several speakers.", Default: 0,
				Counts: config.Plain, Least: config.Num(0), Advanced: true},
			{Key: VoicesDir, Title: "Voices folder", About: "The folder where Piper finds voices of yours by name.", Default: "", Zero: "The profile's cache",
				Advanced: true},
			{Key: MaxTextLength, Title: "Text limit", About: "The most characters one request to Piper takes.", Help: "Longer text is split into several requests.",
				Default: 0, Zero: "5,000 characters", Counts: "characters", Least: config.Num(1), Advanced: true},
		}}})
}
