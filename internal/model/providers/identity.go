package providers

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	ProviderPortal = "portal"
	ProviderCustom = "custom"

	PortalInferenceURL = "https://inference-api.icebath.app/v1"
)

var staticOverlays = map[string]Overlay{
	"moa":          {AuthType: "virtual", BaseURLOverride: "moa://local"},
	"openrouter":   {Aggregator: true, BaseURLEnv: "OPENROUTER_BASE_URL"},
	ProviderPortal: {AuthType: authTypeOAuthDeviceCode, BaseURLOverride: PortalInferenceURL},
	"openai-codex": {Transport: "codex_responses", AuthType: authTypeOAuthExternal,
		BaseURLOverride: "https://chatgpt.com/backend-api/codex"},
	"openai-api": {Transport: "codex_responses", BaseURLOverride: "https://api.openai.com/v1",
		BaseURLEnv: "OPENAI_BASE_URL"},
	"xai-oauth": {Transport: "codex_responses", AuthType: authTypeOAuthExternal,
		BaseURLOverride: "https://api.x.ai/v1", BaseURLEnv: "XAI_BASE_URL"},
	"qwen-oauth": {AuthType: authTypeOAuthExternal, BaseURLOverride: "https://portal.qwen.ai/v1",
		BaseURLEnv: "ICEBATH_QWEN_BASE_URL"},
	"lmstudio": {ExtraEnv: []string{"LM_API_KEY"}, BaseURLOverride: "http://127.0.0.1:1234/v1",
		BaseURLEnv: "LM_BASE_URL"},
	"anthropic":       {Transport: "anthropic_messages", ExtraEnv: []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}},
	"zai":             {ExtraEnv: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, BaseURLEnv: "GLM_BASE_URL"},
	"kimi-for-coding": {BaseURLEnv: "KIMI_BASE_URL"},
	"stepfun": {ExtraEnv: []string{"STEPFUN_API_KEY"}, BaseURLOverride: "https://api.stepfun.ai/step_plan/v1",
		BaseURLEnv: "STEPFUN_BASE_URL"},
	"minimax": {Transport: "anthropic_messages", BaseURLEnv: "MINIMAX_BASE_URL"},
	"minimax-oauth": {Transport: "anthropic_messages", AuthType: authTypeOAuthExternal,
		BaseURLOverride: "https://api.minimax.io/anthropic"},
	"minimax-cn":          {Transport: "anthropic_messages", BaseURLEnv: "MINIMAX_CN_BASE_URL"},
	"deepseek":            {BaseURLEnv: "DEEPSEEK_BASE_URL"},
	"alibaba":             {BaseURLEnv: "DASHSCOPE_BASE_URL"},
	"alibaba-coding-plan": {BaseURLEnv: "ALIBABA_CODING_PLAN_BASE_URL"},
	"vercel":              {Aggregator: true},
	"opencode":            {Aggregator: true, BaseURLEnv: "OPENCODE_ZEN_BASE_URL"},
	"opencode-go":         {Aggregator: true, BaseURLEnv: "OPENCODE_GO_BASE_URL"},
	"kilo":                {Aggregator: true, BaseURLEnv: "KILOCODE_BASE_URL"},
	"huggingface":         {Aggregator: true, BaseURLEnv: "HF_BASE_URL"},
	"novita":              {Aggregator: true, BaseURLEnv: "NOVITA_BASE_URL"},
	"xai": {Transport: "codex_responses", BaseURLOverride: "https://api.x.ai/v1",
		BaseURLEnv: "XAI_BASE_URL"},
	"nvidia":           {BaseURLOverride: "https://integrate.api.nvidia.com/v1", BaseURLEnv: "NVIDIA_BASE_URL"},
	"xiaomi":           {BaseURLEnv: "XIAOMI_BASE_URL"},
	"tencent-tokenhub": {BaseURLEnv: "TOKENHUB_BASE_URL"},
	"tencent-tokenplan": {Transport: "anthropic_messages",
		BaseURLOverride: "https://api.lkeap.cloud.tencent.com/plan/anthropic", BaseURLEnv: "TOKENPLAN_BASE_URL"},
	"arcee": {BaseURLOverride: "https://api.arcee.ai/api/v1", BaseURLEnv: "ARCEE_BASE_URL"},
	"commandcode": {ExtraEnv: []string{"COMMANDCODE_API_KEY"},
		BaseURLOverride: "https://api.commandcode.ai/provider/v1", BaseURLEnv: "COMMANDCODE_BASE_URL"},
	"gmi": {ExtraEnv: []string{"GMI_API_KEY"}, BaseURLOverride: "https://api.gmi-serving.com/v1",
		BaseURLEnv: "GMI_BASE_URL"},
	"fireworks": {ExtraEnv: []string{"FIREWORKS_API_KEY"},
		BaseURLOverride: "https://api.fireworks.ai/inference/v1"},
	"actual": {Transport: "chat_completions", ExtraEnv: []string{"ACTUAL_API_KEY"},
		BaseURLOverride: "https://api.actual.inc/v1", BaseURLEnv: "ACTUAL_BASE_URL"},
	"upstage": {ExtraEnv: []string{"UPSTAGE_API_KEY"}, BaseURLOverride: "https://api.upstage.ai/v1",
		BaseURLEnv: "UPSTAGE_BASE_URL"},
	"nebius-token-factory": {ExtraEnv: []string{"NEBIUS_API_KEY", "NEBIUS_TOKEN_FACTORY_API_KEY"},
		BaseURLOverride: "https://api.tokenfactory.nebius.com/v1", BaseURLEnv: "NEBIUS_BASE_URL"},
	"ollama-cloud":  {BaseURLOverride: "https://ollama.com/v1", BaseURLEnv: "OLLAMA_BASE_URL"},
	"azure-foundry": {BaseURLEnv: "AZURE_FOUNDRY_BASE_URL"},
	"bedrock":       {Transport: "bedrock_converse", AuthType: authTypeAWSSDK},
	"vertex":        {AuthType: authTypeVertex},
}

var aliasGroups = []struct {
	canonical string
	aliases   []string
}{
	{"openrouter", []string{"openai"}},
	{"zai", []string{"glm", "z-ai", "z.ai", "zhipu"}},
	{"xai", []string{"x-ai", "x.ai", "grok"}},
	{"xai-oauth", []string{"grok-oauth", "xai-oauth", "x-ai-oauth", "xai-grok-oauth"}},
	{"nvidia", []string{"nim", "nvidia-nim", "build-nvidia", "nemotron"}},
	{"kimi-for-coding", []string{"kimi", "kimi-coding", "kimi-coding-cn", "moonshot"}},
	{"stepfun", []string{"step", "stepfun-coding-plan"}},
	{"minimax-cn", []string{"minimax-china", "minimax_cn"}},
	{"anthropic", []string{"claude", "claude-code"}},
	{"openai-codex", []string{"chatgpt", "chatgpt-codex"}},
	{"vercel", []string{"ai-gateway", "aigateway", "vercel-ai-gateway"}},
	{"opencode", []string{"opencode-zen", "zen"}},
	{"opencode-go", []string{"go", "opencode-go-sub"}},
	{"kilo", []string{"kilocode", "kilo-code", "kilo-gateway"}},
	{"deepseek", []string{"deep-seek"}},
	{"alibaba", []string{"dashscope", "aliyun", "qwen", "alibaba-cloud"}},
	{"alibaba-coding-plan", []string{"alibaba_coding", "alibaba-coding", "alibaba_coding_plan"}},
	{"huggingface", []string{"hf", "hugging-face", "huggingface-hub"}},
	{"novita", []string{"novita-ai", "novitaai"}},
	{"xiaomi", []string{"mimo", "xiaomi-mimo"}},
	{"tencent-tokenhub", []string{"tencent", "tokenhub", "tencent-cloud", "tencentmaas"}},
	{"tencent-tokenplan", []string{"tokenplan", "tencent-lkeap"}},
	{"bedrock", []string{"aws", "aws-bedrock", "amazon-bedrock", "amazon"}},
	{"arcee", []string{"arcee-ai", "arceeai"}},
	{"gmi", []string{"gmi-cloud", "gmicloud"}},
	{"fireworks", []string{"fireworks-ai", "fw"}},
	{"upstage", []string{"solar"}},
	{"actual", []string{"actual-computer", "actualcomputer", "aci"}},
	{"nebius-token-factory", []string{"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"}},
	{"lmstudio", []string{"lmstudio", "lm-studio", "lm_studio"}},
	{ProviderCustom, []string{"ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp"}},
}

var labelOverrides = map[string]string{
	"moa": "Mixture of Agents", ProviderPortal: "Icebath Portal", "openai-codex": "ChatGPT or Codex Subscription",
	"stepfun": "StepFun Step Plan", "xiaomi": "Xiaomi MiMo", "gmi": "GMI Cloud",
	"upstage": "Upstage Solar", "actual": "Actual Computer", "tencent-tokenhub": "Tencent TokenHub",
	"nebius-token-factory": "Nebius Token Factory", "tencent-tokenplan": "Tencent TokenPlan", "lmstudio": "LM Studio",
	ProviderCustom: "Custom endpoint", "bedrock": "AWS Bedrock", "vertex": "Google Vertex AI", "ollama-cloud": "Ollama Cloud",
	"xai-oauth": "xAI Grok OAuth (SuperGrok / Premium+)",
}

var transportModes = map[string]wire.Mode{
	"openai_chat": wire.ModeChat, "anthropic_messages": wire.ModeAnthropic,
	"codex_responses": wire.ModeResponses, "bedrock_converse": wire.ModeBedrock,
}

var flatNamespaceResellers = []string{"opencode-go", "opencode"}

var responsesNativeHosts = []string{"api.meta.ai", "api.router.com"}

func Alias(name string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	for _, g := range aliasGroups {
		if slices.Contains(g.aliases, key) {
			return g.canonical, true
		}
	}
	return "", false
}

const defaultOverlayTransport = "openai_chat"

func (o Overlay) Resolved() Overlay {
	if o.Transport == "" {
		o.Transport = defaultOverlayTransport
	}
	if o.AuthType == "" {
		o.AuthType = authTypeAPIKey
	}
	return o
}

func StaticOverlay(canonical string) (Overlay, bool) {
	ov, ok := staticOverlays[canonical]
	return ov.Resolved(), ok
}

func LabelOverride(canonical string) (string, bool) {
	label, ok := labelOverrides[canonical]
	return label, ok
}

func TransportMode(transport string) (wire.Mode, bool) {
	mode, ok := transportModes[transport]
	return mode, ok
}

func ModeTransport(mode wire.Mode) (string, bool) {
	for transport, m := range transportModes {
		if m == mode {
			return transport, true
		}
	}
	return "", false
}

func IsFlatNamespaceReseller(normalized string) bool {
	return slices.Contains(flatNamespaceResellers, normalized)
}

func isStaticCanonical(key string) bool {
	if _, ok := staticOverlays[key]; ok {
		return true
	}
	for _, g := range aliasGroups {
		if g.canonical == key {
			return true
		}
	}
	for _, row := range builtinRows {
		if row.ID == key {
			return true
		}
	}
	return false
}

func overlayKey(s *Spec) string {
	if s.ModelsDevID != "" {
		return s.ModelsDevID
	}
	return s.Name
}

func (r *Registry) overlayProfiles(ctx context.Context) []*Spec {
	specs := []*Spec{}
	for _, p := range r.All(ctx) {
		if s := p.Spec(); s.Overlay != nil {
			specs = append(specs, s)
		}
	}
	return specs
}

func overlayOwner(name string, specs []*Spec) *Spec {
	if name == "" {
		return nil
	}
	for _, s := range specs {
		if name == s.Name || name == s.ModelsDevID || slices.Contains(s.OverlayAliases, name) {
			return s
		}
	}
	return nil
}

func (r *Registry) Overlays(ctx context.Context) map[string]Overlay {
	merged := make(map[string]Overlay, len(staticOverlays))
	for k, v := range staticOverlays {
		merged[k] = v.Resolved()
	}
	for _, s := range r.overlayProfiles(ctx) {
		if _, ok := merged[overlayKey(s)]; !ok {
			merged[overlayKey(s)] = s.Overlay.Resolved()
		}
	}
	return merged
}

func (r *Registry) NormalizeProvider(ctx context.Context, name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := Alias(key); ok {
		return canonical
	}
	if isStaticCanonical(key) {
		return key
	}
	if s := overlayOwner(key, r.overlayProfiles(ctx)); s != nil {
		return overlayKey(s)
	}
	return key
}

func (r *Registry) Overlay(ctx context.Context, canonical string) (Overlay, string, bool) {
	if ov, ok := staticOverlays[canonical]; ok {
		label, found := labelOverrides[canonical]
		if !found {
			label = canonical
		}
		return ov.Resolved(), label, true
	}
	s := overlayOwner(canonical, r.overlayProfiles(ctx))
	if s == nil || overlayKey(s) != canonical {
		return Overlay{}, canonical, false
	}
	label, found := labelOverrides[canonical]
	if !found {
		label = s.DisplayName
		if label == "" {
			label = canonical
		}
	}
	return s.Overlay.Resolved(), label, true
}

func (r *Registry) IsActualRoute(ctx context.Context, provider, baseURL string) bool {
	return r.NormalizeProvider(ctx, provider) == "actual" || URLHostname(baseURL) == "api.actual.inc"
}

func parseBaseURL(raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}

func URLHostname(baseURL string) string {
	u := parseBaseURL(baseURL)
	if u == nil {
		return ""
	}
	return strings.TrimRight(strings.ToLower(u.Hostname()), ".")
}

func URLPath(baseURL string) string {
	u := parseBaseURL(baseURL)
	if u == nil {
		return ""
	}
	return strings.TrimRight(strings.ToLower(u.Path), "/")
}

func HostMatches(baseURL, domain string) bool {
	host := URLHostname(baseURL)
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func IsOfficialOpenAIHost(baseURL string) bool {
	return HostMatches(baseURL, "api.openai.com")
}

func HostMandatedMode(baseURL string) (wire.Mode, bool) {
	if baseURL == "" {
		return "", false
	}
	lower := strings.ToLower(strings.TrimRight(baseURL, "/"))
	host := URLHostname(baseURL)
	switch {
	case host == "api.actual.inc":
		return wire.ModeChat, true
	case host == "api.kimi.com" && strings.Contains(lower, "/coding"):
		return wire.ModeAnthropic, true
	case host == "api.anthropic.com" || strings.HasSuffix(lower, "/anthropic"):
		return wire.ModeAnthropic, true
	case IsOfficialOpenAIHost(baseURL) || slices.Contains(responsesNativeHosts, host):
		return wire.ModeResponses, true
	case strings.HasPrefix(host, "bedrock-runtime.") && HostMatches(baseURL, "amazonaws.com"):
		return wire.ModeBedrock, true
	}
	return "", false
}

func PortalAnthropicWire(cfg *config.Config) config.AnthropicWire {
	if cfg == nil {
		return config.WireChat
	}
	value := config.AnthropicWire(strings.ToLower(strings.TrimSpace(string(cfg.Portal.AnthropicWire))))
	if value == config.WireNative || value == config.WireAuto {
		return value
	}
	return config.WireChat
}

func PortalMode(model string, w config.AnthropicWire) wire.Mode {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic/") && w == config.WireNative {
		return wire.ModeAnthropic
	}
	return wire.ModeChat
}
