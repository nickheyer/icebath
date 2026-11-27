package config

import speechconfig "github.com/nickheyer/icebath/internal/speech/config"

const (
	Name          = "local"
	Key           = speechconfig.STT + "." + Name
	ComputeType   = Key + ".compute_type"
	Device        = Key + ".device"
	InitialPrompt = Key + ".initial_prompt"
	Language      = Key + ".language"
	Logprob       = Key + ".logprob_threshold"
	Model         = Key + "." + speechconfig.ModelKey
	NoSpeech      = Key + ".no_speech_prob_threshold"
	UnloadIdle    = Key + ".unload_after_idle_seconds"
	VAD           = Key + ".vad"
	VADMinSilence = Key + ".vad_min_silence_ms"
	DefaultModel  = "base"
	// Python set the engine runs in
	Extra = "stt-whisper"
)
