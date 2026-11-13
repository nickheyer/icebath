package config

import "gopkg.in/yaml.v3"

// Speech-to-text section, with command providers under providers and plugin provider blocks kept by name
type STTConfig struct {
	Enabled              bool                 `yaml:"enabled"`
	EchoTranscripts      bool                 `yaml:"echo_transcripts"`
	Language             string               `yaml:"language"`
	Streaming            bool                 `yaml:"streaming"`
	CloudTrimSilence     bool                 `yaml:"cloud_trim_silence"`
	CloudTrimThresholdDB int                  `yaml:"cloud_trim_threshold_db"`
	CloudTrimKeepMS      int                  `yaml:"cloud_trim_keep_ms"`
	Local                STTLocal             `yaml:"local"`
	Groq                 STTModelLanguage     `yaml:"groq"`
	OpenAI               STTOpenAI            `yaml:"openai"`
	Mistral              STTModelLanguage     `yaml:"mistral"`
	XAI                  STTModelLanguage     `yaml:"xai"`
	ElevenLabs           STTElevenLabs        `yaml:"elevenlabs"`
	DeepInfra            STTDeepInfra         `yaml:"deepinfra"`
	Provider             string               `yaml:"provider,omitempty"`
	Prompt               string               `yaml:"prompt,omitempty"`
	Providers            Map[CommandProvider] `yaml:"providers,omitempty"`
	Backend              string               `yaml:"backend,omitempty"`
	CloudProvider        string               `yaml:"cloud_provider,omitempty"`
	UseGateway           Scalar               `yaml:"use_gateway,omitempty"`
	rest                 rest
}

type sttFields STTConfig

// Decodes the mapping and keeps plugin provider blocks and unknown keys
func (s *STTConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sttFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s STTConfig) MarshalYAML() (any, error) {
	return encodeMapping(sttFields(s), s.rest)
}

// Block of a provider that is not built in, read as a command provider or plugin settings
func (s STTConfig) ProviderBlock(name string) (CommandProvider, bool, error) {
	if p, ok := s.Providers.Get(name); ok {
		return p, true, nil
	}
	return namedBlock(s.rest, name)
}

// Built-in transcription provider names
func BuiltinSTTProviders() []string {
	return []string{"local", "groq", "openai", "mistral", "elevenlabs", "deepinfra", "xai"}
}

// Local faster-whisper settings
type STTLocal struct {
	Model                  string  `yaml:"model"`
	Language               string  `yaml:"language"`
	InitialPrompt          string  `yaml:"initial_prompt"`
	VAD                    bool    `yaml:"vad" coerce:"boolkeep"`
	VADMinSilenceMS        int     `yaml:"vad_min_silence_ms"`
	NoSpeechProbThreshold  float64 `yaml:"no_speech_prob_threshold"`
	LogprobThreshold       float64 `yaml:"logprob_threshold"`
	UnloadAfterIdleSeconds int     `yaml:"unload_after_idle_seconds"`
	Device                 string  `yaml:"device,omitempty"`
	ComputeType            string  `yaml:"compute_type,omitempty"`
	rest                   rest
}

type sttLocalFields STTLocal

// Decodes the mapping and keeps unknown keys
func (l *STTLocal) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sttLocalFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l STTLocal) MarshalYAML() (any, error) {
	return encodeMapping(sttLocalFields(l), l.rest)
}

// Cloud transcription block keyed by model and language
type STTModelLanguage struct {
	Model    string `yaml:"model"`
	Language string `yaml:"language"`
	BaseURL  string `yaml:"base_url,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
	rest     rest
}

type sttModelLanguageFields STTModelLanguage

// Decodes the mapping and keeps unknown keys
func (s *STTModelLanguage) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sttModelLanguageFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s STTModelLanguage) MarshalYAML() (any, error) {
	return encodeMapping(sttModelLanguageFields(s), s.rest)
}

// OpenAI transcription settings
type STTOpenAI struct {
	Model          string  `yaml:"model"`
	StreamingModel string  `yaml:"streaming_model"`
	Language       string  `yaml:"language"`
	Timeout        float64 `yaml:"timeout"`
	MaxRetries     int     `yaml:"max_retries"`
	BaseURL        string  `yaml:"base_url,omitempty"`
	APIKey         string  `yaml:"api_key,omitempty"`
	rest           rest
}

type sttOpenAIFields STTOpenAI

// Decodes the mapping and keeps unknown keys
func (o *STTOpenAI) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sttOpenAIFields)(o), &o.rest)
}

// Writes the mapping with unknown keys after the named ones
func (o STTOpenAI) MarshalYAML() (any, error) {
	return encodeMapping(sttOpenAIFields(o), o.rest)
}

// ElevenLabs Scribe settings
type STTElevenLabs struct {
	ModelID        string `yaml:"model_id"`
	LanguageCode   string `yaml:"language_code"`
	TagAudioEvents bool   `yaml:"tag_audio_events"`
	Diarize        bool   `yaml:"diarize"`
	BaseURL        string `yaml:"base_url,omitempty"`
	rest           rest
}

type sttElevenLabsFields STTElevenLabs

// Decodes the mapping and keeps unknown keys
func (e *STTElevenLabs) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sttElevenLabsFields)(e), &e.rest)
}

// Writes the mapping with unknown keys after the named ones
func (e STTElevenLabs) MarshalYAML() (any, error) {
	return encodeMapping(sttElevenLabsFields(e), e.rest)
}

// DeepInfra transcription settings
type STTDeepInfra struct {
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url,omitempty"`
	rest    rest
}

type sttDeepInfraFields STTDeepInfra

// Decodes the mapping and keeps unknown keys
func (d *STTDeepInfra) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sttDeepInfraFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d STTDeepInfra) MarshalYAML() (any, error) {
	return encodeMapping(sttDeepInfraFields(d), d.rest)
}

// Voice mode section
type VoiceConfig struct {
	VoiceChatMode              VoiceChatMode `yaml:"voice_chat_mode"`
	GPTLive                    GPTLive       `yaml:"gpt_live"`
	RecordKey                  string        `yaml:"record_key"`
	SubmitMode                 SubmitMode    `yaml:"submit_mode"`
	MaxRecordingSeconds        float64       `yaml:"max_recording_seconds" coerce:"exact"`
	AutoTTS                    bool          `yaml:"auto_tts" coerce:"bool"`
	ClientDirect               bool          `yaml:"client_direct" coerce:"notfalse"`
	BeepEnabled                bool          `yaml:"beep_enabled"`
	BeepVolume                 float64       `yaml:"beep_volume"`
	ThinkingSound              bool          `yaml:"thinking_sound"`
	SilenceThreshold           int           `yaml:"silence_threshold" coerce:"exact"`
	SilenceDuration            float64       `yaml:"silence_duration" coerce:"exact"`
	BargeIn                    bool          `yaml:"barge_in" coerce:"bool"`
	BargeInGraceSeconds        float64       `yaml:"barge_in_grace_seconds"`
	BargeInThresholdMultiplier float64       `yaml:"barge_in_threshold_multiplier"`
	StopPhrases                StringList    `yaml:"stop_phrases"`
	rest                       rest
}

type voiceFields VoiceConfig

// Decodes the mapping and keeps unknown keys
func (v *VoiceConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*voiceFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VoiceConfig) MarshalYAML() (any, error) {
	return encodeMapping(voiceFields(v), v.rest)
}

// Full-duplex voice model settings
type GPTLive struct {
	Model        string `yaml:"model"`
	Voice        string `yaml:"voice"`
	Instructions string `yaml:"instructions"`
	APIKey       string `yaml:"api_key,omitempty"`
	BaseURL      string `yaml:"base_url,omitempty"`
	rest         rest
}

type gptLiveFields GPTLive

// Decodes the mapping and keeps unknown keys
func (g *GPTLive) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*gptLiveFields)(g), &g.rest)
}

// Writes the mapping with unknown keys after the named ones
func (g GPTLive) MarshalYAML() (any, error) {
	return encodeMapping(gptLiveFields(g), g.rest)
}

// How the desktop voice conversation is wired
type VoiceChatMode string

// Voice chat modes
const (
	VoiceChained VoiceChatMode = "chained"
	VoiceGPTLive VoiceChatMode = "gpt-live"
)

// Accepted voice chat modes, read case-insensitively with gpt_live, gptlive and live meaning gpt-live and any other value chained
func VoiceChatModes() []VoiceChatMode {
	return []VoiceChatMode{VoiceChained, VoiceGPTLive}
}

// What a finished TUI recording does
type SubmitMode string

// Recording submit modes
const (
	SubmitDirect SubmitMode = "direct"
	SubmitDraft  SubmitMode = "draft"
)

// Accepted recording submit modes
func SubmitModes() []SubmitMode {
	return []SubmitMode{SubmitDirect, SubmitDraft}
}

// Native image embedding budgets, replay_recent_images kept as written since a non-integer turns replay off
type VisionConfig struct {
	EmbedTargetBytes   int    `yaml:"embed_target_bytes" coerce:"strict"`
	MaxCallsPerImage   *int   `yaml:"max_calls_per_image" coerce:"strict"`
	ReplayRecentImages Scalar `yaml:"replay_recent_images"`
	rest               rest
}

type visionFields VisionConfig

// Decodes the mapping and keeps unknown keys
func (v *VisionConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*visionFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VisionConfig) MarshalYAML() (any, error) {
	return encodeMapping(visionFields(v), v.rest)
}

// Hands-free wake word section
type WakeWordConfig struct {
	Enabled            bool          `yaml:"enabled"`
	Surface            WakeSurface   `yaml:"surface"`
	InputDevice        *Scalar       `yaml:"input_device"`
	Capture            WakeCapture   `yaml:"capture"`
	Provider           string        `yaml:"provider"`
	Phrase             string        `yaml:"phrase"`
	Sensitivity        float64       `yaml:"sensitivity"`
	ConfirmationFrames int           `yaml:"confirmation_frames"`
	StartNewSession    bool          `yaml:"start_new_session" coerce:"bool"`
	ProfileRouting     bool          `yaml:"profile_routing" coerce:"bool"`
	OpenWakeWord       WakeModel     `yaml:"openwakeword"`
	Sherpa             WakeSherpa    `yaml:"sherpa"`
	Porcupine          WakePorcupine `yaml:"porcupine"`
	rest               rest
}

type wakeWordFields WakeWordConfig

// Decodes the mapping and keeps unknown keys
func (w *WakeWordConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*wakeWordFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WakeWordConfig) MarshalYAML() (any, error) {
	return encodeMapping(wakeWordFields(w), w.rest)
}

// Surface eligible to claim the wake word
type WakeSurface string

// Wake surfaces
const (
	WakeSurfaceAuto WakeSurface = "auto"
	WakeSurfaceCLI  WakeSurface = "cli"
	WakeSurfaceTUI  WakeSurface = "tui"
	WakeSurfaceGUI  WakeSurface = "gui"
)

// Accepted wake surfaces
func WakeSurfaces() []WakeSurface {
	return []WakeSurface{WakeSurfaceAuto, WakeSurfaceCLI, WakeSurfaceTUI, WakeSurfaceGUI}
}

// Where wake audio is captured
type WakeCapture string

// Wake capture sources
const (
	WakeCaptureAuto   WakeCapture = "auto"
	WakeCaptureLocal  WakeCapture = "local"
	WakeCaptureClient WakeCapture = "client"
)

// Accepted wake capture sources
func WakeCaptures() []WakeCapture {
	return []WakeCapture{WakeCaptureAuto, WakeCaptureLocal, WakeCaptureClient}
}

// Wake engines in auto preference order
func WakeProviders() []string {
	return []string{"auto", "openwakeword", "sherpa", "porcupine"}
}

// openWakeWord model choice
type WakeModel struct {
	Model string `yaml:"model"`
	rest  rest
}

type wakeModelFields WakeModel

// Decodes the mapping and keeps unknown keys
func (w *WakeModel) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*wakeModelFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WakeModel) MarshalYAML() (any, error) {
	return encodeMapping(wakeModelFields(w), w.rest)
}

// sherpa-onnx keyword model location
type WakeSherpa struct {
	ModelDir string `yaml:"model_dir"`
	rest     rest
}

type wakeSherpaFields WakeSherpa

// Decodes the mapping and keeps unknown keys
func (w *WakeSherpa) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*wakeSherpaFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WakeSherpa) MarshalYAML() (any, error) {
	return encodeMapping(wakeSherpaFields(w), w.rest)
}

// Porcupine keyword choice
type WakePorcupine struct {
	Keyword string `yaml:"keyword"`
	rest    rest
}

type wakePorcupineFields WakePorcupine

// Decodes the mapping and keeps unknown keys
func (w *WakePorcupine) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*wakePorcupineFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WakePorcupine) MarshalYAML() (any, error) {
	return encodeMapping(wakePorcupineFields(w), w.rest)
}

// Simulated typing delay before gateway replies
type HumanDelayConfig struct {
	Mode  HumanDelayMode `yaml:"mode"`
	MinMS int            `yaml:"min_ms"`
	MaxMS int            `yaml:"max_ms"`
	rest  rest
}

type humanDelayFields HumanDelayConfig

// Decodes the mapping and keeps unknown keys
func (h *HumanDelayConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*humanDelayFields)(h), &h.rest)
}

// Writes the mapping with unknown keys after the named ones
func (h HumanDelayConfig) MarshalYAML() (any, error) {
	return encodeMapping(humanDelayFields(h), h.rest)
}

// Typing delay mode
type HumanDelayMode string

// Typing delay modes
const (
	DelayOff     HumanDelayMode = "off"
	DelayNatural HumanDelayMode = "natural"
	DelayCustom  HumanDelayMode = "custom"
)

// Accepted typing delay modes, a false or empty value reading as off and any other value as natural
func HumanDelayModes() []HumanDelayMode {
	return []HumanDelayMode{DelayOff, DelayNatural, DelayCustom}
}

// Defaults of the transcription section
func defaultSTT() STTConfig {
	return STTConfig{
		Enabled:              true,
		EchoTranscripts:      true,
		Language:             "en",
		CloudTrimSilence:     true,
		CloudTrimThresholdDB: -40,
		CloudTrimKeepMS:      300,
		Local:                STTLocal{Model: "base", VAD: true, VADMinSilenceMS: 500, NoSpeechProbThreshold: 0.6, LogprobThreshold: -1.0},
		Groq:                 STTModelLanguage{Model: "whisper-large-v3-turbo"},
		OpenAI:               STTOpenAI{Model: "whisper-1", StreamingModel: "gpt-live-transcribe", Timeout: 60, MaxRetries: 1},
		Mistral:              STTModelLanguage{Model: "voxtral-mini-latest"},
		ElevenLabs:           STTElevenLabs{ModelID: "scribe_v2"},
	}
}

// Defaults of the voice section
func defaultVoice() VoiceConfig {
	return VoiceConfig{
		VoiceChatMode:              VoiceChained,
		GPTLive:                    GPTLive{Model: "gpt-live-1", Voice: "marin"},
		RecordKey:                  "ctrl+b",
		SubmitMode:                 SubmitDirect,
		MaxRecordingSeconds:        120,
		ClientDirect:               true,
		BeepEnabled:                true,
		BeepVolume:                 0.3,
		ThinkingSound:              true,
		SilenceThreshold:           200,
		SilenceDuration:            3.0,
		BargeIn:                    true,
		BargeInGraceSeconds:        0.5,
		BargeInThresholdMultiplier: 3.0,
		StopPhrases:                ListOf("stop"),
	}
}

// Defaults of the wake word section
func defaultWakeWord() WakeWordConfig {
	return WakeWordConfig{
		Surface:            WakeSurfaceAuto,
		Capture:            WakeCaptureAuto,
		Provider:           "auto",
		Phrase:             "hey icebath",
		Sensitivity:        0.6,
		ConfirmationFrames: 3,
		StartNewSession:    true,
		ProfileRouting:     true,
		OpenWakeWord:       WakeModel{Model: "hey_icebath"},
		Porcupine:          WakePorcupine{Keyword: "jarvis"},
	}
}

// Documentation of the speech sections
func speechDocs() map[string]string {
	return map[string]string{
		"stt.enabled":                         "Transcribe inbound voice messages.",
		"stt.echo_transcripts":                "Echo the raw transcript of gateway voice messages back as a message.",
		"stt.language":                        "Global language hint unless a per-provider one overrides it; \"en\" because Whisper auto-detect misreads short clips; \"\" = auto; \"es\", ... No provider is seeded (a stored value is an explicit pick; unset = autodetect): local | groq | openai | mistral | elevenlabs | deepinfra | xai.",
		"stt.streaming":                       "Live partial text while speaking (openai, xai, elevenlabs); on failure the file path is used.",
		"stt.cloud_trim_silence":              "Pre-upload ffmpeg silence trim (local whisper uses VAD); on failure the raw audio is uploaded.",
		"stt.cloud_trim_threshold_db":         "Audio quieter than this counts as silence.",
		"stt.cloud_trim_keep_ms":              "How much of each pause survives (natural pacing).",
		"stt.local.model":                     "tiny, base, small, medium, large-v3, turbo.",
		"stt.local.language":                  "Auto-detect; set \"en\", \"es\", ... to force.",
		"stt.local.vad":                       "Anti-hallucination (faster-whisper decodes junk from silence). Silero VAD filter (false = raw audio, for music and ambient). A segment is dropped only if no_speech_prob is ABOVE no_speech_prob_threshold AND avg_logprob is BELOW logprob_threshold.",
		"stt.local.vad_min_silence_ms":        "Min silence (ms) that splits speech chunks.",
		"stt.local.unload_after_idle_seconds": "0 = never; e.g. 300 frees the model after 5min.",
		"stt.groq.model":                      "whisper-large-v3-turbo, whisper-large-v3.",
		"stt.openai.model":                    "whisper-1, gpt-4o-mini-transcribe, gpt-4o-transcribe, gpt-transcribe.",
		"stt.openai.streaming_model":          "Model for stt.streaming; the one model with mid-utterance deltas.",
		"stt.openai.timeout":                  "Seconds; allow self-hosted backends time to cold-start.",
		"stt.openai.max_retries":              "OpenAI SDK transport retries.",
		"stt.mistral.model":                   "voxtral-mini-latest, voxtral-mini-2602.",
		"stt.xai.model":                       "\"\" = STT_XAI_MODEL or grok-voice-transcribe-2.0; or grok-voice-transcribe-1.0.",
		"stt.elevenlabs.model_id":             "scribe_v2, scribe_v1.",
		"stt.elevenlabs.language_code":        "Auto-detect; set \"eng\", \"spa\", ... to force.",
		"stt.deepinfra.model":                 "Empty = first stt-tagged model from the live catalog. An optional base_url key overrides DEEPINFRA_BASE_URL for STT only.",
		"voice.voice_chat_mode":               "How the Desktop voice conversation is wired. chained: STT -> icebath turn -> TTS (the stt.* and tts.* providers). gpt-live: one full-duplex voice model (OpenAI GPT-Live) owns the mic and speaker and DELEGATES every real request to icebath (any model or provider you have selected); needs an OpenAI API key. $0.05/min voice layer billing.",
		"voice.gpt_live.voice":                "marin | quartz | ripple | vesper | willow | stone | gleam | meridian | ...",
		"voice.gpt_live.instructions":         "Extra sentences appended to the live model's conversation persona (tone, pacing, language). Optional api_key and base_url keys override the OpenAI audio credentials for this mode only.",
		"voice.record_key":                    "Push-to-talk key.",
		"voice.submit_mode":                   "TUI: direct submits immediately; draft = editable transcript.",
		"voice.max_recording_seconds":         "Longest single recording.",
		"voice.auto_tts":                      "Speak every reply in voice mode.",
		"voice.client_direct":                 "Desktop remote clients call STT and TTS providers DIRECTLY (config + key fetched over authenticated REST at session start) instead of relaying via the gateway.",
		"voice.beep_enabled":                  "Record start and stop beeps in CLI voice mode.",
		"voice.beep_volume":                   "Beep amplitude multiplier, 0.0-1.0.",
		"voice.thinking_sound":                "Ambient bubble sound while the agent works (volume = beep_volume).",
		"voice.silence_threshold":             "RMS below this = silence (0-32767).",
		"voice.silence_duration":              "Seconds of silence before auto-stop.",
		"voice.barge_in":                      "Interrupt the agent and stop TTS when the user starts talking.",
		"voice.barge_in_grace_seconds":        "Trip suppression after TTS onset (the mic stays live the whole turn).",
		"voice.barge_in_threshold_multiplier": "Speech trigger = quiet-room floor x this (floor calibrated BEFORE playback).",
		"voice.stop_phrases":                  "Saying EXACTLY one of these (case-insensitive, punctuation ignored) ends the voice chat instead of going to the agent. [] disables.",
		"vision":                              "Native images (vision_analyze, browser screenshots, replayed user images) are re-sent on every later call.",
		"vision.embed_target_bytes":           "Byte budget for one embedded image (clamped 64 KiB..4 MiB). Raise it for dense phone screenshots of tables the model calls \"unreadable\" at 256 KB.",
		"vision.max_calls_per_image":          "How often vision_analyze may embed the SAME image (region crops included) per session. null = 3 inside delegated subagents (they run unattended), unlimited for the main agent; an explicit number applies everywhere; 0 = unlimited.",
		"vision.replay_recent_images":         "Newest user images re-sent on turns rebuilt from state.db; 0 = off.",
		"wake_word":                           "\"Hey icebath\" hands-free wake word: always-on, on-device hotword detection that starts a fresh voice session. Off by default; toggle with /wake.",
		"wake_word.surface":                   "Eligible surface: \"auto\" (first claimant) | \"cli\" | \"tui\" | \"gui\".",
		"wake_word.input_device":              "PortAudio input device index or name; null = process default.",
		"wake_word.capture":                   "auto | local | client (desktop streams the mic via wake.feed).",
		"wake_word.provider":                  "auto: first platform-supported engine (openwakeword, sherpa, porcupine). Explicit choices stay pinned. Porcupine needs PORCUPINE_ACCESS_KEY.",
		"wake_word.phrase":                    "sherpa: this IS the detected phrase; other engines: a cosmetic label (detection is keyed by the model or keyword below).",
		"wake_word.sensitivity":               "0.0-1.0 threshold, consistent across engines (higher = stricter).",
		"wake_word.confirmation_frames":       "openWakeWord only: consecutive over-threshold frames to fire (higher = fewer false triggers, more latency; 1 = single-frame).",
		"wake_word.start_new_session":         "Fresh session on wake vs. continue the current one.",
		"wake_word.profile_routing":           "sherpa only: listen for every wake-enabled profile's phrase and route to it.",
		"wake_word.openwakeword.model":        "\"hey_icebath\" | a built-in openWakeWord name (\"hey_jarvis\", \"alexa\", ...) | path to a custom .tflite model.",
		"wake_word.sherpa.model_dir":          "sherpa-onnx KWS model dir; empty = auto-download the small English zipformer.",
		"wake_word.porcupine.keyword":         "Built-in keyword (\"jarvis\", \"computer\", ...) or path to a custom .ppn.",
		"human_delay":                         "Simulated typing delay before gateway replies.",
		"human_delay.mode":                    "off | natural (800-2500 ms) | custom (min_ms to max_ms).",
	}
}
