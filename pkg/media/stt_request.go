package media

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/settings"
)

// On-device faster-whisper settings
type localSTTSettings struct {
	Model           string
	Language        string
	InitialPrompt   string
	Device          string
	ComputeType     string
	VAD             bool
	VADMinSilenceMS int
	NoSpeechProb    float64
	Logprob         float64
	UnloadAfterIdle int
}

// OpenAI recognition settings
type openAISTTSettings struct {
	APIKey  string
	BaseURL string
	// The endpoint comes from STT_OPENAI_BASE_URL, so the env key may go to it
	BaseURLFromEnv bool
	Model          string
	StreamingModel string
	Language       string
	MaxRetries     int
	Timeout        float64
}

// Groq recognition settings
type groqSTTSettings struct {
	BaseURL  string
	Model    string
	Language string
}

// Mistral recognition settings
type mistralSTTSettings struct {
	Model    string
	Language string
}

// xAI recognition settings
type xaiSTTSettings struct {
	BaseURL  string
	Model    string
	Language string
	Format   bool
	Diarize  bool
}

// ElevenLabs recognition settings
type elevenLabsSTTSettings struct {
	BaseURL        string
	ModelID        string
	LanguageCode   string
	Diarize        bool
	TagAudioEvents bool
}

// DeepInfra recognition settings
type deepInfraSTTSettings struct {
	BaseURL string
	Model   string
}

// Speech recognition settings in effect: the registry's values for the context
type sttRequest struct {
	Enabled bool
	// Configured service as written, the default when unset
	Provider string
	// Whether the person chose the service rather than leaving the default
	ProviderSet bool
	// General recognition language, empty to detect it
	Language string
	// Static prompt priming cloud recognition
	Prompt    string
	Streaming bool
	// Silence trim before a cloud upload
	CloudTrim       bool
	CloudTrimDB     int
	CloudTrimKeepMS int
	// Program of the local_command service, empty to find one
	LocalCommand string
	// Programs of your own by lower case name
	Commands map[string]*commandProvider

	Local      localSTTSettings
	OpenAI     openAISTTSettings
	Groq       groqSTTSettings
	Mistral    mistralSTTSettings
	XAI        xaiSTTSettings
	ElevenLabs elevenLabsSTTSettings
	DeepInfra  deepInfraSTTSettings
}

// Defaults of a program under stt.providers
var commandSTTDefaults = commandProvider{OutputFormat: defaultCommandSTTOutputFormat, Timeout: defaultCommandSTTTimeoutSeconds * time.Second}

// Speech recognition settings in effect for ctx
func newSTTRequest(ctx context.Context) *sttRequest {
	provider, _ := settings.Resolve(ctx, settings.RecognitionService.Key)
	openAIBase, _ := settings.Resolve(ctx, settings.SttOpenaiBaseUrl.Key)
	return &sttRequest{
		Enabled:         settings.SpeechRecognition.Get(ctx),
		Provider:        strings.TrimSpace(settings.RecognitionService.Get(ctx)),
		ProviderSet:     provider.Source != settings.SourceDefault,
		Language:        strings.TrimSpace(settings.SttLanguage.Get(ctx)),
		Prompt:          settings.SttPrompt.Get(ctx),
		Streaming:       settings.SttStreaming.Get(ctx),
		CloudTrim:       settings.SttCloudTrimSilence.Get(ctx),
		CloudTrimDB:     settings.SttCloudTrimThresholdDb.Get(ctx),
		CloudTrimKeepMS: max(0, int(settings.SttCloudTrimKeepMs.Get(ctx)/time.Millisecond)),
		LocalCommand:    strings.TrimSpace(settings.SttLocalCommand.Get(ctx)),
		Commands:        commandProvidersOf(settings.SttProviders.Get(ctx), nonCommandSTTNames, commandSTTDefaults),
		Local: localSTTSettings{Model: settings.SttLocalModel.Get(ctx), Language: settings.SttLocalLanguage.Get(ctx),
			InitialPrompt: settings.SttLocalInitialPrompt.Get(ctx), Device: settings.SttLocalDevice.Get(ctx), ComputeType: settings.SttLocalComputeType.Get(ctx),
			VAD: settings.SttLocalVad.Get(ctx), VADMinSilenceMS: int(settings.SttLocalVadMinSilenceMs.Get(ctx) / time.Millisecond),
			NoSpeechProb: settings.SttLocalNoSpeechProbThreshold.Get(ctx), Logprob: settings.SttLocalLogprobThreshold.Get(ctx),
			UnloadAfterIdle: max(0, int(settings.SttLocalUnloadAfterIdleSeconds.Get(ctx)/time.Second))},
		OpenAI: openAISTTSettings{APIKey: settings.SttOpenaiApiKey.Get(ctx), BaseURL: settings.SttOpenaiBaseUrl.Get(ctx),
			BaseURLFromEnv: openAIBase.Source == settings.SourceEnv, Model: settings.SttOpenaiModel.Get(ctx),
			StreamingModel: settings.SttOpenaiStreamingModel.Get(ctx), Language: settings.SttOpenaiLanguage.Get(ctx),
			MaxRetries: settings.SttOpenaiMaxRetries.Get(ctx), Timeout: settings.SttOpenaiTimeout.Get(ctx).Seconds()},
		Groq:    groqSTTSettings{BaseURL: settings.SttGroqBaseUrl.Get(ctx), Model: settings.SttGroqModel.Get(ctx), Language: settings.SttGroqLanguage.Get(ctx)},
		Mistral: mistralSTTSettings{Model: settings.SttMistralModel.Get(ctx), Language: settings.SttMistralLanguage.Get(ctx)},
		XAI: xaiSTTSettings{BaseURL: settings.SttXaiBaseUrl.Get(ctx), Model: settings.SttXaiModel.Get(ctx), Language: settings.SttXaiLanguage.Get(ctx),
			Format: settings.SttXaiFormat.Get(ctx), Diarize: settings.SttXaiDiarize.Get(ctx)},
		ElevenLabs: elevenLabsSTTSettings{BaseURL: settings.SttElevenlabsBaseUrl.Get(ctx), ModelID: settings.SttElevenlabsModelId.Get(ctx),
			LanguageCode: settings.SttElevenlabsLanguageCode.Get(ctx), Diarize: settings.SttElevenlabsDiarize.Get(ctx),
			TagAudioEvents: settings.SttElevenlabsTagAudioEvents.Get(ctx)},
		DeepInfra: deepInfraSTTSettings{BaseURL: settings.SttDeepinfraBaseUrl.Get(ctx), Model: settings.SttDeepinfraModel.Get(ctx)},
	}
}

// Speech recognition settings of the context's profile, the seam tests replace
var loadSTTRequest = func(ctx context.Context) *sttRequest { return newSTTRequest(ctx) }

// Program of your own a recognition service name picks, nil for built in, reserved and undeclared names
func (r *sttRequest) command(name string) *commandProvider {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || nonCommandSTTNames[key] {
		return nil
	}
	return r.Commands[key]
}

// Own language settings of a built in service, first set wins
func (r *sttRequest) providerLanguages(ctx context.Context, provider string) []string {
	switch provider {
	case "local", "local_command":
		return []string{r.Local.Language}
	case "openai":
		return []string{r.OpenAI.Language}
	case "groq":
		return []string{r.Groq.Language}
	case "mistral":
		return []string{r.Mistral.Language}
	case "xai":
		return []string{r.XAI.Language}
	case "elevenlabs":
		return []string{r.ElevenLabs.LanguageCode}
	case "deepinfra":
		return nil
	}
	if p := r.command(provider); p != nil {
		return []string{p.Language}
	}
	return []string{providerField(ctx, "stt", provider, "language")}
}

// Text a plugin's own settings block holds at root.name.field, empty when nothing is set there
func providerField(ctx context.Context, root, name, field string) string {
	v, err := settings.Resolve(ctx, root+"."+name+"."+field)
	if err != nil || v.Value == nil {
		return ""
	}
	return strings.TrimSpace(pyrepr.Str(v.Value))
}
