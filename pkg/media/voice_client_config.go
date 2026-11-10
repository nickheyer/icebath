package media

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	sttWireOpenAI     = "openai-multipart"
	sttWireXAI        = "xai-stt"
	sttWireElevenLabs = "elevenlabs-stt"
	ttsWireOpenAI     = "openai-speech"
	ttsWireElevenLabs = "elevenlabs-tts"
)

// Whether voice.client_direct allows client direct voice, true unless set to a false spelling
func clientDirectEnabled(ctx context.Context) bool {
	value, ok := voiceConfig(ctx)["client_direct"]
	if !ok {
		return true
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch pystr.Lower(pystr.Strip(v)) {
		case "0", "false", "no", "off":
			return false
		}
	}
	return true
}

func relayVoiceConfig(reason string) *pyjson.Object {
	return jsonObject("mode", "relay", "reason", reason)
}

// A provider section of a config, empty when the config or the section is not a mapping
func providerSection(cfg map[string]any, name string) map[string]any {
	if section, ok := asStrMap(cfg[name]); ok {
		return section
	}
	return map[string]any{}
}

func directVoiceConfig(wire, providerName string, baseURL any, apiKey string, model any, extra ...any) *pyjson.Object {
	return jsonObject(append([]any{"mode", "direct", "wire", wire, "provider", providerName, "base_url", baseURL,
		"api_key", apiKey, "model", model}, extra...)...)
}

// The Whisper hallucination phrases and repeat regex a client applies to direct transcripts
func sttHallucinationFilter() *pyjson.Object {
	phrases := make([]string, 0, len(whisperHallucinations))
	for phrase := range whisperHallucinations {
		phrases = append(phrases, phrase)
	}
	sort.Strings(phrases)
	list := make([]any, len(phrases))
	for i, p := range phrases {
		list[i] = p
	}
	return jsonObject("phrases", list, "repeat_regex", `^(?:thank you|thanks|bye|you|ok|okay|the end|[.,!\s])+$`)
}

// The configured DeepInfra model, else the first catalog model of the kind, nil when the catalog is empty
func deepinfraModel(ctx context.Context, section map[string]any, kind string) any {
	if v := section["model"]; pyTruthy(v) {
		return v
	}
	if ids := provider.DeepinfraModelIDs(ctx, kind, false); len(ids) > 0 {
		return ids[0]
	}
	return nil
}

func truthyOr(v, def any) any {
	if pyTruthy(v) {
		return v
	}
	return def
}

// Direct STT wire for the active built in provider with its credentials, relay for local, plugin or keyless setups
func resolveSTTClientConfig(ctx context.Context) (*pyjson.Object, error) {
	sttConfig := loadSTTConfig(ctx)
	if !isSTTEnabled(ctx, sttConfig) {
		return relayVoiceConfig("stt disabled"), nil
	}
	providerName, err := getProvider(ctx, sttConfig)
	if err != nil {
		return nil, err
	}
	if isLocalSTTProvider(providerName) {
		return relayVoiceConfig("local provider"), nil
	}
	if !builtinSTTProviders[providerName] {
		return relayVoiceConfig("command/plugin provider"), nil
	}
	var extra []string
	if providerName == "elevenlabs" {
		extra = []string{"language_code"}
	}
	var language any
	if lang := resolveSTTLanguage(ctx, providerName, sttConfig, extra...); lang != "" {
		language = lang
	}
	section := providerSection(sttConfig, providerName)
	timeout := configNumberFloat(providerSection(sttConfig, "openai"), "timeout", 60.0)
	direct := func(wire string, baseURL any, apiKey string, model any) *pyjson.Object {
		return directVoiceConfig(wire, providerName, baseURL, apiKey, model, "language", language, "timeout_s", timeout,
			"hallucination_filter", sttHallucinationFilter())
	}
	envBaseURL := func(envVar, def string) string {
		base := ""
		if v := section["base_url"]; pyTruthy(v) {
			base = pyrepr.Str(v)
		} else if v := sttEnvValue(ctx, envVar); v != "" {
			base = v
		} else {
			base = def
		}
		return strings.TrimRight(pystr.Strip(base), "/")
	}
	switch providerName {
	case "groq", "mistral":
		envVar, defaultModel, base := "GROQ_API_KEY", sttDefaultGroqModel, groqSTTBaseURL
		if providerName == "mistral" {
			envVar, defaultModel, base = "MISTRAL_API_KEY", sttDefaultMistralModel, "https://api.mistral.ai/v1"
		}
		apiKey := resolveSTTProviderKey(ctx, envVar, providerName)
		if apiKey == "" {
			return relayVoiceConfig("no credentials"), nil
		}
		return direct(sttWireOpenAI, base, apiKey, truthyOr(section["model"], defaultModel)), nil
	case "openai":
		apiKey, baseURL, err := resolveSTTOpenAIAudioClientConfig(ctx)
		var valueErr *sttValueError
		if errors.As(err, &valueErr) {
			return relayVoiceConfig(fmt.Sprintf("openai resolution failed: %s", err)), nil
		}
		if err != nil {
			return nil, err
		}
		return direct(sttWireOpenAI, baseURL, apiKey, truthyOr(section["model"], sttDefaultOpenAIModel)), nil
	case "xai":
		apiKey := pystr.Strip(sttEnvValue(ctx, "XAI_API_KEY"))
		if apiKey == "" {
			return relayVoiceConfig("xai oauth (server-managed) or no credentials"), nil
		}
		model := ""
		if v := section["model"]; pyTruthy(v) {
			model = pyrepr.Str(v)
		}
		return direct(sttWireXAI, envBaseURL("XAI_STT_BASE_URL", xaiSTTBaseURL), apiKey, normalizeXAISTTModel(model)), nil
	case "elevenlabs":
		apiKey := resolveSTTProviderKey(ctx, "ELEVENLABS_API_KEY", "elevenlabs")
		if apiKey == "" {
			return relayVoiceConfig("no credentials"), nil
		}
		return direct(sttWireElevenLabs, envBaseURL("ELEVENLABS_STT_BASE_URL", elevenLabsSTTBaseURL), apiKey,
			truthyOr(section["model"], sttDefaultElevenLabsModel)), nil
	case "deepinfra":
		apiKey := resolveSTTProviderKey(ctx, "DEEPINFRA_API_KEY", "deepinfra")
		if apiKey == "" {
			return relayVoiceConfig("no credentials"), nil
		}
		model := deepinfraModel(ctx, section, "stt")
		if model == nil {
			return relayVoiceConfig("no deepinfra stt model"), nil
		}
		return direct(sttWireOpenAI, provider.DeepinfraBaseURL(ctx, section), apiKey, model), nil
	}
	return relayVoiceConfig(fmt.Sprintf("provider %s has no client wire", pyrepr.Repr(providerName))), nil
}

// Direct TTS wire for openai, elevenlabs or deepinfra with voice, speed and chunking, relay otherwise
func resolveTTSClientConfig(ctx context.Context) (*pyjson.Object, error) {
	ttsConfig := loadTTSConfig(ctx, nil)
	providerName := getTTSProvider(ttsConfig)
	if !builtinTTSProviders[providerName] {
		return relayVoiceConfig("command/plugin provider"), nil
	}
	minLen := SentenceChunkerFromConfig(ttsConfig).MinLen
	switch providerName {
	case "openai":
		route, err := resolveOpenAIAudioClientConfig(ctx, nil)
		var valueErr *ttsValueError
		if errors.As(err, &valueErr) {
			return relayVoiceConfig(fmt.Sprintf("openai resolution failed: %s", err)), nil
		}
		if err != nil {
			return nil, err
		}
		oai := providerSection(ttsConfig, "openai")
		model := truthyOr(oai["model"], defaultOpenAITTSModel)
		configBase := oai["base_url"]
		baseURL := truthyOr(configBase, route.BaseURL)
		if route.IsManaged && !pyTruthy(configBase) && !managedOpenAITTSModels[pyrepr.Str(model)] {
			model = defaultOpenAITTSModel
		}
		speedDefault, ok := ttsConfig["speed"]
		if !ok {
			speedDefault = 1.0
		}
		rawSpeed, ok := oai["speed"]
		if !ok {
			rawSpeed = speedDefault
		}
		speed, err := pyint.Float(rawSpeed)
		if err != nil {
			speed = 1.0
		}
		return directVoiceConfig(ttsWireOpenAI, "openai", baseURL, route.APIKey, model,
			"voice", truthyOr(oai["voice"], defaultOpenAITTSVoice), "speed", speed,
			"extra_body", openAIExtraBody(oai), "min_len", minLen), nil
	case "elevenlabs":
		apiKey := ttsResolveProviderKey(nil, "ELEVENLABS_API_KEY", "elevenlabs")
		if apiKey == "" {
			return relayVoiceConfig("no credentials"), nil
		}
		el := providerSection(ttsConfig, "elevenlabs")
		base := strings.TrimRight(pyrepr.Str(truthyOr(el["base_url"], "https://api.elevenlabs.io/v1")), "/")
		return directVoiceConfig(ttsWireElevenLabs, "elevenlabs", base, apiKey, truthyOr(el["model_id"], defaultElevenLabsModelID),
			"voice", truthyOr(el["voice_id"], defaultElevenLabsVoiceID), "speed", nil, "min_len", minLen), nil
	case "deepinfra":
		apiKey := ttsResolveProviderKey(nil, "DEEPINFRA_API_KEY", "deepinfra")
		if apiKey == "" {
			return relayVoiceConfig("no credentials"), nil
		}
		di := providerSection(ttsConfig, "deepinfra")
		model := deepinfraModel(ctx, di, "tts")
		if model == nil {
			return relayVoiceConfig("no deepinfra tts model"), nil
		}
		return directVoiceConfig(ttsWireOpenAI, "deepinfra", provider.DeepinfraBaseURL(ctx, di), apiKey, model,
			"voice", truthyOr(di["voice"], "af_bella"), "speed", nil, "min_len", minLen), nil
	}
	return relayVoiceConfig(fmt.Sprintf("provider %s has no client wire", pyrepr.Repr(providerName))), nil
}

// ResolveClientVoiceConfig is the STT and TTS client direct wiring of the active profile, relay on any failure
func ResolveClientVoiceConfig(ctx context.Context) *pyjson.Object {
	out := pyjson.NewObject()
	if !clientDirectEnabled(ctx) {
		out.Set("stt", relayVoiceConfig("voice.client_direct disabled"))
		out.Set("tts", relayVoiceConfig("voice.client_direct disabled"))
	} else {
		for _, step := range []struct {
			key     string
			resolve func(context.Context) (*pyjson.Object, error)
		}{{"stt", resolveSTTClientConfig}, {"tts", resolveTTSClientConfig}} {
			var result *pyjson.Object
			err := callQuietlyErr(func() error {
				var err error
				result, err = step.resolve(ctx)
				return err
			})
			if err != nil {
				voiceLog().Error(fmt.Sprintf("client voice-config %s resolution failed: %s", strings.ToUpper(step.key), err))
				result = relayVoiceConfig("resolution error")
			}
			out.Set(step.key, result)
		}
	}
	var available bool
	err := callQuietlyErr(func() error {
		var err error
		available, err = StreamingAvailable(ctx, nil)
		return err
	})
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("live STT capability probe failed: %s", err))
		return out
	}
	if stt, ok := out.Get("stt"); ok {
		stt.(*pyjson.Object).Set("streaming", available)
	}
	return out
}
