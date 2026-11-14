package catalog

import (
	"context"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	unknownModelContext = 200000
	defaultOverrideKey  = "_default"
)

type ModelInfo struct {
	ID               string
	Name             string
	Family           string
	ProviderID       string
	Reasoning        bool
	ToolCall         bool
	Attachment       bool
	Temperature      bool
	StructuredOutput bool
	OpenWeights      bool
	InputModalities  []string
	OutputModalities []string
	ContextWindow    int
	MaxOutput        int
	MaxInput         int
	CostInput        float64
	CostOutput       float64
	CostCacheRead    *float64
	CostCacheWrite   *float64
	KnowledgeCutoff  string
	ReleaseDate      string
	Status           string
	Interleaved      bool
	InterleavedField string
}

func (m ModelInfo) HasCostData() bool { return m.CostInput > 0 || m.CostOutput > 0 }

func (m ModelInfo) SupportsVision() bool {
	return m.Attachment || slices.Contains(m.InputModalities, "image")
}

func (m ModelInfo) SupportsPDF() bool { return slices.Contains(m.InputModalities, "pdf") }

func (m ModelInfo) SupportsAudioInput() bool { return slices.Contains(m.InputModalities, "audio") }

func (m ModelInfo) FormatCapabilities() string {
	var labels []string
	for _, flag := range []struct {
		on    bool
		label string
	}{
		{m.Reasoning, "reasoning"}, {m.ToolCall, "tools"}, {m.SupportsVision(), "vision"}, {m.SupportsPDF(), "PDF"},
		{m.SupportsAudioInput(), "audio"}, {m.StructuredOutput, "structured output"}, {m.OpenWeights, "open weights"},
	} {
		if flag.on {
			labels = append(labels, flag.label)
		}
	}
	if len(labels) == 0 {
		return "basic"
	}
	return strings.Join(labels, ", ")
}

type ProviderInfo struct {
	ID         string
	Name       string
	Env        []string
	API        string
	Doc        string
	ModelCount int
}

type Capabilities struct {
	SupportsTools     bool
	SupportsVision    *bool
	SupportsReasoning *bool
	ContextWindow     int
	MaxOutputTokens   int
	ModelFamily       string
}

type providerPair struct {
	provider string
	mdev     string
}

var providerToModelsDev = []providerPair{
	{"openrouter", "openrouter"}, {"novita", "novita-ai"}, {"anthropic", "anthropic"},
	{"openai", "openai"}, {"openai-api", "openai"}, {"openai-codex", "openai"}, {"zai", "zai"},
	{"kimi", "kimi-for-coding"}, {"kimi-coding", "kimi-for-coding"},
	{"moonshot", "kimi-for-coding"}, {"stepfun", "stepfun"},
	{"kimi-coding-cn", "kimi-for-coding"}, {"minimax", "minimax"},
	{"minimax-oauth", "minimax"}, {"minimax-cn", "minimax-cn"}, {"deepseek", "deepseek"},
	{"alibaba", "alibaba"}, {"qwen-oauth", "alibaba"},
	{"ai-gateway", "vercel"}, {"opencode-zen", "opencode"},
	{"opencode-go", "opencode-go"},
	{"kilocode", "kilo"}, {"fireworks", "fireworks-ai"},
	{"huggingface", "huggingface"}, {"gemini", "google"}, {"google", "google"},
	{"xai", "xai"},
	{"xai-oauth", "xai"},
	{"xiaomi", "xiaomi"}, {"nvidia", "nvidia"},
	{"meta-ai", "meta"}, {"meta", "meta"}, {"groq", "groq"}, {"mistral", "mistral"},
	{"togetherai", "togetherai"}, {"perplexity", "perplexity"}, {"cohere", "cohere"},
	{"ollama-cloud", "ollama-cloud"},
}

var noisePatterns = regexp.MustCompile(`(?i)-tts\b|embedding|live-|-(preview|exp)-\d{2,4}[-_]|-image\b|-image-preview\b|-customtools\b`)

var googleHiddenModels = []string{
	"gemma-4-31b-it", "gemma-4-26b-it", "gemma-4-26b-a4b-it",
	"gemma-3-1b", "gemma-3-1b-it", "gemma-3-2b", "gemma-3-2b-it",
	"gemma-3-4b", "gemma-3-4b-it", "gemma-3-12b", "gemma-3-12b-it",
	"gemma-3-27b", "gemma-3-27b-it",
	"gemini-1.5-flash", "gemini-1.5-pro", "gemini-1.5-flash-8b",
	"gemini-2.0-flash", "gemini-2.0-flash-lite",
}

func unknownModelBase() *mdModel {
	return &mdModel{limits: map[string]float64{"context": unknownModelContext}, flags: map[string]bool{"tool_call": true}, cost: map[string]float64{}}
}

func builtinVisionModel(context, output float64, family string) *mdModel {
	return &mdModel{
		family: family, limits: map[string]float64{"context": context, "output": output},
		flags: map[string]bool{"tool_call": true, "reasoning": true}, cost: map[string]float64{},
		hasModalities: true, inputIsList: true, inputMods: []string{"text", "image"}, outputMods: []string{"text"},
	}
}

var builtinModelMetadata = map[[2]string]*mdModel{
	{"openai", "gpt-6-astra"}:                    builtinVisionModel(1_050_000, 128_000, "gpt-6"),
	{"deepseek", "deepseek-flash"}:               builtinVisionModel(1_000_000, 384_000, "deepseek-flash"),
	{"deepseek", "deepseek-v4-flash"}:            builtinVisionModel(1_000_000, 384_000, "deepseek-flash"),
	{"deepseek", "deepseek-v4.1-flash"}:          builtinVisionModel(1_000_000, 384_000, "deepseek-flash"),
	{"deepseek", "deepseek-v4-flash-vision-exp"}: builtinVisionModel(1_000_000, 384_000, "deepseek-flash"),
}

func staticModelsDevID(provider string) (string, bool) {
	for _, pair := range providerToModelsDev {
		if pair.provider == provider {
			return pair.mdev, true
		}
	}
	return "", false
}

func (c *Catalog) profileModelsDevIDs(ctx context.Context) []providerPair {
	var out []providerPair
	for _, p := range c.reg.All(ctx) {
		spec := p.Spec()
		if _, static := staticModelsDevID(spec.Name); spec.ModelsDevID != "" && !static {
			out = append(out, providerPair{spec.Name, spec.ModelsDevID})
		}
	}
	return out
}

func (c *Catalog) ModelsDevProviderMap(ctx context.Context) map[string]string {
	ctx = c.scoped(ctx)
	out := make(map[string]string, len(providerToModelsDev))
	for _, pair := range providerToModelsDev {
		out[pair.provider] = pair.mdev
	}
	for _, pair := range c.profileModelsDevIDs(ctx) {
		out[pair.provider] = pair.mdev
	}
	return out
}

func (c *Catalog) modelsDevIDFor(ctx context.Context, provider string) (string, bool) {
	key := strings.TrimSpace(provider)
	if key == "" {
		return "", false
	}
	if id, ok := staticModelsDevID(key); ok {
		return id, true
	}
	for _, pair := range c.profileModelsDevIDs(ctx) {
		if pair.provider == key {
			return pair.mdev, true
		}
	}
	return "", false
}

func (c *Catalog) ModelsDevIDFor(ctx context.Context, provider string) (string, bool) {
	return c.modelsDevIDFor(c.scoped(ctx), provider)
}

func (c *Catalog) modelsDevToProviderIDs(ctx context.Context, mdevID string) []string {
	var out []string
	for _, pair := range providerToModelsDev {
		if pair.mdev == mdevID {
			out = append(out, pair.provider)
		}
	}
	for _, pair := range c.profileModelsDevIDs(ctx) {
		if pair.mdev == mdevID {
			out = append(out, pair.provider)
		}
	}
	return out
}

func (c *Catalog) frozenConfig(ctx context.Context, cfg *config.Config) *config.Config {
	if cfg != nil {
		return cfg
	}
	return c.config(ctx)
}

func (c *Catalog) configuredCatalogProvider(ctx context.Context, provider string, cfg *config.Config) (string, bool) {
	name := strings.TrimSpace(provider)
	if strings.HasPrefix(strings.ToLower(name), "custom:") {
		name = name[len("custom:"):]
	}
	if name == "" {
		return "", false
	}
	if _, known := c.modelsDevIDFor(ctx, name); known {
		return "", false
	}
	cfg = c.frozenConfig(ctx, cfg)
	if cfg == nil {
		return "", false
	}
	alias := ""
	if entry, ok := cfg.Providers.Get(name); ok {
		alias = entry.CatalogProvider
	}
	if alias == "" {
		for _, entry := range cfg.Extra.CustomProviders.Entries {
			if entry.Valid() && strings.TrimSpace(entry.Name) == name {
				alias = entry.CatalogProvider
				break
			}
		}
	}
	alias = strings.TrimSpace(alias)
	return alias, alias != ""
}

func (c *Catalog) modelsDevID(ctx context.Context, provider string, cfg *config.Config) (string, bool) {
	key := strings.TrimSpace(provider)
	if id, ok := c.modelsDevIDFor(ctx, key); ok {
		return id, true
	}
	alias, ok := c.configuredCatalogProvider(ctx, key, cfg)
	if !ok {
		return "", false
	}
	id, mapped := c.modelsDevIDFor(ctx, alias)
	if !mapped {
		id = alias
	}
	known := slices.ContainsFunc(providerToModelsDev, func(p providerPair) bool { return p.mdev == id }) ||
		slices.ContainsFunc(c.profileModelsDevIDs(ctx), func(p providerPair) bool { return p.mdev == id })
	if !known {
		reg := c.modelsDevRegistry(ctx, false)
		if reg != nil {
			_, known = reg.providers[id]
		}
	}
	if !known {
		if c.once("catalog-provider\x00" + key + "\x00" + alias) {
			slog.Warn("catalog_provider is neither an icebath provider id nor a models.dev id, ignoring", "provider", key, "catalog_provider", alias)
		}
		return "", false
	}
	return id, true
}

func (c *Catalog) registryProvider(ctx context.Context, mdevID string, allowNetwork bool) *mdProvider {
	reg := c.modelsDevRegistry(ctx, allowNetwork)
	if reg == nil {
		return nil
	}
	p := reg.providers[mdevID]
	if p == nil || !p.valid {
		return nil
	}
	return p
}

func (c *Catalog) registryModels(ctx context.Context, mdevID string, allowNetwork bool) *mdProvider {
	p := c.registryProvider(ctx, mdevID, allowNetwork)
	if p == nil || !p.hasModels {
		return nil
	}
	return p
}

func (c *Catalog) providerModels(ctx context.Context, provider string, allowNetwork bool, cfg *config.Config) *mdProvider {
	id, ok := c.modelsDevID(ctx, provider, cfg)
	if !ok || id == "" {
		return nil
	}
	return c.registryModels(ctx, id, allowNetwork)
}

func modelEntries(models *mdProvider, model string, suffixFallback bool, provider string) []*mdModel {
	names := []string{model}
	if suffixFallback {
		names = append(names, model+":cloud", model+"-cloud")
	}
	var out []*mdModel
	for _, name := range names {
		if entry := models.models[name]; entry != nil {
			out = append(out, entry)
		}
		lower := strings.ToLower(name)
		for _, id := range models.order {
			if entry := models.models[id]; entry != nil && strings.ToLower(id) == lower {
				out = append(out, entry)
			}
		}
	}
	if provider == "openrouter" {
		if base, ok := OpenRouterVariantBase(model); ok {
			out = append(out, modelEntries(models, base, suffixFallback, "")...)
		}
	}
	return out
}

func (m *mdModel) limit(key string) (int, bool) {
	value, ok := m.limits[key]
	if !ok || value <= 0 {
		return 0, false
	}
	return int(value), true
}

func (m *mdModel) clone() *mdModel {
	out := *m
	out.limits, out.flags, out.cost = maps.Clone(m.limits), maps.Clone(m.flags), maps.Clone(m.cost)
	out.inputMods, out.outputMods = slices.Clone(m.inputMods), slices.Clone(m.outputMods)
	if out.limits == nil {
		out.limits = map[string]float64{}
	}
	if out.flags == nil {
		out.flags = map[string]bool{}
	}
	return &out
}

func (m *mdModel) supportsVision() bool {
	if m.inputIsList {
		return slices.Contains(m.inputMods, "image")
	}
	return m.flags["attachment"]
}

func overrideIsEmpty(ov config.ModelOverride) bool {
	return ov.ContextWindow == nil && ov.SupportsTools == nil && ov.SupportsVision == nil && ov.SupportsReasoning == nil && ov.ModelFamily == ""
}

func (c *Catalog) overrideInt(raw *int, key string) (int, bool) {
	if raw == nil {
		return 0, false
	}
	if *raw > 0 {
		return *raw, true
	}
	if c.once("override\x00" + key + "\x00" + strconv.Itoa(*raw)) {
		slog.Warn("model_overrides: ignoring an invalid value (expected a positive integer)", "key", key, "value", *raw)
	}
	return 0, false
}

func (c *Catalog) providerOverrideSection(ctx context.Context, provider string, cfg *config.Config) (config.Map[config.ModelOverride], bool) {
	cfg = c.frozenConfig(ctx, cfg)
	key := strings.TrimSpace(provider)
	if cfg == nil || key == "" || (cfg.ModelOverrides.Providers.Len() == 0 && cfg.ModelOverrides.Default == nil) {
		return config.Map[config.ModelOverride]{}, false
	}
	candidates := []string{key}
	if id, ok := c.modelsDevIDFor(ctx, key); ok {
		candidates = append(candidates, id)
	}
	candidates = append(candidates, c.modelsDevToProviderIDs(ctx, key)...)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if section, ok := cfg.ModelOverrides.Providers.Get(candidate); ok {
			return section, true
		}
	}
	return config.Map[config.ModelOverride]{}, false
}

func (c *Catalog) explicitModelOverride(ctx context.Context, provider, model string, cfg *config.Config) (config.ModelOverride, bool) {
	modelKey := strings.TrimSpace(model)
	if modelKey == "" {
		return config.ModelOverride{}, false
	}
	section, ok := c.providerOverrideSection(ctx, provider, cfg)
	if !ok {
		return config.ModelOverride{}, false
	}
	if entry, ok := section.Get(modelKey); ok {
		return entry, true
	}
	lower := strings.ToLower(modelKey)
	for _, id := range section.Keys() {
		if id != defaultOverrideKey && strings.ToLower(id) == lower {
			return section.Get(id)
		}
	}
	return config.ModelOverride{}, false
}

func (c *Catalog) defaultModelOverride(ctx context.Context, provider string, cfg *config.Config) (config.ModelOverride, bool) {
	if section, ok := c.providerOverrideSection(ctx, provider, cfg); ok {
		if entry, ok := section.Get(defaultOverrideKey); ok {
			return entry, true
		}
	}
	cfg = c.frozenConfig(ctx, cfg)
	if cfg == nil || cfg.ModelOverrides.Default == nil {
		return config.ModelOverride{}, false
	}
	return *cfg.ModelOverrides.Default, true
}

func (c *Catalog) overrideFor(ctx context.Context, provider, model string, catalogHit bool, cfg *config.Config) (config.ModelOverride, bool) {
	if explicit, ok := c.explicitModelOverride(ctx, provider, model, cfg); ok {
		return explicit, true
	}
	if catalogHit {
		return config.ModelOverride{}, false
	}
	return c.defaultModelOverride(ctx, provider, cfg)
}

func (c *Catalog) overrideContextWindow(ctx context.Context, provider, model string) (int, bool) {
	ov, ok := c.explicitModelOverride(ctx, provider, model, nil)
	if !ok {
		return 0, false
	}
	return c.overrideInt(ov.ContextWindow, "context_window")
}

func (c *Catalog) defaultOverrideContext(ctx context.Context, provider string) (int, bool) {
	ov, ok := c.defaultModelOverride(ctx, provider, nil)
	if !ok {
		return 0, false
	}
	return c.overrideInt(ov.ContextWindow, "context_window")
}

func (c *Catalog) mergeOverride(raw *mdModel, ov config.ModelOverride) *mdModel {
	merged := raw.clone()
	if window, ok := c.overrideInt(ov.ContextWindow, "context_window"); ok {
		merged.limits["context"] = float64(window)
	}
	if ov.SupportsVision != nil {
		mods := []string{}
		if raw.inputIsList {
			mods = slices.Clone(raw.inputMods)
		}
		has := slices.Contains(mods, "image")
		if *ov.SupportsVision && !has {
			mods = append(mods, "image")
		} else if !*ov.SupportsVision && has {
			mods = slices.DeleteFunc(mods, func(m string) bool { return m == "image" })
		}
		merged.hasModalities, merged.inputIsList, merged.inputMods = true, true, mods
		merged.flags["attachment"] = *ov.SupportsVision
	}
	if ov.SupportsTools != nil {
		merged.flags["tool_call"] = *ov.SupportsTools
	}
	if ov.SupportsReasoning != nil {
		merged.flags["reasoning"] = *ov.SupportsReasoning
	}
	if ov.ModelFamily != "" {
		merged.family = ov.ModelFamily
	}
	return merged
}

func (c *Catalog) builtinModel(ctx context.Context, provider, model string, cfg *config.Config) *mdModel {
	key, ok := c.modelsDevID(ctx, provider, cfg)
	if !ok || key == "" {
		key = strings.TrimSpace(provider)
	}
	return builtinModelMetadata[[2]string{key, strings.ToLower(strings.TrimSpace(model))}]
}

func (c *Catalog) relayVisionMarker(ctx context.Context, provider, model string) *mdModel {
	if !strings.Contains(strings.ToLower(strings.TrimSpace(model)), "-vision") {
		return nil
	}
	if _, ok := c.reg.OpenCodeFamily(ctx, provider); !ok {
		return nil
	}
	base := unknownModelBase()
	base.hasModalities, base.inputIsList, base.inputMods, base.outputMods = true, true, []string{"text", "image"}, []string{"text"}
	return base
}

func (c *Catalog) declaredCapabilities(ctx context.Context, provider, model string) (config.ModelOverride, bool) {
	profile, ok := c.reg.Get(ctx, provider)
	if !ok {
		return config.ModelOverride{}, false
	}
	declared, ok := profile.Spec().ModelCapabilities[model]
	return declared, ok && !overrideIsEmpty(declared)
}

func (c *Catalog) applyOverrides(ctx context.Context, provider, model string, entry *mdModel, cfg *config.Config) *mdModel {
	base := entry
	if base == nil {
		base = c.builtinModel(ctx, provider, model, cfg)
	}
	if declared, ok := c.declaredCapabilities(ctx, provider, model); ok {
		if base == nil {
			base = unknownModelBase()
		}
		base = c.mergeOverride(base, declared)
	}
	override, overridden := c.overrideFor(ctx, provider, model, base != nil, cfg)
	if base == nil {
		base = c.relayVisionMarker(ctx, provider, model)
	}
	if !overridden {
		return base
	}
	if base == nil {
		base = unknownModelBase()
	}
	return c.mergeOverride(base, override)
}

func (c *Catalog) lookupModelsDevContext(ctx context.Context, provider, model string, allowNetwork bool) (int, bool) {
	if window, ok := c.overrideContextWindow(ctx, provider, model); ok {
		return window, true
	}
	if declared, ok := c.declaredCapabilities(ctx, provider, model); ok {
		if window, ok := c.overrideInt(declared.ContextWindow, "context_window"); ok {
			return window, true
		}
	}
	if models := c.providerModels(ctx, provider, allowNetwork, nil); models != nil {
		for _, entry := range modelEntries(models, model, true, provider) {
			if window, ok := entry.limit("context"); ok {
				return window, true
			}
		}
	}
	return c.defaultOverrideContext(ctx, provider)
}

func (c *Catalog) ModelsDevContext(ctx context.Context, provider, model string, allowNetwork bool) (int, bool) {
	return c.lookupModelsDevContext(c.scoped(ctx), provider, model, allowNetwork)
}

func (c *Catalog) Capabilities(ctx context.Context, provider, model string, allowNetwork bool, cfg *config.Config) (Capabilities, bool) {
	ctx = c.scoped(ctx)
	var entry *mdModel
	if models := c.providerModels(ctx, provider, allowNetwork, cfg); models != nil {
		if found := modelEntries(models, model, true, provider); len(found) > 0 {
			entry = found[0]
		}
	}
	unknownBase := entry == nil && c.builtinModel(ctx, provider, model, cfg) == nil
	raw := c.applyOverrides(ctx, provider, model, entry, cfg)
	if raw == nil {
		return Capabilities{}, false
	}
	out := Capabilities{SupportsTools: raw.flags["tool_call"], ContextWindow: unknownModelContext, ModelFamily: raw.family}
	if _, declared := raw.flags["attachment"]; !unknownBase || declared || raw.hasModalities {
		out.SupportsVision = new(raw.supportsVision())
	}
	if _, declared := raw.flags["reasoning"]; !unknownBase || declared {
		out.SupportsReasoning = new(raw.flags["reasoning"])
	}
	if window, ok := raw.limit("context"); ok {
		out.ContextWindow = window
	}
	out.MaxOutputTokens, _ = raw.limit("output")
	return out, true
}

func hiddenFromProviderCatalog(provider, modelID string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return (p == "gemini" || p == "google") && slices.Contains(googleHiddenModels, strings.ToLower(strings.TrimSpace(modelID)))
}

func (c *Catalog) ListProviderModels(ctx context.Context, provider string, allowNetwork bool) []string {
	ctx = c.scoped(ctx)
	if normalized := c.normalizeProvider(ctx, provider); normalized != "" {
		provider = normalized
	}
	models := c.providerModels(ctx, provider, allowNetwork, nil)
	out := []string{}
	if models == nil {
		return out
	}
	for _, id := range models.order {
		if !hiddenFromProviderCatalog(provider, id) {
			out = append(out, id)
		}
	}
	return out
}

func (c *Catalog) ListAgenticModels(ctx context.Context, provider string, allowNetwork bool) []string {
	ctx = c.scoped(ctx)
	models := c.providerModels(ctx, provider, allowNetwork, nil)
	out := []string{}
	if models == nil {
		return out
	}
	for _, id := range models.order {
		entry := models.models[id]
		if entry != nil && !hiddenFromProviderCatalog(provider, id) && entry.flags["tool_call"] && !noisePatterns.MatchString(id) {
			out = append(out, id)
		}
	}
	return out
}

func modelInfoOf(id string, raw *mdModel, providerID string) ModelInfo {
	info := ModelInfo{
		ID: id, Name: raw.name, Family: raw.family, ProviderID: providerID,
		Reasoning: raw.flags["reasoning"], ToolCall: raw.flags["tool_call"], Attachment: raw.flags["attachment"],
		Temperature: raw.flags["temperature"], StructuredOutput: raw.flags["structured_output"], OpenWeights: raw.flags["open_weights"],
		InputModalities: slices.Clone(raw.inputMods), OutputModalities: slices.Clone(raw.outputMods),
		CostInput: raw.cost["input"], CostOutput: raw.cost["output"],
		KnowledgeCutoff: raw.knowledge, ReleaseDate: raw.releaseDate, Status: raw.status,
		Interleaved: raw.interleaved, InterleavedField: raw.interleavedField,
	}
	if info.Name == "" {
		info.Name = id
	}
	info.ContextWindow, _ = raw.limit("context")
	info.MaxOutput, _ = raw.limit("output")
	info.MaxInput, _ = raw.limit("input")
	if rate, ok := raw.cost["cache_read"]; ok {
		info.CostCacheRead = &rate
	}
	if rate, ok := raw.cost["cache_write"]; ok {
		info.CostCacheWrite = &rate
	}
	return info
}

func (c *Catalog) providerInfo(ctx context.Context, providerID string, allowNetwork bool, cfg *config.Config) (ProviderInfo, bool) {
	id, ok := c.modelsDevID(ctx, providerID, cfg)
	if !ok || id == "" {
		id = providerID
	}
	raw := c.registryProvider(ctx, id, allowNetwork)
	if raw == nil {
		return ProviderInfo{}, false
	}
	info := ProviderInfo{ID: id, Name: raw.name, Env: slices.Clone(raw.env), API: raw.api, Doc: raw.doc}
	if info.Name == "" {
		info.Name = id
	}
	if raw.hasModels {
		info.ModelCount = len(raw.order)
	}
	return info, true
}

func (c *Catalog) ProviderInfo(ctx context.Context, providerID string, allowNetwork bool, cfg *config.Config) (ProviderInfo, bool) {
	return c.providerInfo(c.scoped(ctx), providerID, allowNetwork, cfg)
}

func (c *Catalog) ModelInfo(ctx context.Context, providerID, modelID string, allowNetwork bool, cfg *config.Config) (ModelInfo, bool) {
	ctx = c.scoped(ctx)
	id, ok := c.modelsDevID(ctx, providerID, cfg)
	if !ok || id == "" {
		id = providerID
	}
	matchedID := modelID
	var entry *mdModel
	if models := c.registryModels(ctx, id, allowNetwork); models != nil {
		if found := modelEntries(models, modelID, false, providerID); len(found) > 0 {
			entry, matchedID = found[0], found[0].id
		}
	}
	raw := c.applyOverrides(ctx, providerID, modelID, entry, cfg)
	if raw == nil {
		return ModelInfo{}, false
	}
	return modelInfoOf(matchedID, raw, id), true
}

type cacheOnlyKey struct{}

func cacheOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheOnlyKey{}, true)
}

func (c *Catalog) ModelsDevProvider(ctx context.Context, canonical string) (providers.ModelsDevInfo, bool) {
	allowNetwork := ctx.Value(cacheOnlyKey{}) == nil
	info, ok := c.providerInfo(c.scoped(ctx), canonical, allowNetwork, nil)
	if !ok {
		return providers.ModelsDevInfo{}, false
	}
	return providers.ModelsDevInfo{Name: info.Name, Env: info.Env, API: info.API, Doc: info.Doc}, true
}
