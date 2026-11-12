package provider

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/home"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ManagedImageDefaultKreaModel is the image model written for accounts entitled to Krea
const ManagedImageDefaultKreaModel = "krea-3"

type nousFeatureSpec struct {
	label             string
	includedByDefault bool
	coverage          string
	gateway           string
	sectionKey        string
	sectionField      string
	offerLabel        string
	directLabel       string
	defaultDirectEnv  []string
}

var (
	nousFeatureOrder = []string{"web", "image_gen", "video_gen", "tts", "stt", "browser", "modal"}
	nousFeatures     = map[string]nousFeatureSpec{
		"web": {"Web tools", true, "firecrawl", "firecrawl", "web", "backend", "Web search & extract",
			"Firecrawl/Exa/Parallel/Tavily/Perplexity/Keenable key or SearXNG",
			[]string{"PARALLEL_API_KEY", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL"}},
		"image_gen": {"Image generation", true, "fal", "fal-queue", "image_gen", "provider", "Image generation", "FAL key", nil},
		"video_gen": {"Video generation", false, "fal-video", "fal-queue", "video_gen", "provider", "Video generation (FAL)", "FAL key", nil},
		"tts": {"OpenAI TTS", true, "openai-audio", "openai-audio", "tts", "provider", "Text-to-speech (OpenAI TTS)",
			"OpenAI/ElevenLabs key", []string{"ELEVENLABS_API_KEY"}},
		"stt": {"Speech-to-text", true, "openai-audio", "openai-audio", "stt", "provider", "Speech-to-text (OpenAI Whisper)",
			"OpenAI/Groq/Mistral key", []string{"GROQ_API_KEY", "MISTRAL_API_KEY"}},
		"browser": {"Browser automation", true, "browser-use", "browser-use", "browser", "cloud_provider",
			"Browser automation (Browser Use)", "Browser Use/Browserbase key", []string{"BROWSER_USE_API_KEY", "BROWSERBASE_API_KEY"}},
		"modal": {label: "Modal execution", coverage: "modal", gateway: "modal"},
	}
	nousGatewayKeys      = []string{"web", "image_gen", "video_gen", "tts", "stt", "browser"}
	nousDefaultSections  = []string{"web", "tts", "stt", "browser"}
	nousGatewayToolLabel = nousGatewayToolLabels()
	nousDefaultPlatform  = map[string]string{"cli": "icebath-cli"}
)

// ManagedFeatureCoverageCategory maps each managed feature to the tool pool coverage category gating it
var ManagedFeatureCoverageCategory = nousFeatureCoverage()

func nousFeatureCoverage() map[string]string {
	out := map[string]string{}
	for key, spec := range nousFeatures {
		out[key] = spec.coverage
	}
	return out
}

func nousGatewayToolLabels() map[string]string {
	out := map[string]string{}
	for _, key := range nousGatewayKeys {
		out[key] = nousFeatures[key].offerLabel
	}
	return out
}

// NousFeatureState is one managed feature's availability, activity and selection as status surfaces show it
type NousFeatureState struct {
	Key                string
	Label              string
	IncludedByDefault  bool
	Available          bool
	Active             bool
	ManagedByNous      bool
	DirectOverride     bool
	ToolsetEnabled     bool
	CurrentProvider    string
	ExplicitConfigured bool
}

// NousSubscriptionFeatures is the per feature state of the managed tool subscription
type NousSubscriptionFeatures struct {
	Subscribed      bool
	NousAuthPresent bool
	ProviderIsNous  bool
	Features        map[string]NousFeatureState
	AccountInfo     *NousPortalAccountInfo
}

// Feature is the state of one feature key
func (f NousSubscriptionFeatures) Feature(key string) NousFeatureState { return f.Features[key] }

// Items lists every feature state in table order
func (f NousSubscriptionFeatures) Items() []NousFeatureState {
	out := make([]NousFeatureState, 0, len(nousFeatureOrder))
	for _, key := range nousFeatureOrder {
		out = append(out, f.Features[key])
	}
	return out
}

// Local stack probes pkg/browser and pkg/media install at init, each accessor panics when its owner never linked
var (
	BrowserHarnessCLI       func(env *tools.Env) []string
	ChromiumExecutable      func(env *tools.Env, allowOverride bool) string
	FasterWhisperImportable func() bool
	ManagedImageRoute       func(provider, modelID any) string
)

func browserHarnessCLI(env *tools.Env) []string {
	if BrowserHarnessCLI == nil {
		panic("provider.BrowserHarnessCLI is not installed: pkg/browser assigns findCLI at init")
	}
	return BrowserHarnessCLI(env)
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

func managedImageRoute(provider, modelID any) string {
	if ManagedImageRoute == nil {
		panic("provider.ManagedImageRoute is not installed: pkg/media assigns managedRoute at init")
	}
	return ManagedImageRoute(provider, modelID)
}

var (
	nousSubEnvValueFn    = nousSubEnvValue
	nousSubAccountInfoFn = func(env PoolEnv, forceFresh bool) *NousPortalAccountInfo {
		return getNousPortalAccountInfoIn(env, forceFresh, 60)
	}
	nousSubToolsetEnabledFn = nousSubToolsetEnabled
	nousSubHarnessFn        = func(env PoolEnv) bool { return browserHarnessCLI(nousSubToolsEnv(env)) != nil }
	nousSubChromiumFn       = func(env PoolEnv) string { return chromiumExecutable(nousSubToolsEnv(env), true) }
	nousSubFasterWhisperFn  = fasterWhisperImportable
	nousSubAudioKeyFn       = func(env PoolEnv) string { return tools.ResolveOpenAIAudioAPIKey(nousSubToolsEnv(env)) }
	nousSubDirectModalFn    = func(env PoolEnv) bool { return tools.HasDirectModalCredentials(nousSubToolsEnv(env)) }
	nousSubFalKeyFn         = func(env PoolEnv) bool { return tools.FalKeyIsConfigured(nousSubToolsEnv(env)) }
	nousSubGatewayReadyFn   = func(env PoolEnv, vendor string) bool {
		return tools.IsManagedToolGatewayReady(nousSubToolsEnv(env), vendor, nil, nil)
	}
	nousSubDirectCredsFn = nousGatewayDirectCredentials
	nousSubChecklistFn   = promptChecklist
	nousSubSaveConfigFn  = func(env PoolEnv, cfg map[string]any) error { return config.Save(envHome(env), config.Tree(cfg)) }
)

// SwapNousSubAccountInfo replaces the account lookup the subscription features read, returning the previous lookup
func SwapNousSubAccountInfo(fn func(PoolEnv, bool) *NousPortalAccountInfo) func(PoolEnv, bool) *NousPortalAccountInfo {
	prev := nousSubAccountInfoFn
	nousSubAccountInfoFn = fn
	return prev
}

// Tool env over the profile home, config and secrets for the tool backend readers
func nousSubToolsEnv(env PoolEnv) *tools.Env {
	return &tools.Env{Home: envHome(env), Config: config.Tree(env.config()), Secrets: env.Secrets, Log: env.log()}
}

// Scope aware environment value falling back to the profile .env file
func nousSubEnvValue(env PoolEnv, name string) string {
	if v, ok := env.scope().GetSecret(name); ok {
		return v
	}
	return env.envFile()[name]
}

// Profile config when the caller passes none, an empty mapping when nothing loads
func nousSubLoadConfig(env PoolEnv) map[string]any {
	cfg := env.config()
	if cfg == nil {
		cfg = ConfigLoader(envHome(env))()
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg
}

// Section explicitly opting into the gateway through the legacy use_gateway flag
func nousUsesGateway(section any) bool {
	m, ok := section.(map[string]any)
	return ok && isTruthyValue(m["use_gateway"])
}

// Stored provider of a section, nous for the managed pick, a vendor for BYOK, false when unselected
func nousSelectedProvider(section any, nameKey string) (string, bool) {
	m, ok := section.(map[string]any)
	if !ok {
		return "", false
	}
	if nousUsesGateway(m) {
		return "nous", true
	}
	value, present := m[nameKey]
	if !present || value == nil {
		return "", false
	}
	s := strings.ToLower(pyStripSpace(pyStr(value)))
	return s, s != ""
}

func nousSection(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Section as a mapping, replacing a missing or non mapping value in the config
func nousEnsureSection(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	cfg[key] = m
	return m
}

// Stores the managed nous selection for a feature, with an image model when given
func nousSelectNous(cfg map[string]any, key, model string) {
	spec := nousFeatures[key]
	section := nousEnsureSection(cfg, spec.sectionKey)
	section[spec.sectionField] = "nous"
	delete(section, "use_gateway")
	if model != "" {
		section["model"] = model
	}
}

// Lowercased stripped text of value or the default when the value is falsy
func nousNorm(value any, def string) string {
	s := def
	if pyTruthy(value) {
		s = pyStr(value)
	}
	return strings.ToLower(pyStripSpace(s))
}

func nousProviderIsNous(cfg map[string]any) bool {
	return nousNorm(nousSection(cfg, "model")["provider"], "") == "nous"
}

// Some platform's configured toolsets cover every tool of the toolset
func nousSubToolsetEnabled(env PoolEnv, cfg map[string]any, toolsetKey string) bool {
	platforms, _ := cfg["platform_toolsets"].(map[string]any)
	if len(platforms) == 0 {
		platforms = map[string]any{"cli": []any{nousDefaultPlatform["cli"]}}
	}
	target := tools.Default.ResolveToolset(env.ctx(), "", toolsetKey)
	if len(target) == 0 {
		return false
	}
	for platform, raw := range platforms {
		names, _ := tools.ParsePlatformToolsetsValue(raw)
		if len(names) == 0 && nousDefaultPlatform[platform] != "" {
			names = []string{nousDefaultPlatform[platform]}
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

// Local browser starts when the harness exists and lightpanda or a runnable Chromium backs it
func nousLocalBrowserRunnable(env PoolEnv, browserCfg map[string]any) bool {
	if !nousSubHarnessFn(env) {
		return false
	}
	if nousNorm(browserCfg["engine"], "auto") == "lightpanda" {
		return true
	}
	return nousSubChromiumFn(env) != ""
}

type nousProviderLabelSet struct {
	def    string
	labels map[string]string
}

var nousProviderLabels = map[string]nousProviderLabelSet{
	"browser": {"local", map[string]string{"browserbase": "Browserbase", "browser-use": "Browser Use", "firecrawl": "Firecrawl", "local": "Local browser"}},
	"tts": {"edge", map[string]string{"openai": "OpenAI TTS", "elevenlabs": "ElevenLabs", "edge": "Edge TTS", "xai": "xAI TTS",
		"mistral": "Mistral Voxtral TTS", "neutts": "NeuTTS"}},
	"stt": {"local", map[string]string{"openai": "OpenAI Whisper", "groq": "Groq Whisper", "mistral": "Mistral Voxtral Transcribe",
		"local": "Local faster-whisper"}},
}

// Display label of a provider, the raw name when unknown and the default label when empty
func nousProviderLabel(kind, current string) string {
	set := nousProviderLabels[kind]
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
func nousLocalSTTBackendAvailable(env PoolEnv) bool {
	if nousSubEnvValueFn(env, "ICEBATH_LOCAL_STT_COMMAND") != "" {
		return true
	}
	return nousSubFasterWhisperFn()
}

func nousAnyEnv(env PoolEnv, names ...string) bool {
	for _, name := range names {
		if nousSubEnvValueFn(env, name) != "" {
			return true
		}
	}
	return false
}

// Feature state with the table label, direct override defaulting to active without the managed route
func nousState(key string, s NousFeatureState, directOverride *bool) NousFeatureState {
	spec := nousFeatures[key]
	s.Key, s.Label, s.IncludedByDefault = key, spec.label, spec.includedByDefault
	if directOverride != nil {
		s.DirectOverride = *directOverride
	} else {
		s.DirectOverride = s.Active && !s.ManagedByNous
	}
	return s
}

// Web search and extract state with per capability overrides deciding the active backend
func nousWebFeature(env PoolEnv, webCfg map[string]any, toolEnabled, managed, webGW, directFirecrawl bool) NousFeatureState {
	backend, search, extract := nousNorm(webCfg["backend"], ""), nousNorm(webCfg["search_backend"], ""), nousNorm(webCfg["extract_backend"], "")
	if backend == "nous" || webGW {
		backend = "firecrawl"
	}
	keylessTavily := backend == "tavily" || search == "tavily" || extract == "tavily"
	direct := map[string]bool{
		"exa":        nousAnyEnv(env, "EXA_API_KEY") && !webGW,
		"firecrawl":  directFirecrawl,
		"parallel":   nousAnyEnv(env, "PARALLEL_API_KEY") && !webGW,
		"tavily":     (nousAnyEnv(env, "TAVILY_API_KEY") || keylessTavily) && !webGW,
		"perplexity": nousAnyEnv(env, "PERPLEXITY_API_KEY") && !webGW,
		"searxng":    nousAnyEnv(env, "SEARXNG_URL"),
	}
	webManaged := managed && ((backend == "firecrawl" && !directFirecrawl) || search == "nous" || extract == "nous")
	active := webManaged || direct[backend] || direct[search] || ((extract == "tavily" || extract == "perplexity") && direct[extract])
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
	return nousState("web", NousFeatureState{Available: managed || anyDirect, Active: toolEnabled && active, ManagedByNous: webManaged,
		ToolsetEnabled: toolEnabled, CurrentProvider: current, ExplicitConfigured: backend != "" || search != "" || extract != ""}, nil)
}

// ManagedImagePartner names the gateway partner the image request is dispatched to, empty when a direct vendor owns it
func ManagedImagePartner(cfg map[string]any) string {
	section := nousSection(cfg, "image_gen")
	return map[string]string{"fal": "FAL", "krea": "Krea", "portal": "Nous Portal"}[managedImageRoute(section["provider"], section["model"])]
}

// Image or video state sharing the FAL key with independently gated managed availability
func nousFalFeature(key string, toolEnabled, direct, managed bool, selected string, hasSelected bool) NousFeatureState {
	falManaged := toolEnabled && managed && !direct
	label := ""
	switch {
	case (hasSelected && selected != "nous") || (!hasSelected && direct):
		label = "FAL"
	case falManaged || (hasSelected && selected == "nous"):
		label = "Nous Subscription"
	}
	return nousState(key, NousFeatureState{Available: managed || direct, Active: toolEnabled && (falManaged || direct),
		ManagedByNous: falManaged, ToolsetEnabled: toolEnabled, CurrentProvider: label, ExplicitConfigured: hasSelected || direct}, nil)
}

func nousAudioProvider(cfg map[string]any, def string, gw bool) string {
	provider := nousNorm(cfg["provider"], def)
	if provider == "nous" || gw {
		return "openai"
	}
	if provider == "" {
		return def
	}
	return provider
}

type nousSelection struct {
	value string
	set   bool
}

// Text to speech and speech to text states, STT reported toolset enabled since no tool gates it
func nousAudioFeatures(env PoolEnv, ttsCfg, sttCfg map[string]any, ttsToolEnabled bool, managed map[string]bool, selected map[string]nousSelection, useGateway map[string]bool) (NousFeatureState, NousFeatureState) {
	ttsGW, sttGW := useGateway["tts"], useGateway["stt"]
	ttsCurrent, sttCurrent := nousAudioProvider(ttsCfg, "edge", ttsGW), nousAudioProvider(sttCfg, "local", sttGW)
	audioKey := nousSubAudioKeyFn(env) != ""
	directTTS, directSTT := audioKey && !ttsGW, audioKey && !sttGW
	ttsAvailable := false
	switch ttsCurrent {
	case "edge", "neutts":
		ttsAvailable = true
	case "openai":
		ttsAvailable = managed["tts"] || directTTS
	case "elevenlabs":
		ttsAvailable = nousAnyEnv(env, "ELEVENLABS_API_KEY") && !ttsGW
	case "mistral":
		ttsAvailable = nousAnyEnv(env, "MISTRAL_API_KEY")
	}
	tts := nousState("tts", NousFeatureState{Available: ttsAvailable, Active: ttsToolEnabled && ttsAvailable,
		ManagedByNous: ttsToolEnabled && ttsCurrent == "openai" && managed["tts"] && !directTTS, ToolsetEnabled: ttsToolEnabled,
		CurrentProvider: nousProviderLabel("tts", ttsCurrent), ExplicitConfigured: selected["tts"].set && selected["tts"].value != "edge"}, nil)
	sttAvailable := false
	switch sttCurrent {
	case "local":
		sttAvailable = nousLocalSTTBackendAvailable(env) && !sttGW
	case "openai":
		sttAvailable = managed["stt"] || directSTT
	case "groq":
		sttAvailable = nousAnyEnv(env, "GROQ_API_KEY") && !sttGW
	case "mistral":
		sttAvailable = nousAnyEnv(env, "MISTRAL_API_KEY") && !sttGW
	}
	stt := nousState("stt", NousFeatureState{Available: sttAvailable, Active: sttAvailable, ToolsetEnabled: true,
		ManagedByNous: sttCurrent == "openai" && managed["stt"] && !directSTT, CurrentProvider: nousProviderLabel("stt", sttCurrent),
		ExplicitConfigured: selected["stt"].set}, nil)
	return tts, stt
}

// Browser state resolved with the same precedence the runtime uses
func nousBrowserFeature(env PoolEnv, browserCfg map[string]any, toolEnabled, managed, browserGW, directFirecrawl bool) NousFeatureState {
	rawProvider, explicit := browserCfg["cloud_provider"]
	provider := tools.NormalizeBrowserCloudProvider("")
	if explicit {
		provider = tools.NormalizeBrowserCloudProvider(pyOrStr(rawProvider))
	}
	if provider == "nous" || browserGW {
		provider = "browser-use"
	}
	directBrowserbase := nousSubEnvValueFn(env, "BROWSERBASE_API_KEY") != "" && nousSubEnvValueFn(env, "BROWSERBASE_PROJECT_ID") != "" && !browserGW
	directBrowserUse := nousAnyEnv(env, "BROWSER_USE_API_KEY") && !browserGW
	localAvailable := nousSubHarnessFn(env)
	localRunnable := nousLocalBrowserRunnable(env, browserCfg)
	browserUseManaged := toolEnabled && localAvailable && managed && !directBrowserUse
	var current string
	var available, managedNow bool
	switch {
	case explicit:
		cloud := map[string]bool{"browserbase": localAvailable && directBrowserbase, "browser-use": localAvailable && (managed || directBrowserUse),
			"firecrawl": localAvailable && directFirecrawl}
		current = "local"
		if _, ok := cloud[provider]; ok {
			current = provider
		}
		available = localRunnable
		if v, ok := cloud[current]; ok {
			available = v
		}
		managedNow = current == "browser-use" && browserUseManaged
	case managed || directBrowserUse:
		current, available, managedNow = "browser-use", localAvailable, browserUseManaged
	case directBrowserbase:
		current, available = "browserbase", localAvailable
	default:
		current, available = "local", localRunnable
	}
	return nousState("browser", NousFeatureState{Available: available, Active: toolEnabled && available, ManagedByNous: managedNow,
		ToolsetEnabled: toolEnabled, CurrentProvider: nousProviderLabel("browser", current), ExplicitConfigured: explicit}, nil)
}

// Modal terminal state, a non modal backend or a resolved selection always counting as available
func nousModalFeature(env PoolEnv, terminalCfg map[string]any, toolEnabled, managed, managedToolsFlag bool) NousFeatureState {
	backend := nousNorm(terminalCfg["backend"], "local")
	mode := tools.NormalizeModalMode(pyOrStr(terminalCfg["modal_mode"]))
	directModal := nousSubDirectModalFn(env)
	flag := managedToolsFlag
	state := tools.ResolveModalBackendState(nousSubToolsEnv(env), mode, directModal, managed, &flag)
	isModal := backend == "modal"
	selected := ""
	if isModal {
		selected = state.SelectedBackend
	}
	var available, active, managedNow, directOverride bool
	if !isModal || selected == "managed" || selected == "direct" {
		available, active = true, toolEnabled
		managedNow = selected == "managed" && toolEnabled
		directOverride = isModal && selected == "direct" && toolEnabled
	} else {
		switch mode {
		case "managed":
			available = managed
		case "direct":
			available = directModal
		default:
			available = managed || directModal
		}
	}
	current := "Modal"
	if !isModal {
		current = backend
		if current == "" {
			current = "local"
		}
	}
	return nousState("modal", NousFeatureState{Available: available, Active: active, ManagedByNous: managedNow, ToolsetEnabled: toolEnabled,
		CurrentProvider: current, ExplicitConfigured: isModal}, &directOverride)
}

// GetNousSubscriptionFeatures resolves every managed feature's state from the config and the account entitlement
func GetNousSubscriptionFeatures(env PoolEnv, cfg map[string]any, forceFresh bool) NousSubscriptionFeatures {
	if cfg == nil {
		cfg = nousSubLoadConfig(env)
	}
	providerIsNous := nousProviderIsNous(cfg)
	info := nousSubAccountInfoFn(env, forceFresh)
	authPresent := info != nil && info.LoggedIn
	managedToolsFlag := authPresent && info.ToolGatewayEntitled()
	enabled := map[string]bool{}
	for _, key := range []string{"web", "image_gen", "video_gen", "tts", "browser", "terminal"} {
		enabled[key] = nousSubToolsetEnabledFn(env, cfg, key)
	}
	selected, useGateway := map[string]nousSelection{}, map[string]bool{}
	for _, key := range nousGatewayKeys {
		spec := nousFeatures[key]
		value, set := nousSelectedProvider(nousSection(cfg, spec.sectionKey), spec.sectionField)
		selected[key] = nousSelection{value, set}
		useGateway[key] = set && value == "nous"
	}
	managed := map[string]bool{}
	for _, key := range nousFeatureOrder {
		spec := nousFeatures[key]
		managed[key] = managedToolsFlag && nousSubGatewayReadyFn(env, spec.gateway) && info.ToolGatewayEntitledFor(spec.coverage) &&
			(!selected[key].set || useGateway[key])
	}
	directFirecrawl := nousAnyEnv(env, "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL") && !useGateway["web"]
	falConfigured := nousSubFalKeyFn(env)
	tts, stt := nousAudioFeatures(env, nousSection(cfg, "tts"), nousSection(cfg, "stt"), enabled["tts"], managed, selected, useGateway)
	fal := func(key string) NousFeatureState {
		return nousFalFeature(key, enabled[key], falConfigured && !useGateway[key], managed[key], selected[key].value, selected[key].set)
	}
	features := map[string]NousFeatureState{
		"web":       nousWebFeature(env, nousSection(cfg, "web"), enabled["web"], managed["web"], useGateway["web"], directFirecrawl),
		"image_gen": fal("image_gen"),
		"video_gen": fal("video_gen"),
		"tts":       tts,
		"stt":       stt,
		"browser":   nousBrowserFeature(env, nousSection(cfg, "browser"), enabled["browser"], managed["browser"], useGateway["browser"], directFirecrawl),
		"modal":     nousModalFeature(env, nousSection(cfg, "terminal"), enabled["terminal"], managed["modal"], managedToolsFlag),
	}
	return NousSubscriptionFeatures{Subscribed: providerIsNous || authPresent, NousAuthPresent: authPresent,
		ProviderIsNous: providerIsNous, Features: features, AccountInfo: info}
}

// Direct credentials that keep a category off the managed default, the audio key counting for TTS and STT
func nousHasManagedDefaultDirect(env PoolEnv, key string) bool {
	if (key == "tts" || key == "stt") && nousSubAudioKeyFn(env) != "" {
		return true
	}
	return nousAnyEnv(env, nousFeatures[key].defaultDirectEnv...)
}

// Krea 3 for Krea entitled accounts with no stored image model, empty otherwise
func nousKreaImageDefault(cfg map[string]any, info *NousPortalAccountInfo) string {
	if pyTruthy(nousSection(cfg, "image_gen")["model"]) || info == nil || !info.ToolGatewayEntitledFor("krea") {
		return ""
	}
	return ManagedImageDefaultKreaModel
}

// ApplyNousManagedDefaults switches unconfigured features of an entitled Nous user to the managed gateway
func ApplyNousManagedDefaults(env PoolEnv, cfg map[string]any, enabledToolsets []string, forceFresh bool) map[string]bool {
	features := GetNousSubscriptionFeatures(env, cfg, forceFresh)
	info := features.AccountInfo
	changed := map[string]bool{}
	if info == nil || !info.LoggedIn || !info.ToolGatewayEntitled() || !features.ProviderIsNous {
		return changed
	}
	selectedToolsets := map[string]bool{}
	for _, name := range enabledToolsets {
		selectedToolsets[name] = true
	}
	for _, key := range nousDefaultSections {
		nousEnsureSection(cfg, key)
	}
	for _, key := range nousDefaultSections {
		if features.Features[key].ExplicitConfigured || nousHasManagedDefaultDirect(env, key) {
			continue
		}
		if key == "stt" {
			if nousLocalSTTBackendAvailable(env) || !info.ToolGatewayEntitledFor("openai-audio") {
				continue
			}
		} else if !selectedToolsets[key] {
			continue
		}
		nousSelectNous(cfg, key, "")
		changed[key] = true
	}
	for _, item := range []struct{ key, category string }{{"image_gen", ""}, {"video_gen", "fal-video"}} {
		if !selectedToolsets[item.key] || nousSubFalKeyFn(env) || (item.category != "" && !info.ToolGatewayEntitledFor(item.category)) {
			continue
		}
		model := ""
		if item.key == "image_gen" && !features.Features[item.key].ExplicitConfigured {
			model = nousKreaImageDefault(cfg, info)
		}
		nousSelectNous(cfg, item.key, model)
		changed[item.key] = true
	}
	return changed
}

// Tool key to direct credential presence, env configured keyless local backends counting as configured
func nousGatewayDirectCredentials(env PoolEnv) map[string]bool {
	falDirect := nousSubFalKeyFn(env)
	audioDirect := nousSubAudioKeyFn(env) != ""
	return map[string]bool{
		"web": nousAnyEnv(env, "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "PARALLEL_API_KEY", "TAVILY_API_KEY", "PERPLEXITY_API_KEY",
			"EXA_API_KEY", "SEARXNG_URL"),
		"image_gen": falDirect,
		"video_gen": falDirect,
		"tts":       audioDirect || nousAnyEnv(env, "ELEVENLABS_API_KEY"),
		"stt":       audioDirect || nousAnyEnv(env, "GROQ_API_KEY", "MISTRAL_API_KEY"),
		"browser": nousAnyEnv(env, "BROWSER_USE_API_KEY", "CAMOFOX_URL") ||
			(nousSubEnvValueFn(env, "BROWSERBASE_API_KEY") != "" && nousSubEnvValueFn(env, "BROWSERBASE_PROJECT_ID") != ""),
	}
}

// GetGatewayEligibleTools splits the covered gateway tools into unconfigured, own key, explicit and already managed lists
func GetGatewayEligibleTools(env PoolEnv, cfg map[string]any, forceFresh bool) (unconfigured, hasDirect, explicitConfigured, alreadyManaged []string) {
	info := nousSubAccountInfoFn(env, forceFresh)
	if info == nil || !info.LoggedIn || !info.ToolGatewayEntitled() {
		return nil, nil, nil, nil
	}
	if cfg == nil {
		cfg = nousSubLoadConfig(env)
	}
	if !nousProviderIsNous(cfg) {
		return nil, nil, nil, nil
	}
	direct := nousSubDirectCredsFn(env)
	for _, key := range nousGatewayKeys {
		spec := nousFeatures[key]
		if !info.ToolGatewayEntitledFor(spec.coverage) {
			continue
		}
		selected, set := nousSelectedProvider(cfg[spec.sectionKey], spec.sectionField)
		switch {
		case nousUsesGateway(cfg[key]):
			alreadyManaged = append(alreadyManaged, key)
		case set && selected != "nous":
			explicitConfigured = append(explicitConfigured, key)
		case direct[key]:
			hasDirect = append(hasDirect, key)
		default:
			unconfigured = append(unconfigured, key)
		}
	}
	return unconfigured, hasDirect, explicitConfigured, alreadyManaged
}

// ApplyGatewayDefaults stores the managed selection for the chosen tools and reports which changed
func ApplyGatewayDefaults(cfg map[string]any, toolKeys []string, info *NousPortalAccountInfo) map[string]bool {
	for _, key := range nousDefaultSections {
		nousEnsureSection(cfg, key)
	}
	chosen := map[string]bool{}
	for _, key := range toolKeys {
		chosen[key] = true
	}
	changed := map[string]bool{}
	for _, key := range nousGatewayKeys {
		if !chosen[key] {
			continue
		}
		model := ""
		if key == "image_gen" {
			model = nousKreaImageDefault(cfg, info)
		}
		nousSelectNous(cfg, key, model)
		changed[key] = true
	}
	return changed
}

// PromptEnableToolGateway offers a per tool checklist routing eligible tools through the gateway and records declines
func PromptEnableToolGateway(env PoolEnv, out io.Writer, cfg map[string]any, forceFresh bool) (map[string]bool, error) {
	unconfigured, hasDirect, _, _ := GetGatewayEligibleTools(env, cfg, forceFresh)
	changed := map[string]bool{}
	if len(unconfigured) == 0 && len(hasDirect) == 0 {
		return changed, nil
	}
	info := nousSubAccountInfoFn(env, false)
	poolOnly := info != nil && (info.PaidServiceAccess == nil || !*info.PaidServiceAccess) && info.ToolAccess != nil && info.ToolAccess.Enabled
	sourceLabel, title := "Nous subscription", "Your Nous subscription includes the Tool Gateway — pick the tools to enable:"
	if poolOnly {
		sourceLabel, title = "free tool pool", "Your free Nous tool pool — pick the tools to enable:"
	}
	declined := map[string]bool{}
	if list, ok := cfg["tool_gateway_declined_tools"].([]any); ok {
		for _, item := range list {
			declined[pyStr(item)] = true
		}
	}
	offerKeys := append(append([]string{}, unconfigured...), hasDirect...)
	labels := make([]string, 0, len(offerKeys))
	var preSelected []int
	for i, key := range unconfigured {
		labels = append(labels, nousGatewayToolLabel[key])
		if !declined[key] {
			preSelected = append(preSelected, i)
		}
	}
	for _, key := range hasDirect {
		labels = append(labels, nousGatewayToolLabel[key]+" — keep using your "+nousFeatures[key].directLabel)
	}
	chosenIdx, err := nousSubChecklistFn(title, labels, preSelected)
	if err != nil {
		return changed, nil
	}
	var chosenKeys []string
	chosen := map[string]bool{}
	for _, i := range chosenIdx {
		if i >= 0 && i < len(offerKeys) {
			chosenKeys = append(chosenKeys, offerKeys[i])
			chosen[offerKeys[i]] = true
		}
	}
	var newlyDeclined []string
	for _, key := range unconfigured {
		if !chosen[key] && !declined[key] {
			newlyDeclined = append(newlyDeclined, key)
		}
	}
	reclaimed := false
	for key := range declined {
		reclaimed = reclaimed || chosen[key]
	}
	if len(newlyDeclined) > 0 || reclaimed {
		merged := map[string]bool{}
		for key := range declined {
			merged[key] = true
		}
		for _, key := range newlyDeclined {
			merged[key] = true
		}
		var kept []string
		for key := range merged {
			if !chosen[key] {
				kept = append(kept, key)
			}
		}
		sort.Strings(kept)
		stored := make([]any, 0, len(kept))
		for _, key := range kept {
			stored = append(stored, key)
		}
		cfg["tool_gateway_declined_tools"] = stored
	}
	if len(chosenKeys) > 0 {
		changed = ApplyGatewayDefaults(cfg, chosenKeys, info)
	}
	if len(changed) > 0 || len(newlyDeclined) > 0 {
		if err := nousSubSaveConfigFn(env, cfg); err != nil {
			return changed, err
		}
		keys := make([]string, 0, len(changed))
		for key := range changed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			label, ok := nousGatewayToolLabel[key]
			if !ok {
				label = key
			}
			fmt.Fprintf(out, "  ✓ %s: enabled via %s\n", label, sourceLabel)
		}
	}
	return changed, nil
}

// EnsureNousPortalAccess logs in when needed and reports whether the account is entitled for the capability
func EnsureNousPortalAccess(env PoolEnv, out io.Writer, capability, coverageCategory string) bool {
	if capability == "" {
		capability = "the Nous Tool Gateway"
	}
	entitled := func(info *NousPortalAccountInfo) bool {
		if info == nil {
			return false
		}
		if coverageCategory != "" {
			return info.ToolGatewayEntitledFor(coverageCategory)
		}
		return info.ToolGatewayEntitled()
	}
	info := nousSubAccountInfoFn(env, true)
	if !entitled(info) && (info == nil || !info.LoggedIn) {
		if !nousRunPortalLoginOnly(env, out, capability) {
			return false
		}
		info = nousSubAccountInfoFn(env, true)
	}
	if entitled(info) {
		return true
	}
	message := FormatNousPortalEntitlementMessageWith(info, NousEntitlementOptions{Capability: capability, CoverageCategory: coverageCategory})
	for _, line := range pySplitLines(message) {
		fmt.Fprintf(out, "  %s\n", line)
	}
	return false
}

// Yes on y, yes or blank, no on anything else, nil when input ends or is interrupted
func nousConfirm(prompt string) *bool {
	answer, err := oauthPromptFn(prompt)
	if err != nil {
		return nil
	}
	switch strings.ToLower(pyStripSpace(answer)) {
	case "", "y", "yes":
		yes := true
		return &yes
	}
	no := false
	return &no
}

// Portal device login persisting credentials only, leaving the active provider and model untouched
func nousRunPortalLoginOnly(env PoolEnv, out io.Writer, capability string) bool {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s requires a Nous Portal login.\n", capability)
	proceed := nousConfirm("  Log in to Nous Portal now? [Y/n]: ")
	if proceed == nil {
		fmt.Fprintln(out)
		return false
	}
	if !*proceed {
		fmt.Fprintln(out, "  Skipped Nous Portal login.")
		return false
	}
	handoff, err := nousLoginOnlyPersist(env, out)
	if err != nil {
		var exit *NousLoginExitError
		var cancelled *DeviceFlowCancelledError
		var signInCancelled *SignInCancelledError
		switch {
		case errors.As(err, &exit):
		case errors.Is(err, context.Canceled) || errors.As(err, &cancelled) || errors.As(err, &signInCancelled):
			fmt.Fprintln(out, "\n  Login cancelled.")
		default:
			fmt.Fprintf(out, "  Nous Portal login failed: %v\n", err)
		}
		return false
	}
	fmt.Fprintln(out, "  Nous Portal login successful.")
	for _, view := range handoff {
		for _, line := range guestHandoffResultLines(view) {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	return true
}

// Imports shared credentials or runs the device flow, saves them restoring the prior active provider, drains the handoff
func nousLoginOnlyPersist(env PoolEnv, out io.Writer) ([]map[string]any, error) {
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	data, err := env.Store.Load()
	unlock()
	if err != nil {
		return nil, err
	}
	prior := data["active_provider"]
	var authState map[string]any
	if shared, ok := readSharedNousState(env); ok && !isGuestState(shared) {
		if answer := nousConfirm("  Found existing Nous OAuth credentials. Import them? [Y/n]: "); answer == nil || *answer {
			if imported, ok := tryImportSharedNousState(env, 15*time.Second); ok {
				authState = imported
			}
		}
	}
	if authState == nil {
		authState, err = nousDeviceCodeLoginFn(env, out, NousDeviceLoginArgs{ClientID: DefaultNousClientID, OpenBrowser: true, Timeout: 15 * time.Second})
		if err != nil {
			return nil, err
		}
	}
	err = env.Store.Update(func(data AuthData) (bool, error) {
		if err := env.Store.StoreProviderState(data, "nous", authState, true); err != nil {
			return false, err
		}
		if truthyAny(prior) {
			data["active_provider"] = prior
		} else {
			delete(data, "active_provider")
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	writeSharedNousState(env, authState, false)
	handoff, err := nousGuestHandoffDrainFn(env)
	if err != nil {
		return nil, err
	}
	nousSyncPoolFromAuthStore(env)
	nousSettleAfterUpgradeFn(env, authState)
	return handoff, nil
}

// Installs the terminal setup check reading Nous auth from the subscription features of the config
func init() {
	tools.NousSubscriptionAuthPresent = func(env *tools.Env, cfg map[string]any) bool {
		return GetNousSubscriptionFeatures(toolsPoolEnv(env), cfg, false).NousAuthPresent
	}
}

// Credential environment of a tool env, its home, secrets and logger, the process home when no env is bound
func toolsPoolEnv(env *tools.Env) PoolEnv {
	if env == nil {
		return credentialLifecycleEnv(context.Background(), home.Process())
	}
	pe := credentialLifecycleEnv(context.Background(), env.Home)
	if env.Secrets != nil {
		pe.Secrets = env.Secrets
	}
	if env.Log != nil {
		pe.Log = env.Log
	}
	return pe
}
