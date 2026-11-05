package provider

import "strings"

// Canonical default endpoints shared by resolution and the profile table
const (
	OpenRouterBaseURL     = "https://openrouter.ai/api/v1"
	CodexBaseURL          = "https://chatgpt.com/backend-api/codex"
	OpenAIBaseURL         = "https://api.openai.com/v1"
	AnthropicBaseURL      = "https://api.anthropic.com"
	NousInferenceBaseURL  = "https://inference-api.nousresearch.com/v1"
	NousPortalBaseURL     = "https://portal.nousresearch.com"
	QwenBaseURL           = "https://portal.qwen.ai/v1"
	XAIBaseURL            = "https://api.x.ai/v1"
	LMStudioBaseURL       = "http://127.0.0.1:1234/v1"
	OllamaCloudBaseURL    = "https://ollama.com/v1"
	ActualBaseURL         = "https://api.actual.inc/v1"
	StepFunIntlBaseURL    = "https://api.stepfun.ai/step_plan/v1"
	MiniMaxOAuthBaseURL   = "https://api.minimax.io/anthropic"
	MiniMaxOAuthCNBaseURL = "https://api.minimaxi.com/anthropic"
	RouterBaseURL         = "https://api.router.com/v1"
	MoaBaseURL            = "moa://local"
	MoaAPIKey             = "moa-virtual-provider"
	NoKeyPlaceholder      = "no-key-required"
)

// Human friendly and legacy aliases grouped by canonical id with the static table beating plugin aliases
var staticAliasGroups = map[string][]string{
	"openrouter":             {"openai", "or"},
	"zai":                    {"glm", "z-ai", "z.ai", "zhipu"},
	"xai":                    {"x-ai", "x.ai", "grok"},
	"xai-oauth":              {"grok-oauth", "x-ai-oauth", "xai-grok-oauth"},
	"nvidia":                 {"nim", "nvidia-nim", "build-nvidia", "nemotron"},
	"kimi-coding":            {"kimi", "moonshot", "kimi-for-coding"},
	"kimi-coding-cn":         {"kimi-cn", "moonshot-cn"},
	"stepfun":                {"step", "stepfun-coding-plan"},
	"minimax":                {"mini-max"},
	"minimax-cn":             {"minimax-china", "minimax_cn"},
	"minimax-oauth":          {"minimax_oauth", "minimax-portal", "minimax-global", "minimax-oauth-io"},
	"anthropic":              {"claude", "claude-code", "claude-oauth"},
	"openai-codex":           {"chatgpt", "chatgpt-codex", "codex", "openai_codex"},
	"ai-gateway":             {"vercel", "aigateway", "vercel-ai-gateway", "ai_gateway"},
	"opencode-zen":           {"opencode", "zen", "opencode_zen"},
	"opencode-go":            {"go", "opencode-go-sub", "opencode_go"},
	"kilocode":               {"kilo", "kilo-code", "kilo-gateway"},
	"deepseek":               {"deep-seek", "deepseek-chat"},
	"alibaba":                {"dashscope", "aliyun", "alibaba-cloud", "qwen-dashscope"},
	"alibaba-cn":             {"dashscope-cn", "alibaba-cloud-cn"},
	"alibaba-coding-plan":    {"alibaba_coding", "alibaba-coding", "alibaba_coding_plan", "dashscope-coding"},
	"alibaba-coding-plan-cn": {"alibaba-coding-cn", "dashscope-coding-cn"},
	"alibaba-token-plan":     {"dashscope-token-plan"},
	"alibaba-token-plan-cn":  {"dashscope-token-plan-cn"},
	"huggingface":            {"hf", "hugging-face", "huggingface-hub"},
	"novita":                 {"novita-ai", "novitaai"},
	"xiaomi":                 {"mimo", "xiaomi-mimo"},
	"tencent-tokenhub":       {"tencent", "tokenhub", "tencent-cloud", "tencentmaas"},
	"tencent-tokenplan":      {"tokenplan", "tencent-lkeap"},
	"bedrock":                {"aws", "aws-bedrock", "amazon-bedrock", "amazon"},
	"arcee":                  {"arcee-ai", "arceeai"},
	"gmi":                    {"gmi-cloud", "gmicloud"},
	"fireworks":              {"fireworks-ai", "fw"},
	"upstage":                {"solar"},
	"actual":                 {"actual-computer", "actualcomputer", "aci"},
	"nebius-token-factory":   {"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"},
	"lmstudio":               {"lm-studio", "lm_studio"},
	"custom":                 {"ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp"},
	"nous":                   {"nous-portal", "nousresearch"},
	"qwen-oauth":             {"qwen", "qwen-portal", "qwen-cli"},
	"gemini":                 {"google", "google-gemini", "google-ai-studio"},
	"vertex":                 {"google-vertex", "vertex-ai", "gcp-vertex", "vertexai"},
	"azure-foundry":          {"azure", "azure-ai-foundry", "azure-ai"},
	"meta-ai":                {"meta", "muse", "muse-spark", "model-api", "msl"},
	"router":                 {"ramp-router", "ramp", "router.com"},
	"commandcode":            {"commandcode-chat"},
	"commandcode-anthropic":  {"commandcode-claude"},
	"deepinfra":              {"deep-infra", "deepinfra-ai"},
	"solstice":               {"solstice-oauth"},
	"ollama-cloud":           {"ollama_cloud"},
}

// Model families Azure Foundry serves only through the Responses API
var azureFoundryResponsesPrefixes = []string{"gpt-5", "codex", "gpt-codex", "o1", "o3", "o4"}

// AzureFoundryModelMode returns codex_responses for Responses only Azure deployments or empty
func AzureFoundryModelMode(model string) APIMode {
	raw := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	if raw == "" {
		return ""
	}
	for _, p := range azureFoundryResponsesPrefixes {
		if strings.HasPrefix(raw, p) {
			return ModeCodexResponses
		}
	}
	return ""
}

var opencodeFamilies = []string{"opencode-go", "opencode-zen"}

// OpenCodeFamily resolves a provider id or a custom name extending a family slug to its OpenCode family
func OpenCodeFamily(providerID string) string {
	raw := strings.ToLower(strings.TrimSpace(providerID))
	if raw == "" {
		return ""
	}
	canon := NormalizeProvider(providerID)
	for _, f := range opencodeFamilies {
		if canon == f {
			return f
		}
	}
	for _, f := range opencodeFamilies {
		if strings.HasPrefix(raw, f) {
			return f
		}
	}
	return ""
}

// NormalizeOpenCodeModelID strips the family prefix from an OpenCode config model id
func NormalizeOpenCodeModelID(providerID, modelID string) string {
	family := OpenCodeFamily(providerID)
	current := strings.TrimSpace(modelID)
	if current == "" || family == "" {
		return current
	}
	for _, prefix := range []string{providerID + "/", family + "/"} {
		if prefix != "/" && strings.HasPrefix(strings.ToLower(current), strings.ToLower(prefix)) {
			return current[len(prefix):]
		}
	}
	return current
}

type prefixMode struct {
	prefixes []string
	mode     APIMode
}

var opencodeModePrefixes = map[string][]prefixMode{
	"opencode-go": {
		{[]string{"gpt-", "grok-", "muse-spark"}, ModeCodexResponses},
		{[]string{"claude-", "minimax-", "qwen", "union-alpha"}, ModeAnthropicMessages},
	},
	"opencode-zen": {
		{[]string{"claude-", "union-alpha"}, ModeAnthropicMessages},
		{[]string{"gpt-", "grok-", "muse-spark"}, ModeCodexResponses},
		{[]string{"qwen"}, ModeAnthropicMessages},
	},
}

// OpenCodeModelMode picks the wire protocol an OpenCode Zen or Go model is served on
func OpenCodeModelMode(providerID, modelID string) APIMode {
	family := OpenCodeFamily(providerID)
	normalized := strings.ToLower(NormalizeOpenCodeModelID(providerID, modelID))
	if normalized != "" {
		for _, pm := range opencodeModePrefixes[family] {
			for _, p := range pm.prefixes {
				if strings.HasPrefix(normalized, p) {
					return pm.mode
				}
			}
		}
	}
	return ModeChatCompletions
}

var opencodeFamilyPaths = map[string]string{"opencode-zen": "/zen", "opencode-go": "/zen/go"}

// NormalizeOpenCodeBaseURL heals the relay path and the /v1 tail for the wire protocol on opencode.ai hosts
func NormalizeOpenCodeBaseURL(providerID string, mode APIMode, baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	family := OpenCodeFamily(providerID)
	if u == "" || family == "" {
		return u
	}
	parsed, err := parseURL(u)
	if err != nil {
		return u
	}
	host := strings.ToLower(parsed.Hostname())
	official := host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
	path := strings.TrimRight(parsed.Path, "/")
	builtin := false
	for _, f := range opencodeFamilies {
		if NormalizeProvider(providerID) == f {
			builtin = true
		}
	}
	if official && builtin && isOpenCodeRelayPath(path) {
		path = opencodeFamilyPaths[family]
		if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/v1") {
			path += "/v1"
		}
	}
	if mode == ModeAnthropicMessages {
		path = strings.TrimSuffix(path, "/v1")
	} else if official && !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	parsed.Path = path
	return parsed.String()
}

func isOpenCodeRelayPath(path string) bool {
	switch path {
	case "/zen", "/zen/go", "/zen/v1", "/zen/go/v1":
		return true
	}
	return false
}

// Compact row shared by the registry derived and plugin derived profile entries
type profileRow struct {
	name, display, desc, signup, base, baseEnv, modelsURL string
	mode                                                  APIMode
	authType                                              string
	keys                                                  []string
	defModel                                              string
	fallbacks                                             []string
	headers                                               map[string]string
	aggregator, hidden, noListing, noHealth               bool
	vision, noVisionTool, omitTemp                        bool
	maxTokens                                             int
	badFormats                                            []string
	modelsDev                                             string
	hostnames                                             []string
	perModel                                              func(string) APIMode
	local                                                 bool
}

func (r profileRow) profile() Profile {
	p := Profile{
		Name: r.name, DisplayName: r.display, Description: r.desc, SignupURL: r.signup,
		BaseURL: r.base, BaseURLEnv: r.baseEnv, ModelsURL: r.modelsURL, Mode: r.mode, AuthType: r.authType,
		KeyEnv: r.keys, DefaultModel: r.defModel, FallbackModels: r.fallbacks, Headers: r.headers,
		Local: r.local, Aggregator: r.aggregator, Hidden: r.hidden,
		ModelListing: !r.noListing, HealthCheck: !r.noHealth, Vision: r.vision, VisionToolMessages: !r.noVisionTool,
		OmitTemperature: r.omitTemp, DefaultMaxTokens: r.maxTokens, UnsupportedResponseFormats: r.badFormats,
		ModelsDevID: r.modelsDev, Hostnames: r.hostnames, PerModelMode: r.perModel,
	}
	if p.ModelsDevID == "" {
		p.ModelsDevID = p.Name
	}
	if p.ModelsURL == "" && p.BaseURL != "" && p.ModelListing {
		p.ModelsURL = strings.TrimRight(p.BaseURL, "/") + "/models"
	}
	return p
}

// Registry rows in the auto detection priority order hermes scans api key providers
func registryRows() []profileRow {
	return []profileRow{
		{name: "nous", display: "Nous Portal", desc: "Nous Research - Hermes model family", signup: "https://nousresearch.com/",
			base: NousInferenceBaseURL, authType: AuthTypeOAuthDeviceCode, keys: []string{"NOUS_API_KEY"},
			fallbacks: []string{"hermes-3-405b", "hermes-3-70b"}, perModel: func(m string) APIMode { return NousMode(m, "chat") }},
		{name: "openai-codex", display: "ChatGPT or Codex Subscription", base: CodexBaseURL, baseEnv: "ICEBATH_CODEX_BASE_URL",
			mode: ModeCodexResponses, authType: AuthTypeOAuthExternal, noListing: true},
		{name: "openai-api", display: "OpenAI API", base: OpenAIBaseURL, baseEnv: "OPENAI_BASE_URL", mode: ModeCodexResponses,
			keys: []string{"OPENAI_API_KEY"}, modelsDev: "openai"},
		{name: "xai-oauth", display: "xAI Grok OAuth (SuperGrok / Premium+)", base: XAIBaseURL, baseEnv: "XAI_BASE_URL",
			mode: ModeCodexResponses, authType: AuthTypeOAuthExternal},
		{name: "qwen-oauth", display: "Qwen OAuth", base: QwenBaseURL, baseEnv: "ICEBATH_QWEN_BASE_URL", authType: AuthTypeOAuthExternal,
			keys: []string{"QWEN_API_KEY"}, maxTokens: 65536},
		{name: "lmstudio", display: "LM Studio", base: LMStudioBaseURL, baseEnv: "LM_BASE_URL", keys: []string{"LM_API_KEY"},
			noListing: true, noHealth: true, local: true},
		{name: "gemini", display: "Google AI Studio", base: "https://generativelanguage.googleapis.com/v1beta", baseEnv: "GEMINI_BASE_URL",
			keys: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, defModel: "gemini-3.6-flash", modelsDev: "google"},
		{name: "zai", display: "Z.AI (GLM)", desc: "Z.AI / GLM - Zhipu AI models", signup: "https://z.ai/", base: "https://api.z.ai/api/paas/v4",
			baseEnv: "GLM_BASE_URL", keys: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"},
			fallbacks: []string{"glm-5.2", "glm-5", "glm-4-9b"}, defModel: "glm-4.5-flash"},
		{name: "kimi-coding", display: "Kimi / Moonshot", base: "https://api.moonshot.ai/v1", baseEnv: "KIMI_BASE_URL",
			keys: []string{"KIMI_API_KEY", "KIMI_CODING_API_KEY"}, omitTemp: true, maxTokens: 32000,
			headers: kimiHeaders(), defModel: "kimi-k2-turbo-preview", modelsDev: "kimi-for-coding"},
		{name: "kimi-coding-cn", display: "Kimi / Moonshot (China)", base: "https://api.moonshot.cn/v1", keys: []string{"KIMI_CN_API_KEY"},
			omitTemp: true, maxTokens: 32000, headers: kimiHeaders(), defModel: "kimi-k2-turbo-preview", modelsDev: "kimi-for-coding"},
		{name: "stepfun", display: "StepFun Step Plan", base: StepFunIntlBaseURL, baseEnv: "STEPFUN_BASE_URL",
			keys: []string{"STEPFUN_API_KEY"}, defModel: "step-3.5-flash"},
		{name: "arcee", display: "Arcee AI", base: "https://api.arcee.ai/api/v1", baseEnv: "ARCEE_BASE_URL", keys: []string{"ARCEEAI_API_KEY"}},
		{name: "gmi", display: "GMI Cloud", desc: "GMI Cloud - multi-model direct API (slash-form model IDs)", signup: "https://www.gmicloud.ai/",
			base: "https://api.gmi-serving.com/v1", baseEnv: "GMI_BASE_URL", keys: []string{"GMI_API_KEY"},
			headers: map[string]string{"User-Agent": UserAgent("IcebathAgent")}, defModel: "google/gemini-3.1-flash-lite-preview",
			fallbacks: []string{"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5", "google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4"}},
		{name: "actual", display: "Actual Computer", desc: "Actual Computer - hosted inference via api.actual.inc, or local offline inference via model.base_url in config.yaml",
			signup: "https://actual.inc", base: ActualBaseURL, baseEnv: "ACTUAL_BASE_URL", keys: []string{"ACTUAL_API_KEY"}},
		{name: "minimax", display: "MiniMax", base: MiniMaxOAuthBaseURL, baseEnv: "MINIMAX_BASE_URL", mode: ModeAnthropicMessages,
			keys: []string{"MINIMAX_API_KEY"}, defModel: "MiniMax-M3"},
		{name: "minimax-oauth", display: "MiniMax (OAuth)", desc: "MiniMax via OAuth browser flow - no API key required", signup: "https://api.minimax.io/",
			base: MiniMaxOAuthBaseURL, mode: ModeAnthropicMessages, authType: AuthTypeOAuthMiniMax, defModel: "MiniMax-M2.7"},
		{name: "anthropic", display: "Anthropic", base: AnthropicBaseURL, baseEnv: "ANTHROPIC_BASE_URL", mode: ModeAnthropicMessages,
			keys: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}, defModel: "claude-haiku-4-5-20251001"},
		{name: "alibaba", display: "Qwen Cloud", base: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", baseEnv: "DASHSCOPE_BASE_URL",
			keys: []string{"DASHSCOPE_API_KEY"}},
		{name: "alibaba-coding-plan", display: "Alibaba Cloud (Coding Plan)", desc: "Alibaba Cloud Coding Plan (Dedicated coding tier)",
			signup: "https://help.aliyun.com/zh/model-studio/", base: "https://coding-intl.dashscope.aliyuncs.com/v1",
			baseEnv: "ALIBABA_CODING_PLAN_BASE_URL", keys: []string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY"}},
		{name: "minimax-cn", display: "MiniMax (China)", base: MiniMaxOAuthCNBaseURL, baseEnv: "MINIMAX_CN_BASE_URL", mode: ModeAnthropicMessages,
			keys: []string{"MINIMAX_CN_API_KEY"}, defModel: "MiniMax-M3"},
		{name: "deepseek", display: "DeepSeek", desc: "DeepSeek - native DeepSeek API", signup: "https://platform.deepseek.com/",
			base: "https://api.deepseek.com/v1", baseEnv: "DEEPSEEK_BASE_URL", keys: []string{"DEEPSEEK_API_KEY"},
			fallbacks: []string{"deepseek-v4-pro", "deepseek-flash"}, defModel: "deepseek-flash", badFormats: []string{"json_schema"}},
		{name: "xai", display: "xAI", base: XAIBaseURL, baseEnv: "XAI_BASE_URL", mode: ModeCodexResponses, keys: []string{"XAI_API_KEY"},
			headers: map[string]string{"User-Agent": UserAgent("Icebath-Agent")}},
		{name: "nvidia", display: "NVIDIA NIM", desc: "NVIDIA NIM - accelerated inference", signup: "https://build.nvidia.com/",
			base: "https://integrate.api.nvidia.com/v1", baseEnv: "NVIDIA_BASE_URL", keys: []string{"NVIDIA_API_KEY"},
			fallbacks: []string{"nvidia/llama-3.1-nemotron-70b-instruct", "nvidia/llama-3.3-70b-instruct"}, maxTokens: 16384},
		{name: "ai-gateway", display: "Vercel AI Gateway", base: "https://ai-gateway.vercel.sh/v1", baseEnv: "AI_GATEWAY_BASE_URL",
			keys: []string{"AI_GATEWAY_API_KEY"}, aggregator: true, headers: attributionHeaders("IcebathAgent"),
			defModel: "google/gemini-3-flash", modelsDev: "vercel"},
		{name: "opencode-zen", display: "OpenCode Zen", base: "https://opencode.ai/zen/v1", baseEnv: "OPENCODE_ZEN_BASE_URL",
			keys: []string{"OPENCODE_ZEN_API_KEY"}, aggregator: true, headers: attributionHeaders("IcebathAgent"),
			defModel: "gemini-3-flash", modelsDev: "opencode", perModel: func(m string) APIMode { return OpenCodeModelMode("opencode-zen", m) }},
		{name: "opencode-go", display: "OpenCode Go", base: "https://opencode.ai/zen/go/v1", baseEnv: "OPENCODE_GO_BASE_URL",
			keys: []string{"OPENCODE_GO_API_KEY"}, aggregator: true, headers: attributionHeaders("IcebathAgent"),
			defModel: "glm-5", noVisionTool: true, perModel: func(m string) APIMode { return OpenCodeModelMode("opencode-go", m) }},
		{name: "kilocode", display: "Kilo Code", base: "https://api.kilo.ai/api/gateway", baseEnv: "KILOCODE_BASE_URL",
			keys: []string{"KILOCODE_API_KEY"}, aggregator: true, defModel: "google/gemini-3.6-flash", modelsDev: "kilo"},
		{name: "huggingface", display: "Hugging Face", desc: "HuggingFace Inference API", signup: "https://huggingface.co/settings/tokens",
			base: "https://router.huggingface.co/v1", baseEnv: "HF_BASE_URL", keys: []string{"HF_TOKEN"}, aggregator: true,
			fallbacks: []string{"Qwen/Qwen3.5-72B-Instruct", "deepseek-ai/DeepSeek-V3.2"}},
		{name: "xiaomi", display: "Xiaomi MiMo", base: "https://api.xiaomimimo.com/v1", baseEnv: "XIAOMI_BASE_URL", keys: []string{"XIAOMI_API_KEY"},
			noHealth: true, vision: true, noVisionTool: true},
		{name: "tencent-tokenhub", display: "Tencent TokenHub", base: "https://tokenhub.tencentmaas.com/v1", baseEnv: "TOKENHUB_BASE_URL",
			keys: []string{"TOKENHUB_API_KEY"}, noHealth: true},
		{name: "tencent-tokenplan", display: "Tencent TokenPlan", base: "https://api.lkeap.cloud.tencent.com/plan/anthropic", baseEnv: "TOKENPLAN_BASE_URL",
			mode: ModeAnthropicMessages, keys: []string{"TOKENPLAN_API_KEY"}},
		{name: "ollama-cloud", display: "Ollama Cloud", base: OllamaCloudBaseURL, baseEnv: "OLLAMA_BASE_URL", keys: []string{"OLLAMA_API_KEY"},
			defModel: "nemotron-3-nano:30b"},
		{name: "bedrock", display: "AWS Bedrock", base: "https://bedrock-runtime.us-east-1.amazonaws.com", baseEnv: "BEDROCK_BASE_URL",
			mode: modeBedrockConverse, authType: AuthTypeAWSSDK, noListing: true},
		{name: "vertex", display: "Google Vertex AI", base: "https://aiplatform.googleapis.com", authType: AuthTypeVertex,
			defModel: "google/gemini-3.6-flash", noListing: true},
		{name: "azure-foundry", display: "Azure Foundry", desc: "Microsoft Foundry - OpenAI-compatible endpoint (user-supplied base URL)",
			signup: "https://ai.azure.com/", baseEnv: "AZURE_FOUNDRY_BASE_URL", keys: []string{"AZURE_FOUNDRY_API_KEY"},
			perModel: AzureFoundryModelMode},
	}
}

func kimiHeaders() map[string]string {
	h := attributionHeaders("IcebathAgent")
	h["Accept-Encoding"] = "gzip"
	return h
}
