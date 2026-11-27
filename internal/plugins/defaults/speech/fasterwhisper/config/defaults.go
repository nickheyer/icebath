package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.STT, Title: "Local Whisper", About: "Whisper on this machine through faster-whisper, free and private; " +
		"also the model and language of Local command",
		Settings: []config.Setting{
			{Key: ComputeType, Title: "Local compute type", About: "Number format the on-device recognizer computes in, such as int8; auto picks one for the device",
				Default: "auto"},
			{Key: Device, Title: "Local device", About: "Where on-device recognition runs, such as cpu or cuda; auto uses the graphics card when it works", Default: "auto"},
			{Key: InitialPrompt, Title: "Local prompt", About: "Text that primes the on-device recognizer, such as names or jargon", Default: ""},
			{Key: Language, Title: "Local language", About: "Language hint for on-device recognition; empty detects it", Default: ""},
			{Key: Logprob, Title: "Confidence cutoff", About: "Average confidence below which on-device text is retried, and a silent piece dropped", Default: -1.0},
			{Key: Model, Title: "Local model", About: "Size of the on-device speech recognition model: base, tiny, small, medium, large-v3 or turbo", Default: DefaultModel},
			{Key: NoSpeech, Title: "No-speech cutoff", About: "Chance of no speech above which a clip is treated as silence", Default: 0.6},
			{Key: UnloadIdle, Title: "Unload when idle", About: "Free the on-device recognizer after this long unused: seconds, or a length such as 10m; 0 keeps it " +
				"loaded", Default: 0},
			{Key: VAD, Title: "Skip silence", About: "Detect speech and skip silent parts before on-device recognition", Default: true},
			{Key: VADMinSilence, Title: "Silence length", About: "How long a pause must last to count as a break in speech: milliseconds, or a length such as 0.5s",
				Default: 500},
		}}})
}
