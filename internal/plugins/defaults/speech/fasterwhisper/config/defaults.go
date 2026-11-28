package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.STT, Title: "Local Whisper", About: "Free, private speech to text that runs on this computer",
		Order: 10, Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The size of the model that runs on this computer.",
				Help: "One of base, tiny, small, medium, large-v3 or turbo. Local command uses this model too.", Default: DefaultModel},
			{Key: Language, Title: "Language", About: "The language of what you say.", Help: "A code such as en or de. Local command uses this language too.",
				Default: "", Zero: speechconfig.SameAsVoice},
			{Key: InitialPrompt, Title: "Hint text", About: "Names and terms that help Local Whisper spell them right.", Default: "",
				Zero: speechconfig.SameAsVoice},
			{Key: VAD, Title: "Skip silence", About: "Find the speech and skip silent parts before recognition.", Default: true, Advanced: true},
			{Key: VADMinSilence, Title: "Pause length", About: "How long a pause must last to count as a break in speech.", Default: 500, Counts: "milliseconds",
				Advanced: true},
			{Key: Device, Title: "Device", About: "The part of this computer that recognition runs on.", Help: "Such as cpu or cuda. The value auto uses the graphics card when it works.",
				Default: "auto", Advanced: true},
			{Key: ComputeType, Title: "Compute type", About: "The number format recognition computes in.", Help: "Such as int8. The value auto picks one for the device.",
				Default: "auto", Advanced: true},
			{Key: NoSpeech, Title: "No-speech cutoff", About: "The chance of no speech above which a clip counts as silence.",
				Default: 0.6, Counts: config.Share, Advanced: true},
			{Key: Logprob, Title: "Confidence cutoff", About: "The average confidence below which text is tried again and a silent piece dropped.",
				Default: -1.0, Advanced: true},
			{Key: UnloadIdle, Title: "Unload when idle", About: "How long the model may sit unused before it is freed.", Default: 0, Zero: "Kept loaded",
				Counts: "seconds", Least: config.Num(0), Advanced: true},
		}}})
}
