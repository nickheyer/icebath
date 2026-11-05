package provider

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Icebath only provider metadata layered over models.dev, keyed by canonical id
var icebathOverlays = map[string]IcebathOverlay{
	"moa":        {AuthType: "virtual", BaseURLOverride: "moa://local"},
	"openrouter": {IsAggregator: true, BaseURLEnvVar: "OPENROUTER_BASE_URL"},
	"nous":       {AuthType: "oauth_device_code", BaseURLOverride: "https://inference-api.nousresearch.com/v1"},
	"openai-codex": {Transport: "codex_responses", AuthType: "oauth_external",
		BaseURLOverride: "https://chatgpt.com/backend-api/codex"},
	"openai-api": {Transport: "codex_responses", BaseURLOverride: "https://api.openai.com/v1",
		BaseURLEnvVar: "OPENAI_BASE_URL"},
	"xai-oauth": {Transport: "codex_responses", AuthType: "oauth_external",
		BaseURLOverride: "https://api.x.ai/v1", BaseURLEnvVar: "XAI_BASE_URL"},
	"qwen-oauth": {AuthType: "oauth_external", BaseURLOverride: "https://portal.qwen.ai/v1",
		BaseURLEnvVar: "ICEBATH_QWEN_BASE_URL"},
	"lmstudio": {ExtraEnvVars: []string{"LM_API_KEY"}, BaseURLOverride: "http://127.0.0.1:1234/v1",
		BaseURLEnvVar: "LM_BASE_URL"},
	"anthropic":       {Transport: "anthropic_messages", ExtraEnvVars: []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}},
	"zai":             {ExtraEnvVars: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, BaseURLEnvVar: "GLM_BASE_URL"},
	"kimi-for-coding": {BaseURLEnvVar: "KIMI_BASE_URL"},
	"stepfun": {ExtraEnvVars: []string{"STEPFUN_API_KEY"},
		BaseURLOverride: "https://api.stepfun.ai/step_plan/v1", BaseURLEnvVar: "STEPFUN_BASE_URL"},
	"minimax": {Transport: "anthropic_messages", BaseURLEnvVar: "MINIMAX_BASE_URL"},
	"minimax-oauth": {Transport: "anthropic_messages", AuthType: "oauth_external",
		BaseURLOverride: "https://api.minimax.io/anthropic"},
	"minimax-cn":          {Transport: "anthropic_messages", BaseURLEnvVar: "MINIMAX_CN_BASE_URL"},
	"deepseek":            {BaseURLEnvVar: "DEEPSEEK_BASE_URL"},
	"alibaba":             {BaseURLEnvVar: "DASHSCOPE_BASE_URL"},
	"alibaba-coding-plan": {BaseURLEnvVar: "ALIBABA_CODING_PLAN_BASE_URL"},
	"vercel":              {IsAggregator: true},
	"opencode":            {IsAggregator: true, BaseURLEnvVar: "OPENCODE_ZEN_BASE_URL"},
	"opencode-go":         {IsAggregator: true, BaseURLEnvVar: "OPENCODE_GO_BASE_URL"},
	"kilo":                {IsAggregator: true, BaseURLEnvVar: "KILOCODE_BASE_URL"},
	"huggingface":         {IsAggregator: true, BaseURLEnvVar: "HF_BASE_URL"},
	"novita":              {IsAggregator: true, BaseURLEnvVar: "NOVITA_BASE_URL"},
	"xai":                 {Transport: "codex_responses", BaseURLOverride: "https://api.x.ai/v1", BaseURLEnvVar: "XAI_BASE_URL"},
	"nvidia":              {BaseURLOverride: "https://integrate.api.nvidia.com/v1", BaseURLEnvVar: "NVIDIA_BASE_URL"},
	"xiaomi":              {BaseURLEnvVar: "XIAOMI_BASE_URL"},
	"tencent-tokenhub":    {BaseURLEnvVar: "TOKENHUB_BASE_URL"},
	"tencent-tokenplan": {Transport: "anthropic_messages",
		BaseURLOverride: "https://api.lkeap.cloud.tencent.com/plan/anthropic", BaseURLEnvVar: "TOKENPLAN_BASE_URL"},
	"arcee": {BaseURLOverride: "https://api.arcee.ai/api/v1", BaseURLEnvVar: "ARCEE_BASE_URL"},
	"commandcode": {ExtraEnvVars: []string{"COMMANDCODE_API_KEY"},
		BaseURLOverride: "https://api.commandcode.ai/provider/v1", BaseURLEnvVar: "COMMANDCODE_BASE_URL"},
	"gmi": {ExtraEnvVars: []string{"GMI_API_KEY"}, BaseURLOverride: "https://api.gmi-serving.com/v1",
		BaseURLEnvVar: "GMI_BASE_URL"},
	"fireworks": {ExtraEnvVars: []string{"FIREWORKS_API_KEY"}, BaseURLOverride: "https://api.fireworks.ai/inference/v1"},
	"actual": {Transport: "chat_completions", ExtraEnvVars: []string{"ACTUAL_API_KEY"},
		BaseURLOverride: "https://api.actual.inc/v1", BaseURLEnvVar: "ACTUAL_BASE_URL"},
	"upstage": {ExtraEnvVars: []string{"UPSTAGE_API_KEY"}, BaseURLOverride: "https://api.upstage.ai/v1",
		BaseURLEnvVar: "UPSTAGE_BASE_URL"},
	"nebius-token-factory": {ExtraEnvVars: []string{"NEBIUS_API_KEY", "NEBIUS_TOKEN_FACTORY_API_KEY"},
		BaseURLOverride: "https://api.tokenfactory.nebius.com/v1", BaseURLEnvVar: "NEBIUS_BASE_URL"},
	"ollama-cloud":  {BaseURLOverride: "https://ollama.com/v1", BaseURLEnvVar: "OLLAMA_BASE_URL"},
	"azure-foundry": {BaseURLEnvVar: "AZURE_FOUNDRY_BASE_URL"},
	"bedrock":       {Transport: "bedrock_converse", AuthType: "aws_sdk"},
	"vertex":        {AuthType: "vertex"},
}

// Human friendly and legacy provider names grouped by canonical models.dev id, later groups win
var providerDefAliasGroups = []struct {
	canon   string
	aliases []string
}{
	{"openrouter", []string{"openai"}}, {"zai", []string{"glm", "z-ai", "z.ai", "zhipu"}},
	{"xai", []string{"x-ai", "x.ai", "grok"}},
	{"xai-oauth", []string{"grok-oauth", "xai-oauth", "x-ai-oauth", "xai-grok-oauth"}},
	{"nvidia", []string{"nim", "nvidia-nim", "build-nvidia", "nemotron"}},
	{"kimi-for-coding", []string{"kimi", "kimi-coding", "kimi-coding-cn", "moonshot"}},
	{"stepfun", []string{"step", "stepfun-coding-plan"}}, {"minimax-cn", []string{"minimax-china", "minimax_cn"}},
	{"anthropic", []string{"claude", "claude-code"}}, {"openai-codex", []string{"chatgpt", "chatgpt-codex"}},
	{"vercel", []string{"ai-gateway", "aigateway", "vercel-ai-gateway"}},
	{"opencode", []string{"opencode-zen", "zen"}}, {"opencode-go", []string{"go", "opencode-go-sub"}},
	{"kilo", []string{"kilocode", "kilo-code", "kilo-gateway"}},
	{"deepseek", []string{"deep-seek"}}, {"alibaba", []string{"dashscope", "aliyun", "qwen", "alibaba-cloud"}},
	{"alibaba-coding-plan", []string{"alibaba_coding", "alibaba-coding", "alibaba_coding_plan"}},
	{"huggingface", []string{"hf", "hugging-face", "huggingface-hub"}}, {"novita", []string{"novita-ai", "novitaai"}},
	{"xiaomi", []string{"mimo", "xiaomi-mimo"}},
	{"tencent-tokenhub", []string{"tencent", "tokenhub", "tencent-cloud", "tencentmaas"}},
	{"tencent-tokenplan", []string{"tokenplan", "tencent-lkeap"}},
	{"bedrock", []string{"aws", "aws-bedrock", "amazon-bedrock", "amazon"}}, {"arcee", []string{"arcee-ai", "arceeai"}},
	{"gmi", []string{"gmi-cloud", "gmicloud"}}, {"fireworks", []string{"fireworks-ai", "fw"}},
	{"upstage", []string{"solar"}}, {"actual", []string{"actual-computer", "actualcomputer", "aci"}},
	{"nebius-token-factory", []string{"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"}},
	{"lmstudio", []string{"lmstudio", "lm-studio", "lm_studio"}},
	{"custom", []string{"ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp"}},
}

// Alias to canonical id lookup inverted from the alias groups
var providerDefAliases = func() map[string]string {
	out := map[string]string{}
	for _, g := range providerDefAliasGroups {
		for _, a := range g.aliases {
			out[a] = g.canon
		}
	}
	return out
}()

// Canonical ids owning an alias group
var providerDefCanonicals = func() map[string]bool {
	out := map[string]bool{}
	for _, g := range providerDefAliasGroups {
		out[g.canon] = true
	}
	return out
}()

// Display labels for providers the models.dev catalog does not name
var providerLabelOverrides = map[string]string{
	"moa": "Mixture of Agents", "nous": "Nous Portal", "openai-codex": "ChatGPT or Codex Subscription",
	"stepfun": "StepFun Step Plan", "xiaomi": "Xiaomi MiMo", "gmi": "GMI Cloud",
	"upstage": "Upstage Solar", "actual": "Actual Computer", "tencent-tokenhub": "Tencent TokenHub",
	"nebius-token-factory": "Nebius Token Factory", "tencent-tokenplan": "Tencent TokenPlan", "lmstudio": "LM Studio",
	"custom": "Custom endpoint", "bedrock": "AWS Bedrock", "vertex": "Google Vertex AI", "ollama-cloud": "Ollama Cloud",
	"xai-oauth": "xAI Grok OAuth (SuperGrok / Premium+)",
}

// Overlay transport names mapped onto api modes
var transportToAPIMode = map[string]APIMode{
	"openai_chat": ModeChatCompletions, "anthropic_messages": ModeAnthropicMessages,
	"codex_responses": ModeCodexResponses, "bedrock_converse": modeBedrockConverse,
}

// Flat namespace resellers whose catalog is first party, flagged aggregators for the flat catalog search only
var flatNamespaceResellers = map[string]bool{"opencode-go": true, "opencode": true}

// LlamacppProviderID is the managed llama.cpp runtime provider id the resolver and the picker Local row share
const LlamacppProviderID = "llamacpp"

// Provider names that address the managed llama.cpp runtime
var llamacppAliases = []string{LlamacppProviderID, "llama.cpp", "llama-cpp"}

// Overlay with the unset transport and auth type taking their api key chat defaults
func (o IcebathOverlay) withDefaults() IcebathOverlay {
	if o.Transport == "" {
		o.Transport = "openai_chat"
	}
	if o.AuthType == "" {
		o.AuthType = "api_key"
	}
	return o
}

// Registered profiles declaring a provider overlay, read without a plugin dir rescan
func overlayProfiles(ctx context.Context) []*Profile {
	var out []*Profile
	for _, p := range listProviders(ctx) {
		if p.Overlay != nil {
			out = append(out, p)
		}
	}
	return out
}

// Overlay declaring profile a name, models.dev id or overlay alias belongs to, nil profiles reads the registry
func overlayProfile(ctx context.Context, name string, profiles []*Profile) *Profile {
	if name == "" {
		return nil
	}
	if profiles == nil {
		profiles = overlayProfiles(ctx)
	}
	for _, p := range profiles {
		if name == p.Name || name == p.ModelsDevID || slices.Contains(p.OverlayAliases, name) {
			return p
		}
	}
	return nil
}

// Key a profile overlay is filed under, its models.dev id else its name
func profileOverlayKey(p *Profile) string {
	if p.ModelsDevID != "" {
		return p.ModelsDevID
	}
	return p.Name
}

// IcebathOverlays returns the static overlays plus those the bound home's profiles declare
func IcebathOverlays(ctx context.Context) map[string]IcebathOverlay {
	merged := make(map[string]IcebathOverlay, len(icebathOverlays))
	for k, v := range icebathOverlays {
		merged[k] = v.withDefaults()
	}
	for _, p := range overlayProfiles(ctx) {
		if _, ok := merged[profileOverlayKey(p)]; !ok {
			merged[profileOverlayKey(p)] = p.Overlay.withDefaults()
		}
	}
	return merged
}

// Whether a key is a built in canonical id that no profile overlay can rename
func isStaticCanonical(key string) bool {
	if providerRegistry().builtin[key] {
		return true
	}
	if _, ok := icebathOverlays[key]; ok {
		return true
	}
	return providerDefCanonicals[key]
}

// NormalizeProviderDef resolves aliases and casing to the canonical models.dev keyed provider id
func NormalizeProviderDef(ctx context.Context, name string) string {
	return normalizeProviderDefWith(ctx, name, nil)
}

// Canonical id reading a prefetched overlay profile list, nil reads the registry
func normalizeProviderDefWith(ctx context.Context, name string, profiles []*Profile) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if canon, ok := providerDefAliases[key]; ok {
		return canon
	}
	if isStaticCanonical(key) {
		return key
	}
	if p := overlayProfile(ctx, key, profiles); p != nil {
		return profileOverlayKey(p)
	}
	return key
}

// Overlay and label for a canonical id from the static table, else from the profile filing an overlay under it
func getOverlay(ctx context.Context, canonical string) (*IcebathOverlay, string) {
	if ov, ok := icebathOverlays[canonical]; ok {
		ov = ov.withDefaults()
		if label, ok := providerLabelOverrides[canonical]; ok {
			return &ov, label
		}
		return &ov, canonical
	}
	p := overlayProfile(ctx, canonical, nil)
	if p == nil || profileOverlayKey(p) != canonical {
		return nil, canonical
	}
	ov := p.Overlay.withDefaults()
	if label, ok := providerLabelOverrides[canonical]; ok {
		return &ov, label
	}
	if p.DisplayName != "" {
		return &ov, p.DisplayName
	}
	return &ov, canonical
}

// IsActualRoute identifies Actual Computer by provider alias or its hosted endpoint
func IsActualRoute(providerName, baseURL string) bool {
	return isActualRouteCtx(context.Background(), providerName, baseURL)
}

func isActualRouteCtx(ctx context.Context, providerName, baseURL string) bool {
	return NormalizeProviderDef(ctx, providerName) == "actual" || Hostname(baseURL) == "api.actual.inc"
}

// models.dev provider entry for a canonical id or nil when the catalog lacks it
var modelsDevInfoFn = func(ctx context.Context, canonical string, allowNetwork bool) *ModelsDevProvider {
	info, ok := ModelsDevProviderInfo(ctx, canonical, allowNetwork)
	if !ok {
		return nil
	}
	return &info
}

func modelsDevInfo(ctx context.Context, canonical string, allowNetwork bool) *ModelsDevProvider {
	return modelsDevInfoFn(ctx, canonical, allowNetwork)
}

// Provider definition carrying an overlay's transport, env var, aggregator and auth fields
func overlayPdef(canonical string, ov IcebathOverlay, name string, envVars []string, baseURL, doc, source string) *ProviderDef {
	return &ProviderDef{ID: canonical, Name: name, Transport: ov.Transport, KeyEnv: envVars, BaseURL: baseURL,
		BaseURLEnv: ov.BaseURLEnvVar, IsAggregator: ov.IsAggregator, AuthType: ov.AuthType, Doc: doc, Source: source}
}

// Plugin profile id when the canonical name is only one of its aliases, so the plugin wins over models.dev
func pluginAliasOwner(ctx context.Context, canonical string) string {
	if canonical == "" || isStaticCanonical(canonical) {
		return canonical
	}
	named, aliased := false, false
	for _, p := range listProviders(ctx) {
		named = named || p.Name == canonical
		aliased = aliased || slices.Contains(p.Aliases, canonical)
	}
	if named || !aliased {
		return canonical
	}
	owner := getProviderProfile(ctx, canonical)
	if owner == nil || !slices.Contains(owner.Aliases, canonical) || owner.Overlay != nil {
		return canonical
	}
	return owner.Name
}

// GetProviderDef looks up a built in provider by id or alias, nil for an unknown or left core provider
func GetProviderDef(ctx context.Context, name string, allowNetwork bool) *ProviderDef {
	canonical := pluginAliasOwner(ctx, NormalizeProviderDef(ctx, name))
	if ProviderLeftCore(canonical) {
		return nil
	}
	mdev := modelsDevInfo(ctx, canonical, allowNetwork)
	overlay, overlayLabel := getOverlay(ctx, canonical)
	if mdev != nil {
		ov := IcebathOverlay{}.withDefaults()
		if overlay != nil {
			ov = *overlay
		}
		envVars := append([]string{}, mdev.Env...)
		for _, ev := range ov.ExtraEnvVars {
			if !slices.Contains(envVars, ev) {
				envVars = append(envVars, ev)
			}
		}
		baseURL := ov.BaseURLOverride
		if baseURL == "" {
			baseURL = mdev.API
		}
		return overlayPdef(canonical, ov, mdev.Name, envVars, baseURL, mdev.Doc, "models.dev")
	}
	if overlay != nil {
		return overlayPdef(canonical, *overlay, overlayLabel, append([]string{}, overlay.ExtraEnvVars...), overlay.BaseURLOverride, "", "icebath")
	}
	pdef := pluginProfilePdef(ctx, canonical)
	if pdef == nil || !(pdef.BaseURL != "" || (pdef.AuthType == "api_key" && len(pdef.KeyEnv) > 0 && pdef.BaseURLEnv != "")) {
		return nil
	}
	return pdef
}

// Registered profile for a name or alias as a provider definition keyed by the profile's canonical name
func pluginProfilePdef(ctx context.Context, name string) *ProviderDef {
	p := getProviderProfile(ctx, name)
	if p == nil {
		return nil
	}
	var urlVars, keyVars []string
	for _, v := range p.KeyEnv {
		if strings.HasSuffix(v, "_BASE_URL") || strings.HasSuffix(v, "_URL") {
			urlVars = append(urlVars, v)
		}
	}
	for _, v := range p.KeyEnv {
		if !slices.Contains(urlVars, v) {
			keyVars = append(keyVars, v)
		}
	}
	mode := strings.TrimSpace(string(p.Mode))
	transport := mode
	for t, m := range transportToAPIMode {
		if string(m) == mode {
			transport = t
		}
	}
	if transport == "" {
		transport = "openai_chat"
	}
	display := p.DisplayName
	if display == "" {
		display = p.Name
	}
	if display == "" {
		display = name
	}
	baseURLEnv := ""
	if len(urlVars) > 0 {
		baseURLEnv = urlVars[0]
	}
	authType := p.AuthType
	if authType == "" {
		authType = "api_key"
	}
	return &ProviderDef{ID: p.Name, Name: display, Transport: transport, KeyEnv: keyVars,
		BaseURL: strings.TrimSpace(p.BaseURL), BaseURLEnv: baseURLEnv, AuthType: authType, Source: "plugin-profile"}
}

// GetProviderLabel returns the label override, else the models.dev or profile name, else the canonical id
func GetProviderLabel(ctx context.Context, providerID string) string {
	canonical := NormalizeProviderDef(ctx, providerID)
	if label, ok := providerLabelOverrides[canonical]; ok {
		return label
	}
	if pdef := GetProviderDef(ctx, canonical, true); pdef != nil {
		return pdef.Name
	}
	return canonical
}

// IsAggregator reports whether the provider is a multi model aggregator or a named custom endpoint
func IsAggregator(providerName string) bool {
	return isAggregatorCtx(context.Background(), providerName)
}

func isAggregatorCtx(ctx context.Context, providerName string) bool {
	norm := NormalizeProviderDef(ctx, providerName)
	if strings.HasPrefix(norm, "custom:") {
		return true
	}
	pdef := GetProviderDef(ctx, norm, true)
	return pdef != nil && pdef.IsAggregator
}

// IsRoutingAggregator excludes flat namespace resellers whose catalog is first party
func IsRoutingAggregator(providerName string) bool {
	return isRoutingAggregatorCtx(context.Background(), providerName)
}

func isRoutingAggregatorCtx(ctx context.Context, providerName string) bool {
	norm := NormalizeProviderDef(ctx, providerName)
	if flatNamespaceResellers[norm] {
		return false
	}
	return isAggregatorCtx(ctx, norm)
}

// DetermineMode picks the wire from the host mandate, the Nous dual wire, then the provider transport
func DetermineMode(providerName, baseURL, model, nousWire string) APIMode {
	return determineModeCtx(context.Background(), providerName, baseURL, model, nousWire)
}

func determineModeCtx(ctx context.Context, providerName, baseURL, model, nousWire string) APIMode {
	if isActualRouteCtx(ctx, providerName, baseURL) {
		return ModeChatCompletions
	}
	if mandated := HostMandatedMode(baseURL); mandated != "" {
		return mandated
	}
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "nous", "nous-portal", "nousresearch":
		return NousMode(model, nousWire)
	}
	if pdef := GetProviderDef(ctx, providerName, true); pdef != nil {
		if m, ok := transportToAPIMode[pdef.Transport]; ok {
			return m
		}
		if registeredAPIModes()[pdef.Transport] {
			return APIMode(pdef.Transport)
		}
		return ModeChatCompletions
	}
	if providerName == "bedrock" {
		return modeBedrockConverse
	}
	return ModeChatCompletions
}

// Distinct registry row for an exact id whose alias collapse would merge several registry providers
func lossyAliasRegistryPdef(ctx context.Context, raw, canonical string) *ProviderDef {
	reg := providerRegistry()
	pcfg := reg.get(ctx, raw)
	if pcfg == nil {
		return nil
	}
	overlays := overlayProfiles(ctx)
	if overlays == nil {
		overlays = []*Profile{}
	}
	var rows []*Profile
	for _, it := range reg.items(ctx) {
		if normalizeProviderDefWith(ctx, it.Key, overlays) == canonical {
			rows = append(rows, it.Row)
		}
	}
	allSame := true
	for _, row := range rows {
		allSame = allSame && row == pcfg
	}
	if allSame && overlayProfile(ctx, canonical, overlays) != nil {
		return nil
	}
	if len(rows) > 1 {
		return &ProviderDef{ID: pcfg.Name, Name: pcfg.DisplayName, Transport: "openai_chat",
			KeyEnv: append([]string{}, pcfg.KeyEnv...), BaseURL: pcfg.BaseURL, AuthType: "api_key", Source: "icebath-auth-registry"}
	}
	return nil
}

// Whether GGUF models are staged under the home models dir for the runtime to serve
func hasStagedLocalModels() bool {
	return len(stagedModelIDs()) > 0
}

// Local provider when the managed or a detected llama.cpp server resolves without waiting, or a model is staged
func llamacppPdef(ctx context.Context) *ProviderDef {
	h := home.Dir(ctx)
	baseURL, _, ok := resolveLlamacppEndpointWithin(ctx, loadConfigTree(h), h, 0)
	if !ok && !hasStagedLocalModels() {
		return nil
	}
	return &ProviderDef{ID: LlamacppProviderID, Name: "Local", Transport: "openai_chat", BaseURL: baseURL,
		AuthType: "api_key", Source: "local-runtime"}
}

// ResolveProviderFull walks user providers, lossy registry ids, built ins, custom providers, llamacpp, models.dev, profiles
func ResolveProviderFull(ctx context.Context, name string, userProviders map[string]any, customProviders []any) *ProviderDef {
	canonical := NormalizeProviderDef(ctx, name)
	raw := strings.ToLower(strings.TrimSpace(name))
	if len(userProviders) > 0 {
		if pdef := resolveUserProvider(raw, userProviders); pdef != nil {
			return pdef
		}
	}
	if canonical != raw {
		if pdef := lossyAliasRegistryPdef(ctx, raw, canonical); pdef != nil {
			return pdef
		}
	}
	if pdef := GetProviderDef(ctx, canonical, true); pdef != nil {
		if pdef.Source == "plugin-profile" && len(userProviders) > 0 {
			if userPdef := resolveUserProvider(pdef.ID, userProviders); userPdef != nil {
				return userPdef
			}
		}
		return pdef
	}
	if len(userProviders) > 0 {
		for _, candidate := range []string{canonical, raw} {
			if pdef := resolveUserProvider(candidate, userProviders); pdef != nil {
				return pdef
			}
		}
	}
	if pdef := resolveCustomProvider(name, customProviders); pdef != nil {
		return pdef
	}
	if slices.Contains(llamacppAliases, raw) {
		if pdef := llamacppPdef(ctx); pdef != nil {
			return pdef
		}
	}
	if info := modelsDevInfo(ctx, canonical, true); info != nil {
		return &ProviderDef{ID: canonical, Name: info.Name, Transport: "openai_chat", KeyEnv: append([]string{}, info.Env...),
			BaseURL: info.API, AuthType: "api_key", Source: "models.dev"}
	}
	if pdef := pluginProfilePdef(ctx, canonical); pdef != nil && pdef.ID != "custom" {
		return pdef
	}
	return nil
}
