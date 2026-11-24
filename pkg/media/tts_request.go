package media

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One program of your own declared under tts.providers or stt.providers
type commandProvider struct {
	Name            string
	Command         string
	OutputFormat    string
	Voice           string
	Model           string
	Language        string
	Speed           float64
	SpeedSet        bool
	Timeout         time.Duration
	VoiceCompatible bool
	MaxTextLength   int
	Normalize       bool
	EnvPassthrough  []string
	WarmCommand     string
	ReleaseCommand  string
}

// Program of one typed providers entry with the defaults laid under it, nil when the entry names no command
func commandProviderOf(name string, raw any, def commandProvider) *commandProvider {
	m, _ := raw.(map[string]any)
	command, _ := m["command"].(string)
	command = strings.TrimSpace(command)
	kind, _ := m["type"].(string)
	if kind = strings.ToLower(strings.TrimSpace(kind)); command == "" || kind != "" && kind != "command" {
		return nil
	}
	p := def
	p.Name, p.Command = name, command
	for _, key := range []string{"output_format", "format"} {
		if v, ok := m[key].(string); ok && v != "" {
			p.OutputFormat = v
		}
	}
	p.Voice, _ = m["voice"].(string)
	p.Model, _ = m["model"].(string)
	p.Language, _ = m["language"].(string)
	if v, ok := m["speed"].(float64); ok {
		p.Speed, p.SpeedSet = v, true
	}
	for _, key := range []string{"timeout_seconds", "timeout"} {
		if v, ok := m[key].(time.Duration); ok && v > 0 {
			p.Timeout = v
		}
	}
	p.VoiceCompatible, _ = m["voice_compatible"].(bool)
	p.Normalize, _ = m["normalize"].(bool)
	if v, ok := m["max_text_length"].(int); ok && v > 0 {
		p.MaxTextLength = v
	}
	for _, s := range config.AsStrings(m["env_passthrough"]) {
		if s = strings.TrimSpace(s); s != "" {
			p.EnvPassthrough = append(p.EnvPassthrough, s)
		}
	}
	p.WarmCommand, _ = m["warm_command"].(string)
	p.ReleaseCommand, _ = m["release_command"].(string)
	return &p
}

// Programs of a typed providers map by lower case name, entries without a command and reserved names left out
func commandProvidersOf(entries map[string]any, reserved map[string]bool, def commandProvider) map[string]*commandProvider {
	out := map[string]*commandProvider{}
	for name, raw := range entries {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || reserved[key] {
			continue
		}
		if p := commandProviderOf(key, raw, def); p != nil {
			out[key] = p
		}
	}
	return out
}

// Value of a decimal setting when one is in effect, nil when unset
func floatOrNil(ctx context.Context, f *settings.Float) *float64 {
	if v, ok := f.Lookup(ctx); ok {
		return &v
	}
	return nil
}

// Value of a whole number setting when one is in effect, nil when unset
func intOrNil(ctx context.Context, n *settings.Int) *int {
	if v, ok := n.Lookup(ctx); ok {
		return &v
	}
	return nil
}

// Upload limits of one chat app from tts.delivery_profiles, zero fields meaning the built in value
type deliveryLimit struct {
	MaxFileBytes int
	SafetyRatio  float64
}

// Edge voice settings
type edgeSpeech struct {
	Voice string
	Speed *float64
}

// ElevenLabs voice settings
type elevenLabsSpeech struct {
	BaseURL          string
	VoiceID          string
	ModelID          string
	StreamingModelID string
}

// OpenAI voice settings
type openAISpeech struct {
	APIKey             string
	BaseURL            string
	Model              string
	Voice              string
	Language           string
	ConsentAttestation string
	Speed              *float64
	PCMSampleRate      int
}

// DeepInfra voice settings
type deepInfraSpeech struct {
	BaseURL string
	Model   string
	Voice   string
	Speed   *float64
}

// MiniMax voice settings
type miniMaxSpeech struct {
	BaseURL    string
	Region     string
	GroupID    string
	Model      string
	VoiceID    string
	Emotion    string
	Speed      *float64
	Vol        float64
	Pitch      int
	SampleRate int
	Bitrate    int
}

// xAI voice settings
type xaiSpeech struct {
	BaseURL           string
	StreamingURL      string
	VoiceID           string
	Language          string
	SampleRate        int
	BitRate           int
	Speed             *float64
	Latency           *int
	AutoSpeechTags    bool
	TextNormalization bool
}

// Mistral voice settings
type mistralSpeech struct {
	BaseURL string
	Model   string
	VoiceID string
}

// Gemini voice settings
type geminiSpeech struct {
	BaseURL           string
	Model             string
	Voice             string
	PersonaPromptFile string
	AudioTags         bool
}

// NeuTTS voice settings
type neuttsSpeech struct {
	Model    string
	Device   string
	RefAudio string
	RefText  string
}

// KittenTTS voice settings
type kittenSpeech struct {
	Model     string
	Voice     string
	Speed     *float64
	CleanText bool
}

// Piper voice settings
type piperSpeech struct {
	Voice          string
	VoicesDir      string
	LengthScale    *float64
	NoiseScale     *float64
	NoiseWScale    *float64
	Volume         float64
	NormalizeAudio bool
	UseCUDA        bool
	SpeakerID      int
}

// Speech settings of one synthesis: the registry's values in effect, with a call's own speed and voice laid over
type ttsRequest struct {
	// Configured service, lower case
	Provider string
	// Speed a call asked for, zero when it gave none
	CallSpeed float64
	// General speed, nil when unset
	Speed *float64
	// Voice and model plugin services get, empty for their own
	Voice string
	Model string
	// Audio format plugin services write, empty for theirs
	OutputFormat string
	// General live latency boost, nil when unset
	Latency *int
	// How long a local voice stays loaded after use
	KeepWarm time.Duration
	// Character caps of the built in services that have one set
	MaxTextLength map[string]int
	// Upload limits by chat app name
	Delivery map[string]deliveryLimit
	// Programs of your own by lower case name
	Commands map[string]*commandProvider
	// Service of speech that plays as the text arrives, empty for the main one
	StreamingProvider string
	// Smallest piece of text spoken at once while streaming
	StreamingMinLen int

	Edge       edgeSpeech
	ElevenLabs elevenLabsSpeech
	OpenAI     openAISpeech
	DeepInfra  deepInfraSpeech
	MiniMax    miniMaxSpeech
	XAI        xaiSpeech
	Mistral    mistralSpeech
	Gemini     geminiSpeech
	NeuTTS     neuttsSpeech
	KittenTTS  kittenSpeech
	Piper      piperSpeech
}

// Defaults of a program under tts.providers
var commandTTSDefaults = commandProvider{OutputFormat: defaultCommandTTSOutputFormat, Timeout: defaultCommandTTSTimeoutSeconds * time.Second, MaxTextLength: defaultCommandTTSMaxTextLength}

// Character cap settings of the built in services
var ttsMaxTextLengthSettings = map[string]*settings.Int{
	"edge": settings.TtsEdgeMaxTextLength, "elevenlabs": settings.TtsElevenlabsMaxTextLength, "openai": settings.TtsOpenaiMaxTextLength,
	"deepinfra": settings.TtsDeepinfraMaxTextLength, "minimax": settings.TtsMinimaxMaxTextLength, "xai": settings.TtsXaiMaxTextLength,
	"mistral": settings.TtsMistralMaxTextLength, "gemini": settings.TtsGeminiMaxTextLength, "neutts": settings.TtsNeuttsMaxTextLength,
	"kittentts": settings.TtsKittenttsMaxTextLength, "piper": settings.TtsPiperMaxTextLength,
}

// Speech settings in effect for ctx
func newTTSRequest(ctx context.Context) *ttsRequest {
	r := &ttsRequest{
		Provider:          strings.ToLower(strings.TrimSpace(settings.SpeechVoice.Get(ctx))),
		Speed:             floatOrNil(ctx, settings.TtsSpeed),
		Voice:             strings.TrimSpace(settings.TtsVoice.Get(ctx)),
		Model:             strings.TrimSpace(settings.TtsModel.Get(ctx)),
		OutputFormat:      strings.ToLower(strings.TrimSpace(settings.TtsOutputFormat.Get(ctx))),
		Latency:           intOrNil(ctx, settings.TtsOptimizeStreamingLatency),
		KeepWarm:          max(0, settings.TtsKeepWarmSeconds.Get(ctx)),
		MaxTextLength:     map[string]int{},
		Delivery:          map[string]deliveryLimit{},
		Commands:          commandProvidersOf(settings.TtsProviders.Get(ctx), builtinTTSProviders, commandTTSDefaults),
		StreamingProvider: strings.ToLower(strings.TrimSpace(settings.TtsStreamingProvider.Get(ctx))),
		StreamingMinLen:   max(1, settings.TtsStreamingMinLen.Get(ctx)),
		Edge:              edgeSpeech{Voice: settings.TtsEdgeVoice.Get(ctx), Speed: floatOrNil(ctx, settings.TtsEdgeSpeed)},
		ElevenLabs: elevenLabsSpeech{BaseURL: settings.TtsElevenlabsBaseUrl.Get(ctx), VoiceID: settings.TtsElevenlabsVoiceId.Get(ctx),
			ModelID: settings.TtsElevenlabsModelId.Get(ctx), StreamingModelID: settings.TtsElevenlabsStreamingModelId.Get(ctx)},
		OpenAI: openAISpeech{APIKey: settings.TtsOpenaiApiKey.Get(ctx), BaseURL: settings.TtsOpenaiBaseUrl.Get(ctx), Model: settings.TtsOpenaiModel.Get(ctx),
			Voice: settings.TtsOpenaiVoice.Get(ctx), Language: settings.TtsOpenaiLanguage.Get(ctx), ConsentAttestation: settings.TtsOpenaiConsentAttestation.Get(ctx),
			Speed: floatOrNil(ctx, settings.TtsOpenaiSpeed), PCMSampleRate: settings.TtsOpenaiPcmSampleRate.Get(ctx)},
		DeepInfra: deepInfraSpeech{BaseURL: settings.TtsDeepinfraBaseUrl.Get(ctx), Model: settings.TtsDeepinfraModel.Get(ctx),
			Voice: settings.TtsDeepinfraVoice.Get(ctx), Speed: floatOrNil(ctx, settings.TtsDeepinfraSpeed)},
		MiniMax: miniMaxSpeech{BaseURL: settings.TtsMinimaxBaseUrl.Get(ctx), Region: settings.TtsMinimaxRegion.Get(ctx), GroupID: settings.TtsMinimaxGroupId.Get(ctx),
			Model: settings.TtsMinimaxModel.Get(ctx), VoiceID: settings.TtsMinimaxVoiceId.Get(ctx), Emotion: settings.TtsMinimaxEmotion.Get(ctx),
			Speed: floatOrNil(ctx, settings.TtsMinimaxSpeed), Vol: settings.TtsMinimaxVol.Get(ctx), Pitch: settings.TtsMinimaxPitch.Get(ctx),
			SampleRate: settings.TtsMinimaxSampleRate.Get(ctx), Bitrate: settings.TtsMinimaxBitrate.Get(ctx)},
		XAI: xaiSpeech{BaseURL: settings.TtsXaiBaseUrl.Get(ctx), StreamingURL: settings.TtsXaiStreamingUrl.Get(ctx), VoiceID: settings.TtsXaiVoiceId.Get(ctx),
			Language: settings.TtsXaiLanguage.Get(ctx), SampleRate: settings.TtsXaiSampleRate.Get(ctx), BitRate: settings.TtsXaiBitRate.Get(ctx),
			Speed: floatOrNil(ctx, settings.TtsXaiSpeed), Latency: intOrNil(ctx, settings.TtsXaiOptimizeStreamingLatency),
			AutoSpeechTags: settings.TtsXaiAutoSpeechTags.Get(ctx), TextNormalization: settings.TtsXaiTextNormalization.Get(ctx)},
		Mistral: mistralSpeech{BaseURL: settings.TtsMistralBaseUrl.Get(ctx), Model: settings.TtsMistralModel.Get(ctx), VoiceID: settings.TtsMistralVoiceId.Get(ctx)},
		Gemini: geminiSpeech{BaseURL: settings.TtsGeminiBaseUrl.Get(ctx), Model: settings.TtsGeminiModel.Get(ctx), Voice: settings.TtsGeminiVoice.Get(ctx),
			PersonaPromptFile: settings.TtsGeminiPersonaPromptFile.Get(ctx), AudioTags: settings.TtsGeminiAudioTags.Get(ctx)},
		NeuTTS: neuttsSpeech{Model: settings.TtsNeuttsModel.Get(ctx), Device: settings.TtsNeuttsDevice.Get(ctx),
			RefAudio: settings.TtsNeuttsRefAudio.Get(ctx), RefText: settings.TtsNeuttsRefText.Get(ctx)},
		KittenTTS: kittenSpeech{Model: settings.TtsKittenttsModel.Get(ctx), Voice: settings.TtsKittenttsVoice.Get(ctx),
			Speed: floatOrNil(ctx, settings.TtsKittenttsSpeed), CleanText: settings.TtsKittenttsCleanText.Get(ctx)},
		Piper: piperSpeech{Voice: settings.TtsPiperVoice.Get(ctx), VoicesDir: settings.TtsPiperVoicesDir.Get(ctx),
			LengthScale: floatOrNil(ctx, settings.TtsPiperLengthScale), NoiseScale: floatOrNil(ctx, settings.TtsPiperNoiseScale),
			NoiseWScale: floatOrNil(ctx, settings.TtsPiperNoiseWScale), Volume: settings.TtsPiperVolume.Get(ctx),
			NormalizeAudio: settings.TtsPiperNormalizeAudio.Get(ctx), UseCUDA: settings.TtsPiperUseCuda.Get(ctx), SpeakerID: settings.TtsPiperSpeakerId.Get(ctx)},
	}
	if r.Provider == "" {
		r.Provider = defaultTTSProvider
	}
	for name, limit := range ttsMaxTextLengthSettings {
		if n, ok := limit.Lookup(ctx); ok && n > 0 {
			r.MaxTextLength[name] = n
		}
	}
	for name, raw := range settings.TtsDeliveryProfiles.Get(ctx) {
		m, _ := raw.(map[string]any)
		var limit deliveryLimit
		limit.MaxFileBytes, _ = m["max_file_bytes"].(int)
		limit.SafetyRatio, _ = m["safety_ratio"].(float64)
		r.Delivery[strings.ToLower(strings.TrimSpace(name))] = limit
	}
	return r
}

// Speech settings of the tool env's profile, the seam tests replace
var loadTTSRequest = func(ctx context.Context, env *tools.Env) *ttsRequest {
	return newTTSRequest(tools.WithEnvHome(ctx, env))
}

// Copy of the request with a call's own speed laid over, clamped to what the services take
func (r *ttsRequest) withCallSpeed(speed float64) *ttsRequest {
	out := *r
	out.CallSpeed = math.Max(0.25, math.Min(4.0, speed))
	return &out
}

// Speed a service speaks at: the call's own, then the service's setting, then the general one; false when none is set
func (r *ttsRequest) speedFor(own *float64) (float64, bool) {
	switch {
	case r.CallSpeed > 0:
		return r.CallSpeed, true
	case own != nil:
		return *own, true
	case r.Speed != nil:
		return *r.Speed, true
	}
	return 1.0, false
}

// Program of your own a provider name picks, nil for built in and undeclared names
func (r *ttsRequest) command(name string) *commandProvider {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || builtinTTSProviders[key] {
		return nil
	}
	return r.Commands[key]
}
