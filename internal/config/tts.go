package config

import "gopkg.in/yaml.v3"

// Text-to-speech section, with command providers under providers and plugin provider blocks kept by name
type TTSConfig struct {
	Provider                 string               `yaml:"provider"`
	KeepWarmSeconds          float64              `yaml:"keep_warm_seconds"`
	Streaming                TTSStreaming         `yaml:"streaming"`
	Edge                     TTSEdge              `yaml:"edge"`
	ElevenLabs               TTSElevenLabs        `yaml:"elevenlabs"`
	OpenAI                   TTSOpenAI            `yaml:"openai"`
	Gemini                   TTSGemini            `yaml:"gemini"`
	XAI                      TTSXAI               `yaml:"xai"`
	Mistral                  TTSVoiceModel        `yaml:"mistral"`
	MiniMax                  TTSMiniMax           `yaml:"minimax"`
	KittenTTS                TTSKittenTTS         `yaml:"kittentts"`
	NeuTTS                   TTSNeuTTS            `yaml:"neutts"`
	Piper                    TTSPiper             `yaml:"piper"`
	DeepInfra                TTSModelVoice        `yaml:"deepinfra"`
	Speed                    *float64             `yaml:"speed,omitempty"`
	OptimizeStreamingLatency *int                 `yaml:"optimize_streaming_latency,omitempty"`
	Providers                Map[CommandProvider] `yaml:"providers,omitempty"`
	Backend                  string               `yaml:"backend,omitempty"`
	CloudProvider            string               `yaml:"cloud_provider,omitempty"`
	UseGateway               Scalar               `yaml:"use_gateway,omitempty"`
	rest                     rest
}

type ttsFields TTSConfig

// Decodes the mapping and keeps plugin provider blocks and unknown keys
func (t *TTSConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t TTSConfig) MarshalYAML() (any, error) {
	return encodeMapping(ttsFields(t), t.rest)
}

// Block of a provider that is not built in, read as a command provider or plugin settings
func (t TTSConfig) ProviderBlock(name string) (CommandProvider, bool, error) {
	if p, ok := t.Providers.Get(name); ok {
		return p, true, nil
	}
	return namedBlock(t.rest, name)
}

// Decodes a provider block kept among the unknown keys of a section
func namedBlock(r rest, name string) (CommandProvider, bool, error) {
	n := r.get(name)
	if n == nil || n.Kind != yaml.MappingNode {
		return CommandProvider{}, false, nil
	}
	var p CommandProvider
	err := p.UnmarshalYAML(n)
	return p, true, err
}

// Built-in speech provider names, any other provider value naming a providers entry
func BuiltinTTSProviders() []string {
	return []string{"edge", "elevenlabs", "openai", "minimax", "xai", "mistral", "gemini", "neutts", "kittentts", "piper", "deepinfra"}
}

// Optional per-provider overrides any built-in speech provider block accepts
type TTSOverrides struct {
	MaxTextLength int      `yaml:"max_text_length,omitempty"`
	BaseURL       string   `yaml:"base_url,omitempty"`
	APIKey        string   `yaml:"api_key,omitempty"`
	Speed         *float64 `yaml:"speed,omitempty"`
}

// User-declared shell command speech or transcription provider
type CommandProvider struct {
	Type            string   `yaml:"type,omitempty"`
	Command         string   `yaml:"command,omitempty" coerce:"exact"`
	Timeout         *float64 `yaml:"timeout,omitempty"`
	TimeoutSeconds  *float64 `yaml:"timeout_seconds,omitempty"`
	Format          string   `yaml:"format,omitempty"`
	OutputFormat    string   `yaml:"output_format,omitempty"`
	Voice           string   `yaml:"voice,omitempty"`
	Model           string   `yaml:"model,omitempty"`
	Speed           Scalar   `yaml:"speed,omitempty"`
	Language        string   `yaml:"language,omitempty"`
	MaxTextLength   int      `yaml:"max_text_length,omitempty"`
	VoiceCompatible bool     `yaml:"voice_compatible,omitempty"`
	EnvPassthrough  []string `yaml:"env_passthrough,omitempty"`
	Normalize       bool     `yaml:"normalize,omitempty"`
	rest            rest
}

type commandProviderFields CommandProvider

// Decodes the mapping and keeps unknown keys
func (c *CommandProvider) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*commandProviderFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CommandProvider) MarshalYAML() (any, error) {
	return encodeMapping(commandProviderFields(c), c.rest)
}

// Audio formats a command speech provider may write
func CommandTTSFormats() []string {
	return []string{"mp3", "wav", "ogg", "flac", "m4a", "aac", "amr", "opus"}
}

// Streaming speech settings
type TTSStreaming struct {
	MinLen   int    `yaml:"min_len"`
	Provider string `yaml:"provider,omitempty"`
	rest     rest
}

type ttsStreamingFields TTSStreaming

// Decodes the mapping and keeps unknown keys
func (s *TTSStreaming) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsStreamingFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s TTSStreaming) MarshalYAML() (any, error) {
	return encodeMapping(ttsStreamingFields(s), s.rest)
}

// Edge speech voice
type TTSEdge struct {
	Voice        string `yaml:"voice"`
	TTSOverrides `yaml:",inline"`
	rest         rest
}

type ttsEdgeFields TTSEdge

// Decodes the mapping and keeps unknown keys
func (e *TTSEdge) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsEdgeFields)(e), &e.rest)
}

// Writes the mapping with unknown keys after the named ones
func (e TTSEdge) MarshalYAML() (any, error) {
	return encodeMapping(ttsEdgeFields(e), e.rest)
}

// ElevenLabs speech voice and model
type TTSElevenLabs struct {
	VoiceID          string `yaml:"voice_id"`
	ModelID          string `yaml:"model_id"`
	StreamingModelID string `yaml:"streaming_model_id,omitempty"`
	WSSURL           string `yaml:"wss_url,omitempty"`
	TTSOverrides     `yaml:",inline"`
	rest             rest
}

type ttsElevenLabsFields TTSElevenLabs

// Decodes the mapping and keeps unknown keys
func (e *TTSElevenLabs) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsElevenLabsFields)(e), &e.rest)
}

// Writes the mapping with unknown keys after the named ones
func (e TTSElevenLabs) MarshalYAML() (any, error) {
	return encodeMapping(ttsElevenLabsFields(e), e.rest)
}

// OpenAI speech settings
type TTSOpenAI struct {
	Model              string `yaml:"model"`
	Voice              string `yaml:"voice"`
	ConsentAttestation string `yaml:"consent_attestation"`
	PCMSampleRate      int    `yaml:"pcm_sample_rate"`
	Language           string `yaml:"language,omitempty"`
	TTSOverrides       `yaml:",inline"`
	rest               rest
}

type ttsOpenAIFields TTSOpenAI

// Decodes the mapping and keeps unknown keys
func (o *TTSOpenAI) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsOpenAIFields)(o), &o.rest)
}

// Writes the mapping with unknown keys after the named ones
func (o TTSOpenAI) MarshalYAML() (any, error) {
	return encodeMapping(ttsOpenAIFields(o), o.rest)
}

// Gemini speech settings
type TTSGemini struct {
	Model             string `yaml:"model"`
	Voice             string `yaml:"voice"`
	AudioTags         bool   `yaml:"audio_tags"`
	PersonaPromptFile string `yaml:"persona_prompt_file"`
	TTSOverrides      `yaml:",inline"`
	rest              rest
}

type ttsGeminiFields TTSGemini

// Decodes the mapping and keeps unknown keys
func (g *TTSGemini) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsGeminiFields)(g), &g.rest)
}

// Writes the mapping with unknown keys after the named ones
func (g TTSGemini) MarshalYAML() (any, error) {
	return encodeMapping(ttsGeminiFields(g), g.rest)
}

// xAI speech settings
type TTSXAI struct {
	VoiceID                  string  `yaml:"voice_id"`
	Language                 string  `yaml:"language"`
	Speed                    float64 `yaml:"speed"`
	AutoSpeechTags           bool    `yaml:"auto_speech_tags"`
	OptimizeStreamingLatency int     `yaml:"optimize_streaming_latency"`
	SampleRate               int     `yaml:"sample_rate"`
	BitRate                  int     `yaml:"bit_rate"`
	MaxTextLength            int     `yaml:"max_text_length,omitempty"`
	BaseURL                  string  `yaml:"base_url,omitempty"`
	APIKey                   string  `yaml:"api_key,omitempty"`
	SpeechTags               *bool   `yaml:"speech_tags,omitempty"`
	TextNormalization        Scalar  `yaml:"text_normalization,omitempty"`
	StreamingURL             string  `yaml:"streaming_url,omitempty"`
	rest                     rest
}

type ttsXAIFields TTSXAI

// Decodes the mapping and keeps unknown keys
func (x *TTSXAI) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsXAIFields)(x), &x.rest)
}

// Writes the mapping with unknown keys after the named ones
func (x TTSXAI) MarshalYAML() (any, error) {
	return encodeMapping(ttsXAIFields(x), x.rest)
}

// Speech block keyed by model and voice id
type TTSVoiceModel struct {
	Model        string `yaml:"model"`
	VoiceID      string `yaml:"voice_id"`
	TTSOverrides `yaml:",inline"`
	rest         rest
}

type ttsVoiceModelFields TTSVoiceModel

// Decodes the mapping and keeps unknown keys
func (v *TTSVoiceModel) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsVoiceModelFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v TTSVoiceModel) MarshalYAML() (any, error) {
	return encodeMapping(ttsVoiceModelFields(v), v.rest)
}

// Speech block keyed by model and voice name
type TTSModelVoice struct {
	Model        string `yaml:"model"`
	Voice        string `yaml:"voice"`
	TTSOverrides `yaml:",inline"`
	rest         rest
}

type ttsModelVoiceFields TTSModelVoice

// Decodes the mapping and keeps unknown keys
func (v *TTSModelVoice) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsModelVoiceFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v TTSModelVoice) MarshalYAML() (any, error) {
	return encodeMapping(ttsModelVoiceFields(v), v.rest)
}

// MiniMax speech settings, the voice and audio knobs sent with each request
type TTSMiniMax struct {
	Model        string   `yaml:"model"`
	VoiceID      string   `yaml:"voice_id"`
	Region       string   `yaml:"region,omitempty"`
	GroupID      string   `yaml:"group_id,omitempty"`
	Vol          *float64 `yaml:"vol,omitempty"`
	Pitch        *int     `yaml:"pitch,omitempty"`
	Emotion      string   `yaml:"emotion,omitempty"`
	SampleRate   *int     `yaml:"sample_rate,omitempty"`
	Bitrate      *int     `yaml:"bitrate,omitempty"`
	TTSOverrides `yaml:",inline"`
	rest         rest
}

type ttsMiniMaxFields TTSMiniMax

// Decodes the mapping and keeps unknown keys
func (m *TTSMiniMax) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsMiniMaxFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m TTSMiniMax) MarshalYAML() (any, error) {
	return encodeMapping(ttsMiniMaxFields(m), m.rest)
}

// MiniMax endpoint regions, an empty region picking cn only when that is the sole credential
func MiniMaxRegions() []string {
	return []string{"global", "cn"}
}

// Local KittenTTS settings
type TTSKittenTTS struct {
	Model        string `yaml:"model"`
	Voice        string `yaml:"voice"`
	CleanText    *bool  `yaml:"clean_text,omitempty"`
	TTSOverrides `yaml:",inline"`
	rest         rest
}

type ttsKittenTTSFields TTSKittenTTS

// Decodes the mapping and keeps unknown keys
func (k *TTSKittenTTS) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsKittenTTSFields)(k), &k.rest)
}

// Writes the mapping with unknown keys after the named ones
func (k TTSKittenTTS) MarshalYAML() (any, error) {
	return encodeMapping(ttsKittenTTSFields(k), k.rest)
}

// Local NeuTTS settings
type TTSNeuTTS struct {
	RefAudio      string `yaml:"ref_audio"`
	RefText       string `yaml:"ref_text"`
	Model         string `yaml:"model"`
	Device        string `yaml:"device"`
	MaxTextLength int    `yaml:"max_text_length,omitempty"`
	rest          rest
}

type ttsNeuTTSFields TTSNeuTTS

// Decodes the mapping and keeps unknown keys
func (t *TTSNeuTTS) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsNeuTTSFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t TTSNeuTTS) MarshalYAML() (any, error) {
	return encodeMapping(ttsNeuTTSFields(t), t.rest)
}

// Local Piper settings
type TTSPiper struct {
	Voice          string   `yaml:"voice"`
	VoicesDir      string   `yaml:"voices_dir,omitempty"`
	UseCUDA        *bool    `yaml:"use_cuda,omitempty" coerce:"bool"`
	LengthScale    *float64 `yaml:"length_scale,omitempty"`
	NoiseScale     *float64 `yaml:"noise_scale,omitempty"`
	NoiseWScale    *float64 `yaml:"noise_w_scale,omitempty"`
	Volume         *float64 `yaml:"volume,omitempty"`
	NormalizeAudio *bool    `yaml:"normalize_audio,omitempty" coerce:"bool"`
	SpeakerID      *int     `yaml:"speaker_id,omitempty" coerce:"exactint"`
	MaxTextLength  int      `yaml:"max_text_length,omitempty"`
	rest           rest
}

type ttsPiperFields TTSPiper

// Decodes the mapping and keeps unknown keys
func (p *TTSPiper) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*ttsPiperFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p TTSPiper) MarshalYAML() (any, error) {
	return encodeMapping(ttsPiperFields(p), p.rest)
}

// Defaults of the speech section
func defaultTTS() TTSConfig {
	return TTSConfig{
		Provider:        "edge",
		KeepWarmSeconds: 60,
		Streaming:       TTSStreaming{MinLen: 20},
		Edge:            TTSEdge{Voice: "en-US-AriaNeural"},
		ElevenLabs:      TTSElevenLabs{VoiceID: "pNInz6obpgDQGcFmaJgB", ModelID: "eleven_multilingual_v2"},
		OpenAI:          TTSOpenAI{Model: "gpt-4o-mini-tts", Voice: "alloy", PCMSampleRate: 24000},
		Gemini:          TTSGemini{Model: "gemini-2.5-flash-preview-tts", Voice: "Kore"},
		XAI:             TTSXAI{VoiceID: "eve", Language: "en", Speed: 1.0, SampleRate: 24000, BitRate: 128000},
		Mistral:         TTSVoiceModel{Model: "voxtral-mini-tts-2603", VoiceID: "c69964a6-ab8b-4f8a-9465-ec0925096ec8"},
		MiniMax:         TTSMiniMax{Model: "speech-02-hd", VoiceID: "English_expressive_narrator"},
		KittenTTS:       TTSKittenTTS{Model: "KittenML/kitten-tts-nano-0.8-int8", Voice: "Jasper"},
		NeuTTS:          TTSNeuTTS{Model: "neuphonic/neutts-air-q4-gguf", Device: "cpu"},
		Piper:           TTSPiper{Voice: "en_US-lessac-medium"},
		DeepInfra:       TTSModelVoice{Voice: "default"},
	}
}

// Documentation of the speech section
func ttsDocs() map[string]string {
	return map[string]string{
		"tts":                                "Text-to-speech. Each provider accepts an optional `max_text_length:` override for the per-request input-character cap; omit to use the provider's documented limit (OpenAI 4096, xAI 15000, MiniMax 10000, ElevenLabs 5k-40k model-aware, Gemini 32000, Edge 5000, Mistral 4000, NeuTTS and KittenTTS 2000).",
		"tts.provider":                       "\"edge\" (free) | \"elevenlabs\" (premium) | \"openai\" | \"xai\" | \"minimax\" | \"mistral\" | \"gemini\" | \"deepinfra\" | \"neutts\" (local) | \"kittentts\" (local) | \"piper\" (local), or the name of a tts.providers command entry.",
		"tts.keep_warm_seconds":              "Seconds a local engine (Piper, KittenTTS) stays loaded after the last speech toggle turns off, so a quick re-activation (wake word, voice-chat restart) skips the reload. 0 unloads immediately.",
		"tts.streaming.min_len":              "Shortest first sentence (chars) spoken on its own by streaming TTS; shorter openers ride with the next sentence. 20 suits English; CJK voice setups use ~6.",
		"tts.streaming.provider":             "Streaming speech engine: a provider name pins that streamer (no streaming when it is unusable), auto picks the first usable one, empty follows tts.provider. A pinned name never substitutes another voice.",
		"tts.minimax.region":                 "MiniMax endpoint and credential: global (MINIMAX_API_KEY) or cn (MINIMAX_CN_API_KEY); empty picks cn only when it is the sole configured credential.",
		"tts.minimax.group_id":               "GroupId added to the request URL when absent there; empty falls back to MINIMAX_GROUP_ID.",
		"tts.elevenlabs.wss_url":             "WebSocket base used with a custom base_url; empty derives it from base_url with a ws or wss scheme.",
		"tts.openai.language":                "Sent to OpenAI-compatible servers as lang_code; empty sends nothing.",
		"tts.edge.voice":                     "Popular: AriaNeural, JennyNeural, AndrewNeural, BrianNeural, SoniaNeural.",
		"tts.elevenlabs.voice_id":            "ElevenLabs voice id (default Adam).",
		"tts.openai.voice":                   "gpt-4o-mini-tts voices: alloy, ash, ballad, cedar, coral, echo, fable, marin, nova, onyx, sage, shimmer, verse.",
		"tts.openai.consent_attestation":     "Forwarded verbatim in the request body for OpenAI-compatible servers whose cloned voices demand it (400 consent_required otherwise); \"\" sends nothing.",
		"tts.openai.pcm_sample_rate":         "Raw PCM rate for streaming playback. OpenAI emits 24 kHz; a compatible endpoint that reports its rate (X-Audio-Sample-Rate header) overrides this automatically.",
		"tts.gemini.audio_tags":              "Gemini 3.1: an aux-model rewrite inserts [audio tags] into the TTS script only.",
		"tts.gemini.persona_prompt_file":     "Optional local text file with performance direction; may include a `{transcript}` marker, else the live transcript is appended.",
		"tts.xai.voice_id":                   "Voice id, or a custom voice ID (docs.x.ai custom voices).",
		"tts.xai.language":                   "BCP-47 code (\"en\", \"pt-BR\") or \"auto\".",
		"tts.xai.speed":                      "0.7 to 1.5.",
		"tts.xai.auto_speech_tags":           "Insert expressive audio tags via an LLM rewrite.",
		"tts.xai.optimize_streaming_latency": "0 to 2, trades quality for lower latency.",
		"tts.xai.sample_rate":                "22050 / 24000 / 44100 / 48000.",
		"tts.xai.bit_rate":                   "MP3 bitrate; only applies when codec=mp3.",
		"tts.mistral.voice_id":               "Voice id (default Paul, Neutral).",
		"tts.kittentts.model":                "nano 25MB; micro 41MB; mini 80MB.",
		"tts.neutts.ref_audio":               "Path to reference voice audio (empty = bundled default).",
		"tts.neutts.ref_text":                "Path to reference voice transcript (empty = bundled default).",
		"tts.neutts.model":                   "HuggingFace model repo.",
		"tts.neutts.device":                  "cpu, cuda, or mps.",
		"tts.piper.voice":                    "Voice name (downloaded on first use) or absolute path to a .onnx file; list: github.com/OHF-Voice/piper1-gpl/blob/main/docs/VOICES.md. Optional keys: voices_dir (~/.icebath/cache/piper-voices/), use_cuda, length_scale (2.0 = twice as slow), noise_scale, noise_w_scale, volume, normalize_audio.",
		"tts.deepinfra.model":                "Empty = first tts-tagged model from the live catalog. An optional base_url key overrides DEEPINFRA_BASE_URL for TTS only.",
		"tts.providers":                      "User-declared command providers: tts.providers.<name>: {type: command, command: \"piper -f {output_path} < {input_path}\"}. Template fields {input_path}/{text_path}, {output_path}, {format}, {voice}, {model}, {speed} are shell-quoted; built-in provider names always win over a same-named entry.",
	}
}
