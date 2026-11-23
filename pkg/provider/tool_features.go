package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ToolFeatureState is one tool feature's availability and the backend serving it as status surfaces show it
type ToolFeatureState struct {
	Key                string
	Label              string
	Available          bool
	Active             bool
	ToolsetEnabled     bool
	CurrentProvider    string
	ExplicitConfigured bool
}

// ToolFeatures is the per feature state of the configured tool backends
type ToolFeatures struct {
	Features map[string]ToolFeatureState
}

// Feature is the state of one feature key
func (f ToolFeatures) Feature(key string) ToolFeatureState { return f.Features[key] }

// Display label of each tool feature
var toolFeatureLabels = map[string]string{"web": "Web tools", "image_gen": "Image generation", "video_gen": "Video generation",
	"tts": "Text-to-speech", "stt": "Speech-to-text", "browser": "Browser automation", "modal": "Modal execution"}

// Toolset the default cli platform enables
var toolDefaultPlatform = map[string]string{"cli": "icebath-cli"}

// Local stack probes pkg/browser and pkg/media install at init, each accessor panics when its owner never linked
var (
	ChromiumExecutable      func(env *tools.Env, allowOverride bool) string
	FasterWhisperImportable func() bool
)

// Seams over the env, toolset, local stack and credential readers, replaced in tests
var (
	toolFeatureEnvValueFn       = toolFeatureEnvValue
	toolFeatureToolsetEnabledFn = toolFeatureToolsetEnabled
	toolFeatureChromiumFn       = func(env PoolEnv) string { return chromiumExecutable(toolFeatureEnv(env), true) }
	toolFeatureFasterWhisperFn  = fasterWhisperImportable
	toolFeatureAudioKeyFn       = func(env PoolEnv) string { return tools.ResolveOpenAIAudioAPIKey(toolFeatureEnv(env)) }
	toolFeatureDirectModalFn    = func(env PoolEnv) bool { return tools.HasDirectModalCredentials(toolFeatureEnv(env)) }
	toolFeatureFalKeyFn         = func(env PoolEnv) bool { return tools.FalKeyIsConfigured(toolFeatureEnv(env)) }
)

// Feature state carrying its label
func toolState(key string, s ToolFeatureState) ToolFeatureState {
	s.Key, s.Label = key, toolFeatureLabels[key]
	return s
}

// Web search and extract state with per capability overrides deciding the active backend
func webToolFeature(env PoolEnv, toolEnabled bool) ToolFeatureState {
	ctx := env.loginCtx()
	backend, search, extract := toolNorm(settings.WebSearch.Get(ctx), ""), toolNorm(settings.WebSearchBackend.Get(ctx), ""), toolNorm(settings.WebExtractBackend.Get(ctx), "")
	keylessTavily := backend == "tavily" || search == "tavily" || extract == "tavily"
	direct := map[string]bool{
		"exa":        toolAnyEnv(env, "EXA_API_KEY"),
		"firecrawl":  toolAnyEnv(env, "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL"),
		"parallel":   toolAnyEnv(env, "PARALLEL_API_KEY"),
		"tavily":     toolAnyEnv(env, "TAVILY_API_KEY") || keylessTavily,
		"perplexity": toolAnyEnv(env, "PERPLEXITY_API_KEY"),
		"searxng":    toolAnyEnv(env, "SEARXNG_URL"),
	}
	active := direct[backend] || direct[search] || ((extract == "tavily" || extract == "perplexity") && direct[extract])
	anyDirect := false
	for _, ok := range direct {
		anyDirect = anyDirect || ok
	}
	current := backend
	if current == "" {
		current = search
	}
	if current == "" {
		current = extract
	}
	return toolState("web", ToolFeatureState{Available: anyDirect, Active: toolEnabled && active, ToolsetEnabled: toolEnabled,
		CurrentProvider: current, ExplicitConfigured: backend != "" || search != "" || extract != ""})
}

// Image or video state on the FAL key
func falToolFeature(key string, toolEnabled, direct, hasSelected bool) ToolFeatureState {
	label := ""
	if hasSelected || direct {
		label = "FAL"
	}
	return toolState(key, ToolFeatureState{Available: direct, Active: toolEnabled && direct, ToolsetEnabled: toolEnabled,
		CurrentProvider: label, ExplicitConfigured: hasSelected || direct})
}

// Text to speech and speech to text states, STT reported toolset enabled since no tool gates it
func audioToolFeatures(env PoolEnv, ttsToolEnabled bool) (ToolFeatureState, ToolFeatureState) {
	ctx := env.loginCtx()
	ttsCurrent, sttCurrent := toolNorm(settings.SpeechVoice.Get(ctx), "edge"), toolNorm(settings.RecognitionService.Get(ctx), "local")
	audioKey := toolFeatureAudioKeyFn(env) != ""
	ttsAvailable := false
	switch ttsCurrent {
	case "edge", "neutts":
		ttsAvailable = true
	case "openai":
		ttsAvailable = audioKey
	case "elevenlabs":
		ttsAvailable = toolAnyEnv(env, "ELEVENLABS_API_KEY")
	case "mistral":
		ttsAvailable = toolAnyEnv(env, "MISTRAL_API_KEY")
	}
	ttsSet := toolSettingSet(ctx, settings.SpeechVoice.Key)
	tts := toolState("tts", ToolFeatureState{Available: ttsAvailable, Active: ttsToolEnabled && ttsAvailable, ToolsetEnabled: ttsToolEnabled,
		CurrentProvider: toolProviderLabel("tts", ttsCurrent), ExplicitConfigured: ttsSet && ttsCurrent != "edge"})
	sttAvailable := false
	switch sttCurrent {
	case "local":
		sttAvailable = localSTTBackendAvailable(env)
	case "openai":
		sttAvailable = audioKey
	case "groq":
		sttAvailable = toolAnyEnv(env, "GROQ_API_KEY")
	case "mistral":
		sttAvailable = toolAnyEnv(env, "MISTRAL_API_KEY")
	}
	sttSet := toolSettingSet(ctx, settings.RecognitionService.Key)
	stt := toolState("stt", ToolFeatureState{Available: sttAvailable, Active: sttAvailable, ToolsetEnabled: true,
		CurrentProvider: toolProviderLabel("stt", sttCurrent), ExplicitConfigured: sttSet})
	return tts, stt
}

// Browser state resolved with the same precedence the runtime uses
func browserToolFeature(env PoolEnv, toolEnabled bool) ToolFeatureState {
	ctx := env.loginCtx()
	cloud, err := settings.Resolve(ctx, "browser.cloud_provider")
	explicit := err == nil && cloud.Source != settings.SourceDefault
	provider := tools.NormalizeBrowserCloudProvider("")
	if explicit {
		provider = tools.NormalizeBrowserCloudProvider(pyOrStr(cloud.Value))
	}
	directBrowserbase := toolFeatureEnvValueFn(env, "BROWSERBASE_API_KEY") != "" && toolFeatureEnvValueFn(env, "BROWSERBASE_PROJECT_ID") != ""
	directBrowserUse := toolAnyEnv(env, "BROWSER_USE_API_KEY")
	directFirecrawl := toolAnyEnv(env, "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL")
	localRunnable := localBrowserRunnable(env)
	var current string
	var available bool
	switch {
	case explicit:
		cloud := map[string]bool{"browserbase": directBrowserbase, "browser-use": directBrowserUse, "firecrawl": directFirecrawl}
		current = "local"
		if _, ok := cloud[provider]; ok {
			current = provider
		}
		available = localRunnable
		if v, ok := cloud[current]; ok {
			available = v
		}
	case directBrowserUse:
		current, available = "browser-use", true
	case directBrowserbase:
		current, available = "browserbase", true
	default:
		current, available = "local", localRunnable
	}
	return toolState("browser", ToolFeatureState{Available: available, Active: toolEnabled && available, ToolsetEnabled: toolEnabled,
		CurrentProvider: toolProviderLabel("browser", current), ExplicitConfigured: explicit})
}

// Modal terminal state, a non modal backend counting as available and a modal one on its direct credentials
func modalToolFeature(env PoolEnv, toolEnabled bool) ToolFeatureState {
	backend := toolNorm(settings.WhereCommandsRun.Get(env.loginCtx()), "local")
	isModal := backend == "modal"
	available, active := true, toolEnabled
	if isModal && !toolFeatureDirectModalFn(env) {
		available, active = false, false
	}
	current := "Modal"
	if !isModal {
		current = backend
	}
	return toolState("modal", ToolFeatureState{Available: available, Active: active, ToolsetEnabled: toolEnabled,
		CurrentProvider: current, ExplicitConfigured: isModal})
}

// GetToolFeatures resolves every tool feature's state from the config and the configured credentials
func GetToolFeatures(env PoolEnv) ToolFeatures {
	ctx := env.loginCtx()
	enabled := map[string]bool{}
	for _, key := range []string{"web", "image_gen", "video_gen", "tts", "browser", "terminal"} {
		enabled[key] = toolFeatureToolsetEnabledFn(env, key)
	}
	falConfigured := toolFeatureFalKeyFn(env)
	fal := func(key string) ToolFeatureState {
		return falToolFeature(key, enabled[key], falConfigured, toolSettingSet(ctx, key+".provider"))
	}
	tts, stt := audioToolFeatures(env, enabled["tts"])
	return ToolFeatures{Features: map[string]ToolFeatureState{
		"web":       webToolFeature(env, enabled["web"]),
		"image_gen": fal("image_gen"),
		"video_gen": fal("video_gen"),
		"tts":       tts,
		"stt":       stt,
		"browser":   browserToolFeature(env, enabled["browser"]),
		"modal":     modalToolFeature(env, enabled["terminal"]),
	}}
}

func chromiumExecutable(env *tools.Env, allowOverride bool) string {
	if ChromiumExecutable == nil {
		panic("provider.ChromiumExecutable is not installed: pkg/browser assigns chromiumExecutable at init")
	}
	return ChromiumExecutable(env, allowOverride)
}
func fasterWhisperImportable() bool {
	if FasterWhisperImportable == nil {
		panic("provider.FasterWhisperImportable is not installed: pkg/media assigns the faster_whisper probe at init")
	}
	return FasterWhisperImportable()
}

// Tool env over the profile home, config and secrets for the tool backend readers
func toolFeatureEnv(env PoolEnv) *tools.Env {
	return &tools.Env{Home: envHome(env), Config: config.Tree(env.config()), Secrets: env.Secrets, Log: env.log()}
}

// Scope aware environment value falling back to the profile .env file
func toolFeatureEnvValue(env PoolEnv, name string) string {
	if v, ok := env.scope().GetSecret(name); ok {
		return v
	}
	return env.envFile()[name]
}

// Whether the profile sets a setting itself rather than leaving it at its default
func toolSettingSet(ctx context.Context, key string) bool {
	v, err := settings.Resolve(ctx, key)
	return err == nil && v.Source != settings.SourceDefault
}

// Lowercased stripped text of value or the default when the value is falsy
func toolNorm(value any, def string) string {
	s := def
	if pyTruthy(value) {
		s = pyStr(value)
	}
	return strings.ToLower(pyStripSpace(s))
}

// Some platform's configured toolsets cover every tool of the toolset
func toolFeatureToolsetEnabled(env PoolEnv, toolsetKey string) bool {
	platforms := settings.ToolGroups.Get(env.loginCtx())
	if len(platforms) == 0 {
		platforms = map[string]any{"cli": []any{toolDefaultPlatform["cli"]}}
	}
	target := tools.Default.ResolveToolset(env.ctx(), "", toolsetKey)
	if len(target) == 0 {
		return false
	}
	for platform, raw := range platforms {
		names, _ := tools.ParsePlatformToolsetsValue(raw)
		if len(names) == 0 && toolDefaultPlatform[platform] != "" {
			names = []string{toolDefaultPlatform[platform]}
		}
		available := map[string]bool{}
		for _, name := range names {
			if name == "" {
				continue
			}
			for _, tool := range tools.Default.ResolveToolset(env.ctx(), "", name) {
				available[tool] = true
			}
		}
		covered := true
		for _, tool := range target {
			if !available[tool] {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

// Local browser starts when lightpanda or a runnable Chromium backs it
func localBrowserRunnable(env PoolEnv) bool {
	if toolNorm(settings.BrowserEngine.Get(env.loginCtx()), "auto") == "lightpanda" {
		return true
	}
	return toolFeatureChromiumFn(env) != ""
}

type toolProviderLabelSet struct {
	def    string
	labels map[string]string
}

var toolProviderLabels = map[string]toolProviderLabelSet{
	"browser": {"local", map[string]string{"browserbase": "Browserbase", "browser-use": "Browser Use", "firecrawl": "Firecrawl", "local": "Local browser"}},
	"tts": {"edge", map[string]string{"openai": "OpenAI TTS", "elevenlabs": "ElevenLabs", "edge": "Edge TTS", "xai": "xAI TTS",
		"mistral": "Mistral Voxtral TTS", "neutts": "NeuTTS"}},
	"stt": {"local", map[string]string{"openai": "OpenAI Whisper", "groq": "Groq Whisper", "mistral": "Mistral Voxtral Transcribe",
		"local": "Local faster-whisper"}},
}

// Display label of a provider, the raw name when unknown and the default label when empty
func toolProviderLabel(kind, current string) string {
	set := toolProviderLabels[kind]
	key := current
	if key == "" {
		key = set.def
	}
	if label, ok := set.labels[key]; ok {
		return label
	}
	if current != "" {
		return current
	}
	return set.labels[set.def]
}

// Faster whisper imports or a custom local STT command is configured
func localSTTBackendAvailable(env PoolEnv) bool {
	if toolFeatureEnvValueFn(env, "ICEBATH_LOCAL_STT_COMMAND") != "" {
		return true
	}
	return toolFeatureFasterWhisperFn()
}
func toolAnyEnv(env PoolEnv, names ...string) bool {
	for _, name := range names {
		if toolFeatureEnvValueFn(env, name) != "" {
			return true
		}
	}
	return false
}
