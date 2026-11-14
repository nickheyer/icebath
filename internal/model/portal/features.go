package portal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	ManagedSelection         = "portal"
	ManagedImageDefaultModel = "krea-3"
	defaultCLIToolset        = "icebath-cli"
	EnvLocalSTTCommand       = "ICEBATH_LOCAL_STT_COMMAND"

	ImageRouteFAL    = "fal"
	ImageRouteKrea   = "krea"
	ImageRoutePortal = "portal"
)

type ToolFacts interface {
	LoadConfig(ctx context.Context) (map[string]any, error)
	SaveConfig(ctx context.Context, cfg map[string]any) error
	ToolsetTools(name string) ([]string, error)
	PlatformToolsets(raw any) []string
	BrowserHarness() bool
	ChromiumExecutable() bool
	LocalSTT() bool
	FalKeyConfigured() bool
	DirectModalCredentials() bool
	NormalizeBrowserCloudProvider(value any) string
	NormalizeModalMode(value any) string
	ModalSelectedBackend(mode string, hasDirect, managedReady, managedEnabled bool) string
	OpenAIAudioKey() string
	ManagedImageRoute(provider, model any) string
	Checklist(title string, labels []string, preselected []int) ([]int, error)
}

type featureSpec struct {
	key               string
	label             string
	includedByDefault bool
	coverage          string
	gateway           string
	section           string
	field             string
	offerLabel        string
	directLabel       string
	defaultDirectEnv  []string
}

var featureSpecs = []featureSpec{
	{"web", "Web tools", true, "firecrawl", "firecrawl", "web", "backend", "Web search & extract",
		"Firecrawl/Exa/Parallel/Tavily/Perplexity/Keenable key or SearXNG",
		[]string{"PARALLEL_API_KEY", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL"}},
	{"image_gen", "Image generation", true, "fal", "fal-queue", "image_gen", "provider", "Image generation", "FAL key", nil},
	{"video_gen", "Video generation", false, "fal-video", "fal-queue", "video_gen", "provider", "Video generation (FAL)", "FAL key", nil},
	{"tts", "OpenAI TTS", true, "openai-audio", "openai-audio", "tts", "provider", "Text-to-speech (OpenAI TTS)", "OpenAI/ElevenLabs key",
		[]string{"ELEVENLABS_API_KEY"}},
	{"stt", "Speech-to-text", true, "openai-audio", "openai-audio", "stt", "provider", "Speech-to-text (OpenAI Whisper)", "OpenAI/Groq/Mistral key",
		[]string{"GROQ_API_KEY", "MISTRAL_API_KEY"}},
	{"browser", "Browser automation", true, "browser-use", "browser-use", "browser", "cloud_provider", "Browser automation (Browser Use)",
		"Browser Use/Browserbase key", []string{"BROWSER_USE_API_KEY", "BROWSERBASE_API_KEY"}},
	{"modal", "Modal execution", false, "modal", "modal", "", "", "", "", nil},
}

var defaultSections = []string{"web", "tts", "stt", "browser"}

func featureSpecOf(key string) featureSpec {
	for _, spec := range featureSpecs {
		if spec.key == key {
			return spec
		}
	}
	return featureSpec{}
}

func ManagedFeatureCoverage() map[string]string {
	out := map[string]string{}
	for _, spec := range featureSpecs {
		out[spec.key] = spec.coverage
	}
	return out
}

func gatewayKeys() []string {
	keys := []string{}
	for _, spec := range featureSpecs {
		if spec.section != "" {
			keys = append(keys, spec.key)
		}
	}
	return keys
}

func truthyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return truthy(normalize(v))
}

func configSection(cfg map[string]any, key string) map[string]any {
	section, _ := cfg[key].(map[string]any)
	return section
}

func ensureSection(cfg map[string]any, key string) map[string]any {
	section, ok := cfg[key].(map[string]any)
	if !ok {
		section = map[string]any{}
		cfg[key] = section
	}
	return section
}

func usesGateway(section any) bool {
	m, ok := section.(map[string]any)
	return ok && truthyValue(m["use_gateway"])
}

func rawText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func norm(v any, def string) string {
	if !truthy(normalize(v)) {
		return strings.ToLower(strings.TrimSpace(def))
	}
	return strings.ToLower(strings.TrimSpace(rawText(v)))
}

func selectedProvider(section any, field string) (string, bool) {
	m, ok := section.(map[string]any)
	if !ok {
		return "", false
	}
	if usesGateway(m) {
		return ManagedSelection, true
	}
	value, present := m[field]
	if !present || value == nil {
		return "", false
	}
	text := strings.ToLower(strings.TrimSpace(rawText(value)))
	return text, text != ""
}

func selectManaged(cfg map[string]any, key, model string) {
	spec := featureSpecOf(key)
	section := ensureSection(cfg, spec.section)
	section[spec.field] = ManagedSelection
	delete(section, "use_gateway")
	if model != "" {
		section["model"] = model
	}
}

type FeatureState struct {
	Key                string
	Label              string
	IncludedByDefault  bool
	Available          bool
	Active             bool
	ManagedByPortal    bool
	DirectOverride     bool
	ToolsetEnabled     bool
	CurrentProvider    string
	ExplicitConfigured bool
}

type SubscriptionFeatures struct {
	Subscribed        bool
	PortalAuthPresent bool
	ProviderIsPortal  bool
	Features          map[string]FeatureState
	AccountInfo       *AccountInfo
}

func (f SubscriptionFeatures) Items() []FeatureState {
	out := make([]FeatureState, 0, len(featureSpecs))
	for _, spec := range featureSpecs {
		out = append(out, f.Features[spec.key])
	}
	return out
}

func featureState(key string, s FeatureState) FeatureState {
	spec := featureSpecOf(key)
	s.Key, s.Label, s.IncludedByDefault = key, spec.label, spec.includedByDefault
	return s
}

func withDirectOverride(s FeatureState) FeatureState {
	s.DirectOverride = s.Active && !s.ManagedByPortal
	return s
}

func providerIsPortal(cfg map[string]any) bool {
	return norm(configSection(cfg, "model")["provider"], "") == Provider
}

func (a *Account) envValue(ctx context.Context, name string) string {
	value, _, err := a.cfg.Env().Get(a.bind(ctx), name)
	if err != nil {
		return ""
	}
	return value
}

func (a *Account) anyEnv(ctx context.Context, names ...string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return a.envValue(ctx, name) != "" })
}

func (a *Account) toolsetEnabled(cfg map[string]any, toolsetKey string) (bool, error) {
	platforms, _ := cfg["platform_toolsets"].(map[string]any)
	if len(platforms) == 0 {
		platforms = map[string]any{"cli": []any{defaultCLIToolset}}
	}
	target, err := a.tools.ToolsetTools(toolsetKey)
	if err != nil {
		return false, err
	}
	if len(target) == 0 {
		return false, nil
	}
	names := make([]string, 0, len(platforms))
	for platform := range platforms {
		names = append(names, platform)
	}
	sort.Strings(names)
	for _, platform := range names {
		toolsets := a.tools.PlatformToolsets(platforms[platform])
		if len(toolsets) == 0 && platform == "cli" {
			toolsets = []string{defaultCLIToolset}
		}
		available := map[string]bool{}
		for _, name := range toolsets {
			if name == "" {
				continue
			}
			tools, err := a.tools.ToolsetTools(name)
			if err != nil {
				continue
			}
			for _, tool := range tools {
				available[tool] = true
			}
		}
		if !slices.ContainsFunc(target, func(tool string) bool { return !available[tool] }) {
			return true, nil
		}
	}
	return false, nil
}

func (a *Account) localBrowserRunnable(browserCfg map[string]any) bool {
	if !a.tools.BrowserHarness() {
		return false
	}
	if norm(browserCfg["engine"], "auto") == "lightpanda" {
		return true
	}
	return a.tools.ChromiumExecutable()
}

func providerLabel(kind, current string) string {
	def, labels := "", map[string]string{}
	switch kind {
	case "browser":
		def, labels = "local", map[string]string{"browserbase": "Browserbase", "browser-use": "Browser Use", "firecrawl": "Firecrawl", "local": "Local browser"}
	case "tts":
		def, labels = "edge", map[string]string{"openai": "OpenAI TTS", "elevenlabs": "ElevenLabs", "edge": "Edge TTS", "xai": "xAI TTS",
			"mistral": "Mistral Voxtral TTS", "neutts": "NeuTTS"}
	case "stt":
		def, labels = "local", map[string]string{"openai": "OpenAI Whisper", "groq": "Groq Whisper", "mistral": "Mistral Voxtral Transcribe",
			"local": "Local faster-whisper"}
	}
	if current == "" {
		return labels[def]
	}
	if label, ok := labels[current]; ok {
		return label
	}
	return current
}

func (a *Account) localSTTAvailable(ctx context.Context) bool {
	return a.envValue(ctx, EnvLocalSTTCommand) != "" || a.tools.LocalSTT()
}

func (a *Account) webFeature(ctx context.Context, webCfg map[string]any, toolEnabled, managed, webGateway, directFirecrawl bool) FeatureState {
	backend, search, extract := norm(webCfg["backend"], ""), norm(webCfg["search_backend"], ""), norm(webCfg["extract_backend"], "")
	if backend == ManagedSelection || webGateway {
		backend = "firecrawl"
	}
	chosen := []string{backend, search, extract}
	direct := map[string]bool{
		"exa":        a.anyEnv(ctx, "EXA_API_KEY") && !webGateway,
		"firecrawl":  directFirecrawl,
		"parallel":   a.anyEnv(ctx, "PARALLEL_API_KEY") && !webGateway,
		"tavily":     (a.anyEnv(ctx, "TAVILY_API_KEY") || slices.Contains(chosen, "tavily")) && !webGateway,
		"perplexity": a.anyEnv(ctx, "PERPLEXITY_API_KEY") && !webGateway,
		"searxng":    a.anyEnv(ctx, "SEARXNG_URL"),
	}
	webManaged := managed && ((backend == "firecrawl" && !directFirecrawl) || slices.Contains(chosen[1:], ManagedSelection))
	active := webManaged || direct[backend] || direct[search] || ((extract == "tavily" || extract == "perplexity") && direct[extract])
	anyDirect := false
	for _, ready := range direct {
		anyDirect = anyDirect || ready
	}
	return withDirectOverride(featureState("web", FeatureState{
		Available: managed || anyDirect, Active: toolEnabled && active, ManagedByPortal: webManaged, ToolsetEnabled: toolEnabled,
		CurrentProvider: firstText(backend, search, extract), ExplicitConfigured: backend != "" || search != "" || extract != "",
	}))
}

func (a *Account) ManagedImagePartner(cfg map[string]any) string {
	section := configSection(cfg, "image_gen")
	switch a.tools.ManagedImageRoute(section["provider"], section["model"]) {
	case ImageRouteFAL:
		return "FAL"
	case ImageRouteKrea:
		return "Krea"
	case ImageRoutePortal:
		return portalDisplayName
	}
	return ""
}

func falFeature(key string, toolEnabled, direct, managed bool, selected string, isSelected bool) FeatureState {
	falManaged := toolEnabled && managed && !direct
	label := ""
	switch {
	case (isSelected && selected != ManagedSelection) || (!isSelected && direct):
		label = "FAL"
	case falManaged || (isSelected && selected == ManagedSelection):
		label = "Icebath Portal Subscription"
	}
	return withDirectOverride(featureState(key, FeatureState{
		Available: managed || direct, Active: toolEnabled && (falManaged || direct), ManagedByPortal: falManaged, ToolsetEnabled: toolEnabled,
		CurrentProvider: label, ExplicitConfigured: isSelected || direct,
	}))
}

func audioProvider(cfg map[string]any, def string, gateway bool) string {
	provider := norm(cfg["provider"], def)
	if provider == ManagedSelection || gateway {
		return "openai"
	}
	return firstText(provider, def)
}

type selection struct {
	value string
	set   bool
}

func (a *Account) audioFeatures(ctx context.Context, ttsCfg, sttCfg map[string]any, ttsEnabled bool, managed map[string]bool, selected map[string]selection, gateway map[string]bool) (FeatureState, FeatureState) {
	ttsGateway, sttGateway := gateway["tts"], gateway["stt"]
	ttsCurrent, sttCurrent := audioProvider(ttsCfg, "edge", ttsGateway), audioProvider(sttCfg, "local", sttGateway)
	audioKey := a.tools.OpenAIAudioKey() != ""
	directTTS, directSTT := audioKey && !ttsGateway, audioKey && !sttGateway
	ttsAvailable := map[string]bool{
		"edge": true, "neutts": true, "openai": managed["tts"] || directTTS,
		"elevenlabs": a.anyEnv(ctx, "ELEVENLABS_API_KEY") && !ttsGateway, "mistral": a.anyEnv(ctx, "MISTRAL_API_KEY"),
	}[ttsCurrent]
	tts := withDirectOverride(featureState("tts", FeatureState{
		Available: ttsAvailable, Active: ttsEnabled && ttsAvailable,
		ManagedByPortal: ttsEnabled && ttsCurrent == "openai" && managed["tts"] && !directTTS,
		ToolsetEnabled:  ttsEnabled, CurrentProvider: providerLabel("tts", ttsCurrent),
		ExplicitConfigured: selected["tts"].set && selected["tts"].value != "edge",
	}))
	sttAvailable := map[string]bool{
		"local": a.localSTTAvailable(ctx) && !sttGateway, "openai": managed["stt"] || directSTT,
		"groq": a.anyEnv(ctx, "GROQ_API_KEY") && !sttGateway, "mistral": a.anyEnv(ctx, "MISTRAL_API_KEY") && !sttGateway,
	}[sttCurrent]
	stt := withDirectOverride(featureState("stt", FeatureState{
		Available: sttAvailable, Active: sttAvailable, ToolsetEnabled: true,
		ManagedByPortal: sttCurrent == "openai" && managed["stt"] && !directSTT,
		CurrentProvider: providerLabel("stt", sttCurrent), ExplicitConfigured: selected["stt"].set,
	}))
	return tts, stt
}

func (a *Account) browserFeature(ctx context.Context, browserCfg map[string]any, toolEnabled, managed, browserGateway, directFirecrawl bool) FeatureState {
	_, explicit := browserCfg["cloud_provider"]
	var rawProvider any
	if explicit {
		rawProvider = browserCfg["cloud_provider"]
	}
	provider := a.tools.NormalizeBrowserCloudProvider(rawProvider)
	if provider == ManagedSelection || browserGateway {
		provider = "browser-use"
	}
	directBrowserbase := a.envValue(ctx, "BROWSERBASE_API_KEY") != "" && a.envValue(ctx, "BROWSERBASE_PROJECT_ID") != "" && !browserGateway
	directBrowserUse := a.anyEnv(ctx, "BROWSER_USE_API_KEY") && !browserGateway
	localAvailable, localRunnable := a.tools.BrowserHarness(), a.localBrowserRunnable(browserCfg)
	browserUseManaged := toolEnabled && localAvailable && managed && !directBrowserUse
	current, available, managedNow := "local", localRunnable, false
	switch {
	case explicit:
		cloud := map[string]bool{
			"browserbase": localAvailable && directBrowserbase,
			"browser-use": localAvailable && (managed || directBrowserUse), "firecrawl": localAvailable && directFirecrawl,
		}
		if ready, ok := cloud[provider]; ok {
			current, available = provider, ready
		}
		managedNow = current == "browser-use" && browserUseManaged
	case managed || directBrowserUse:
		current, available, managedNow = "browser-use", localAvailable, browserUseManaged
	case directBrowserbase:
		current, available = "browserbase", localAvailable
	}
	return withDirectOverride(featureState("browser", FeatureState{
		Available: available, Active: toolEnabled && available, ManagedByPortal: managedNow, ToolsetEnabled: toolEnabled,
		CurrentProvider: providerLabel("browser", current), ExplicitConfigured: explicit,
	}))
}

func (a *Account) modalFeature(terminalCfg map[string]any, toolEnabled, managed, managedToolsFlag bool) FeatureState {
	backend := norm(terminalCfg["backend"], "local")
	mode := a.tools.NormalizeModalMode(terminalCfg["modal_mode"])
	directModal := a.tools.DirectModalCredentials()
	isModal := backend == "modal"
	selected := ""
	if isModal {
		selected = a.tools.ModalSelectedBackend(mode, directModal, managed, managedToolsFlag)
	}
	state := FeatureState{ToolsetEnabled: toolEnabled, CurrentProvider: firstText(backend, "local"), ExplicitConfigured: isModal}
	if isModal {
		state.CurrentProvider = "Modal"
	}
	if !isModal || selected == "managed" || selected == "direct" {
		state.Available, state.Active = true, toolEnabled
		state.ManagedByPortal = selected == "managed" && toolEnabled
		state.DirectOverride = isModal && selected == "direct" && toolEnabled
	} else {
		switch mode {
		case "managed":
			state.Available = managed
		case "direct":
			state.Available = directModal
		default:
			state.Available = managed || directModal
		}
	}
	return featureState("modal", state)
}

func (a *Account) loadedConfig(ctx context.Context, cfg map[string]any) (map[string]any, error) {
	if cfg != nil {
		return cfg, nil
	}
	loaded, err := a.tools.LoadConfig(a.bind(ctx))
	if err != nil {
		return nil, err
	}
	if loaded == nil {
		return map[string]any{}, nil
	}
	return loaded, nil
}

func (a *Account) SubscriptionFeatures(ctx context.Context, cfg map[string]any, forceFresh bool) (SubscriptionFeatures, error) {
	ctx = a.bind(ctx)
	cfg, err := a.loadedConfig(ctx, cfg)
	if err != nil {
		return SubscriptionFeatures{}, err
	}
	isPortal := providerIsPortal(cfg)
	info := a.AccountInfo(ctx, AccountOptions{ForceFresh: forceFresh})
	authPresent := info.LoggedIn
	managedToolsFlag := authPresent && info.ToolGatewayEntitled()
	enabled := map[string]bool{}
	for _, key := range []string{"web", "image_gen", "video_gen", "tts", "browser", "terminal"} {
		if enabled[key], err = a.toolsetEnabled(cfg, key); err != nil {
			return SubscriptionFeatures{}, err
		}
	}
	selected, gateway := map[string]selection{}, map[string]bool{}
	for _, key := range gatewayKeys() {
		spec := featureSpecOf(key)
		value, set := selectedProvider(cfg[spec.section], spec.field)
		selected[key] = selection{value: value, set: set}
		gateway[key] = set && value == ManagedSelection
	}
	managed := map[string]bool{}
	for _, spec := range featureSpecs {
		ready := false
		if managedToolsFlag {
			if ready, err = a.ManagedToolGatewayReady(ctx, spec.gateway); err != nil {
				return SubscriptionFeatures{}, err
			}
			ready = ready && info.ToolGatewayEntitledFor(spec.coverage) && (!selected[spec.key].set || gateway[spec.key])
		}
		managed[spec.key] = ready
	}
	directFirecrawl := a.anyEnv(ctx, "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL") && !gateway["web"]
	falConfigured := a.tools.FalKeyConfigured()
	tts, stt := a.audioFeatures(ctx, configSection(cfg, "tts"), configSection(cfg, "stt"), enabled["tts"], managed, selected, gateway)
	fal := func(key string) FeatureState {
		return falFeature(key, enabled[key], falConfigured && !gateway[key], managed[key], selected[key].value, selected[key].set)
	}
	features := map[string]FeatureState{
		"web":       a.webFeature(ctx, configSection(cfg, "web"), enabled["web"], managed["web"], gateway["web"], directFirecrawl),
		"image_gen": fal("image_gen"),
		"video_gen": fal("video_gen"),
		"tts":       tts,
		"stt":       stt,
		"browser":   a.browserFeature(ctx, configSection(cfg, "browser"), enabled["browser"], managed["browser"], gateway["browser"], directFirecrawl),
		"modal":     a.modalFeature(configSection(cfg, "terminal"), enabled["terminal"], managed["modal"], managedToolsFlag),
	}
	return SubscriptionFeatures{Subscribed: isPortal || authPresent, PortalAuthPresent: authPresent, ProviderIsPortal: isPortal, Features: features, AccountInfo: info}, nil
}

func (a *Account) hasManagedDefaultDirect(ctx context.Context, key string) bool {
	return ((key == "tts" || key == "stt") && a.tools.OpenAIAudioKey() != "") || a.anyEnv(ctx, featureSpecOf(key).defaultDirectEnv...)
}

func kreaImageDefault(cfg map[string]any, info *AccountInfo) string {
	if truthy(normalize(configSection(cfg, "image_gen")["model"])) || info == nil || !info.ToolGatewayEntitledFor("krea") {
		return ""
	}
	return ManagedImageDefaultModel
}

func (a *Account) ApplyManagedDefaults(ctx context.Context, cfg map[string]any, enabledToolsets []string, forceFresh bool) ([]string, error) {
	ctx = a.bind(ctx)
	features, err := a.SubscriptionFeatures(ctx, cfg, forceFresh)
	if err != nil {
		return nil, err
	}
	info := features.AccountInfo
	if info == nil || !info.LoggedIn || !info.ToolGatewayEntitled() || !features.ProviderIsPortal {
		return []string{}, nil
	}
	changed := []string{}
	for _, key := range defaultSections {
		ensureSection(cfg, key)
	}
	for _, key := range defaultSections {
		if features.Features[key].ExplicitConfigured || a.hasManagedDefaultDirect(ctx, key) {
			continue
		}
		if key == "stt" {
			if a.localSTTAvailable(ctx) || !info.ToolGatewayEntitledFor("openai-audio") {
				continue
			}
		} else if !slices.Contains(enabledToolsets, key) {
			continue
		}
		selectManaged(cfg, key, "")
		changed = append(changed, key)
	}
	for _, pair := range [][2]string{{"image_gen", ""}, {"video_gen", "fal-video"}} {
		key, category := pair[0], pair[1]
		if slices.Contains(enabledToolsets, key) && !a.tools.FalKeyConfigured() && (category == "" || info.ToolGatewayEntitledFor(category)) {
			model := ""
			if key == "image_gen" && !features.Features[key].ExplicitConfigured {
				model = kreaImageDefault(cfg, info)
			}
			selectManaged(cfg, key, model)
			changed = append(changed, key)
		}
	}
	return changed, nil
}

func (a *Account) gatewayDirectCredentials(ctx context.Context) map[string]bool {
	falDirect, audioDirect := a.tools.FalKeyConfigured(), a.tools.OpenAIAudioKey() != ""
	return map[string]bool{
		"web":       a.anyEnv(ctx, "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "PARALLEL_API_KEY", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "EXA_API_KEY", "SEARXNG_URL"),
		"image_gen": falDirect, "video_gen": falDirect,
		"tts": audioDirect || a.anyEnv(ctx, "ELEVENLABS_API_KEY"),
		"stt": audioDirect || a.anyEnv(ctx, "GROQ_API_KEY", "MISTRAL_API_KEY"),
		"browser": a.anyEnv(ctx, "BROWSER_USE_API_KEY", "CAMOFOX_URL") ||
			(a.envValue(ctx, "BROWSERBASE_API_KEY") != "" && a.envValue(ctx, "BROWSERBASE_PROJECT_ID") != ""),
	}
}

type GatewayEligibility struct {
	Unconfigured       []string
	HasDirect          []string
	ExplicitConfigured []string
	AlreadyManaged     []string
}

func (a *Account) GatewayEligibleTools(ctx context.Context, cfg map[string]any, forceFresh bool) (GatewayEligibility, error) {
	ctx = a.bind(ctx)
	out := GatewayEligibility{Unconfigured: []string{}, HasDirect: []string{}, ExplicitConfigured: []string{}, AlreadyManaged: []string{}}
	info := a.AccountInfo(ctx, AccountOptions{ForceFresh: forceFresh})
	if !info.LoggedIn || !info.ToolGatewayEntitled() {
		return out, nil
	}
	cfg, err := a.loadedConfig(ctx, cfg)
	if err != nil {
		return out, err
	}
	if !providerIsPortal(cfg) {
		return out, nil
	}
	direct := a.gatewayDirectCredentials(ctx)
	for _, key := range gatewayKeys() {
		spec := featureSpecOf(key)
		if !info.ToolGatewayEntitledFor(spec.coverage) {
			continue
		}
		selected, set := selectedProvider(cfg[spec.section], spec.field)
		switch {
		case usesGateway(cfg[key]):
			out.AlreadyManaged = append(out.AlreadyManaged, key)
		case set && selected != ManagedSelection:
			out.ExplicitConfigured = append(out.ExplicitConfigured, key)
		case direct[key]:
			out.HasDirect = append(out.HasDirect, key)
		default:
			out.Unconfigured = append(out.Unconfigured, key)
		}
	}
	return out, nil
}

func ApplyGatewayDefaults(cfg map[string]any, toolKeys []string, info *AccountInfo) []string {
	for _, key := range defaultSections {
		ensureSection(cfg, key)
	}
	changed := []string{}
	for _, key := range gatewayKeys() {
		if !slices.Contains(toolKeys, key) {
			continue
		}
		model := ""
		if key == "image_gen" {
			model = kreaImageDefault(cfg, info)
		}
		selectManaged(cfg, key, model)
		changed = append(changed, key)
	}
	return changed
}

func (a *Account) PromptEnableToolGateway(ctx context.Context, c auth.Console, cfg map[string]any, forceFresh bool) ([]string, error) {
	ctx = a.bind(ctx)
	eligible, err := a.GatewayEligibleTools(ctx, cfg, forceFresh)
	if err != nil {
		return nil, err
	}
	if len(eligible.Unconfigured) == 0 && len(eligible.HasDirect) == 0 {
		return []string{}, nil
	}
	info := a.AccountInfo(ctx, AccountOptions{})
	poolOnly := !isTrue(info.PaidServiceAccess) && info.ToolAccess != nil && info.ToolAccess.Enabled
	sourceLabel, title := "Icebath Portal subscription", "Your Icebath Portal subscription includes the Tool Gateway — pick the tools to enable:"
	if poolOnly {
		sourceLabel, title = "free tool pool", "Your free Icebath Portal tool pool — pick the tools to enable:"
	}
	declined := []string{}
	if raw, ok := cfg["tool_gateway_declined_tools"].([]any); ok {
		for _, item := range raw {
			declined = append(declined, rawText(item))
		}
	}
	offerKeys := append(slices.Clone(eligible.Unconfigured), eligible.HasDirect...)
	labels, preSelected := []string{}, []int{}
	for i, key := range eligible.Unconfigured {
		labels = append(labels, featureSpecOf(key).offerLabel)
		if !slices.Contains(declined, key) {
			preSelected = append(preSelected, i)
		}
	}
	for _, key := range eligible.HasDirect {
		spec := featureSpecOf(key)
		labels = append(labels, fmt.Sprintf("%s — keep using your %s", spec.offerLabel, spec.directLabel))
	}
	chosenIndex, err := a.tools.Checklist(title, labels, preSelected)
	if err != nil {
		return []string{}, nil
	}
	chosen := []string{}
	for _, i := range chosenIndex {
		if i >= 0 && i < len(offerKeys) {
			chosen = append(chosen, offerKeys[i])
		}
	}
	newlyDeclined := []string{}
	for _, key := range eligible.Unconfigured {
		if !slices.Contains(chosen, key) && !slices.Contains(declined, key) {
			newlyDeclined = append(newlyDeclined, key)
		}
	}
	if len(newlyDeclined) > 0 || slices.ContainsFunc(declined, func(key string) bool { return slices.Contains(chosen, key) }) {
		remaining := []any{}
		merged := append(slices.Clone(declined), newlyDeclined...)
		sort.Strings(merged)
		for _, key := range slices.Compact(merged) {
			if !slices.Contains(chosen, key) {
				remaining = append(remaining, key)
			}
		}
		cfg["tool_gateway_declined_tools"] = remaining
	}
	changed := []string{}
	if len(chosen) > 0 {
		changed = ApplyGatewayDefaults(cfg, chosen, info)
	}
	if len(changed) > 0 || len(newlyDeclined) > 0 {
		if err := a.tools.SaveConfig(ctx, cfg); err != nil {
			return nil, err
		}
		shown := slices.Clone(changed)
		sort.Strings(shown)
		for _, key := range shown {
			say(c, fmt.Sprintf("  ✓ %s: enabled via %s", featureSpecOf(key).offerLabel, sourceLabel))
		}
	}
	return changed, nil
}

func confirm(c auth.Console, prompt string) (yes, answered bool) {
	if c.Out != nil {
		fmt.Fprint(c.Out, prompt)
	}
	if c.In == nil {
		return false, false
	}
	line, err := bufio.NewReader(c.In).ReadString('\n')
	if err != nil && line == "" {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, true
	}
	return false, true
}

func (a *Account) EnsurePortalAccess(ctx context.Context, c auth.Console, capability, coverageCategory string) bool {
	ctx = a.bind(ctx)
	if capability == "" {
		capability = "the Icebath Portal Tool Gateway"
	}
	entitled := func(info *AccountInfo) bool {
		if coverageCategory != "" {
			return info.ToolGatewayEntitledFor(coverageCategory)
		}
		return info.ToolGatewayEntitled()
	}
	info := a.AccountInfo(ctx, AccountOptions{ForceFresh: true})
	if !entitled(info) && !info.LoggedIn {
		if !a.loginOnly(ctx, c, capability) {
			return false
		}
		info = a.AccountInfo(ctx, AccountOptions{ForceFresh: true})
	}
	if entitled(info) {
		return true
	}
	message := EntitlementMessage(info, EntitlementOptions{Capability: capability, CoverageCategory: coverageCategory})
	if message != "" {
		for _, line := range strings.Split(message, "\n") {
			say(c, "  "+line)
		}
	}
	return false
}

func (a *Account) loginOnly(ctx context.Context, c auth.Console, capability string) bool {
	say(c, "")
	say(c, fmt.Sprintf("  %s requires an Icebath Portal login.", capability))
	proceed, answered := confirm(c, "  Log in to Icebath Portal now? [Y/n]: ")
	if !answered {
		say(c, "")
		return false
	}
	if !proceed {
		say(c, "  Skipped Icebath Portal login.")
		return false
	}
	err := func() error {
		priorActive, err := a.store.Active(ctx)
		if err != nil {
			return err
		}
		var state *object
		if shared := a.readShared(); shared != nil && !isGuestState(shared) {
			if importIt, answered := confirm(c, "  Found existing Icebath Portal OAuth credentials. Import them? [Y/n]: "); importIt || !answered {
				state = a.tryImportShared(ctx, defaultTimeout)
			}
		}
		if state == nil {
			if state, err = a.deviceCodeLogin(ctx, c, DeviceLoginOptions{}); err != nil {
				return err
			}
		}
		err = a.store.Transact(ctx, Provider, auth.LockTimeout, func(tx *auth.Tx) error {
			return tx.SaveWithActive(state.raw(), priorActive)
		})
		if err != nil {
			return err
		}
		a.writeShared(ctx, state, false)
		handoff, err := a.Drain(ctx)
		if err != nil {
			return err
		}
		a.syncPool(ctx)
		a.settleAfterUpgrade(ctx, state)
		say(c, "  Icebath Portal login successful.")
		for _, view := range handoff {
			for _, line := range a.ResultLines(view) {
				say(c, "  "+line)
			}
		}
		return nil
	}()
	if err == nil {
		return true
	}
	var exit *ExitError
	switch {
	case auth.IsCancelled(err):
		say(c, "\n  Login cancelled.")
	case errors.As(err, &exit):
	default:
		say(c, fmt.Sprintf("  Icebath Portal login failed: %v", err))
	}
	return false
}
