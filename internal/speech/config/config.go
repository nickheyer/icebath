package config

// Talking to the agent and hearing its replies
const (
	Voice              = "voice"
	Input              = Voice + ".input_enabled"
	SpeakReplies       = Voice + ".speak_replies"
	SubmitMode         = Voice + ".submit_mode"
	BargeIn            = Voice + ".barge_in"
	BargeInGrace       = Voice + ".barge_in_grace_seconds"
	BargeInMultiplier  = Voice + ".barge_in_threshold_multiplier"
	Beep               = Voice + ".beep_enabled"
	BeepVolume         = Voice + ".beep_volume"
	Debug              = Voice + ".debug"
	MaxRecording       = Voice + ".max_recording_seconds"
	SilenceDuration    = Voice + ".silence_duration"
	SilenceThreshold   = Voice + ".silence_threshold"
	StopPhrases        = Voice + ".stop_phrases"
	ThinkingSound      = Voice + ".thinking_sound"
	DebugEnv           = "ICEBATH_VOICE_DEBUG"
	SubmitDirect       = "direct"
	SubmitDraft        = "draft"
	DefaultSilence     = 200
	DefaultSilenceSecs = 3.0
	DefaultBeepVolume  = 0.3
)

// Text to speech
const (
	TTS               = "tts"
	TTSProvider       = TTS + ".provider"
	Speed             = TTS + ".speed"
	PluginVoice       = TTS + ".voice"
	PluginModel       = TTS + ".model"
	OutputFormat      = TTS + ".output_format"
	Latency           = TTS + ".optimize_streaming_latency"
	KeepWarm          = TTS + ".keep_warm_seconds"
	Delivery          = TTS + ".delivery_profiles"
	TTSCommands       = TTS + ".providers"
	StreamingMinLen   = TTS + ".streaming.min_len"
	StreamingProvider = TTS + ".streaming.provider"
	DefaultVoice      = "edge"
	// tts.streaming.provider value that tries every service that streams, in their order
	AutoStreaming = "auto"
)

// Speech to text
const (
	STT             = "stt"
	STTEnabled      = STT + ".enabled"
	STTProvider     = STT + ".provider"
	EchoTranscripts = STT + ".echo_transcripts"
	Language        = STT + ".language"
	Prompt          = STT + ".prompt"
	Streaming       = STT + ".streaming"
	TrimSilence     = STT + ".cloud_trim_silence"
	TrimThreshold   = STT + ".cloud_trim_threshold_db"
	TrimKeep        = STT + ".cloud_trim_keep_ms"
	STTCommands     = STT + ".providers"
	LocalCommand    = STT + ".local_command"
	LanguageEnv     = "ICEBATH_LOCAL_STT_LANGUAGE"
	LocalCommandEnv = "ICEBATH_LOCAL_STT_COMMAND"
	// Name of the engine of this machine, which a launch may ask for
	LocalRecognizer = "local"
)

// Wake word
const (
	Wake                   = "wake_word"
	WakeEnabled            = Wake + ".enabled"
	WakeSurface            = Wake + ".surface"
	WakeInput              = Wake + ".input_device"
	WakeProvider           = Wake + ".provider"
	WakePhrase             = Wake + ".phrase"
	WakeSensitivity        = Wake + ".sensitivity"
	WakeConfirmationFrames = Wake + ".confirmation_frames"
	WakeNewSession         = Wake + ".start_new_session"
	WakeProfileRouting     = Wake + ".profile_routing"
	DefaultPhrase          = "hey icebath"
	AutoWake               = "auto"
	// Variable of the keys file that holds the Picovoice access key Porcupine needs
	AccessKeyEnv = "PORCUPINE_ACCESS_KEY"
)

// Kinds of thing a setting of speech names, by which a screen of speech registers the picker of each
const (
	// A microphone of this computer
	Microphone = "speech.microphone"
)

// Helper model tasks of speech
const (
	// Adds tone and speech tags to text before a voice reads it
	AudioTags = "tts_audio_tags"
	// Answers a turn the user spoke, faster than the session's model
	VoiceChat = "voice_chat"
)

// Words of the value of a service that follows the value kept for every service until it holds its own
const SameAsVoice = "Same as Voice"

// Keys every service keeps under its own section
const (
	MaxTextLength = "max_text_length"
	ServiceSpeed  = "speed"
	BaseURL       = "base_url"
	APIKey        = "api_key"
	ModelKey      = "model"
)
