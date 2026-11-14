package providers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
)

type Row struct {
	ID               string
	Name             string
	AuthType         string
	PortalBaseURL    string
	InferenceBaseURL string
	ClientID         string
	Scope            string
	Extra            map[string]string
	APIKeyEnvVars    []string
	BaseURLEnvVar    string
	Mirrored         bool
}

func keyRow(id, name, baseURL string, envVars []string, urlEnv string) Row {
	return Row{ID: id, Name: name, AuthType: authTypeAPIKey, InferenceBaseURL: baseURL, APIKeyEnvVars: envVars, BaseURLEnvVar: urlEnv}
}

var builtinRows = []Row{
	{ID: ProviderPortal, Name: "Icebath Portal", AuthType: authTypeOAuthDeviceCode, PortalBaseURL: auth.DefaultPortalURL,
		InferenceBaseURL: auth.DefaultPortalInferenceURL, ClientID: auth.DefaultPortalClientID, Scope: auth.DefaultPortalScope},
	{ID: "openai-codex", Name: "OpenAI Codex", AuthType: authTypeOAuthExternal, InferenceBaseURL: auth.DefaultCodexBaseURL},
	keyRow("openai-api", "OpenAI API", "https://api.openai.com/v1", []string{"OPENAI_API_KEY"}, "OPENAI_BASE_URL"),
	{ID: "xai-oauth", Name: "xAI Grok OAuth (SuperGrok / Premium+)", AuthType: authTypeOAuthExternal, InferenceBaseURL: auth.DefaultXAIOAuthBaseURL},
	{ID: "qwen-oauth", Name: "Qwen OAuth", AuthType: authTypeOAuthExternal, InferenceBaseURL: auth.DefaultQwenBaseURL},
	keyRow("lmstudio", "LM Studio", "http://127.0.0.1:1234/v1", []string{"LM_API_KEY"}, "LM_BASE_URL"),
	keyRow("gemini", "Google AI Studio", "https://generativelanguage.googleapis.com/v1beta", []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, "GEMINI_BASE_URL"),
	keyRow("zai", "Z.AI / GLM", "https://api.z.ai/api/paas/v4", []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, "GLM_BASE_URL"),
	keyRow("kimi-coding", "Kimi / Moonshot", "https://api.moonshot.ai/v1", []string{"KIMI_API_KEY", "KIMI_CODING_API_KEY"}, "KIMI_BASE_URL"),
	keyRow("kimi-coding-cn", "Kimi / Moonshot (China)", "https://api.moonshot.cn/v1", []string{"KIMI_CN_API_KEY"}, ""),
	keyRow("stepfun", "StepFun Step Plan", auth.StepFunStepPlanIntlBaseURL, []string{"STEPFUN_API_KEY"}, "STEPFUN_BASE_URL"),
	keyRow("arcee", "Arcee AI", "https://api.arcee.ai/api/v1", []string{"ARCEEAI_API_KEY"}, "ARCEE_BASE_URL"),
	keyRow("gmi", "GMI Cloud", "https://api.gmi-serving.com/v1", []string{"GMI_API_KEY"}, "GMI_BASE_URL"),
	keyRow("actual", "Actual Computer", auth.DefaultActualBaseURL, []string{"ACTUAL_API_KEY"}, "ACTUAL_BASE_URL"),
	keyRow("minimax", "MiniMax", "https://api.minimax.io/anthropic", []string{"MINIMAX_API_KEY"}, "MINIMAX_BASE_URL"),
	{ID: "minimax-oauth", Name: "MiniMax (OAuth · minimax.io)", AuthType: authTypeOAuthMiniMax,
		PortalBaseURL: auth.MiniMaxOAuthGlobalBase, InferenceBaseURL: auth.MiniMaxOAuthGlobalInference,
		ClientID: auth.MiniMaxOAuthClientID, Scope: auth.MiniMaxOAuthScope,
		Extra: map[string]string{"region": "global", "cn_portal_base_url": auth.MiniMaxOAuthCNBase,
			"cn_inference_base_url": auth.MiniMaxOAuthCNInference}},
	keyRow("anthropic", "Anthropic", "https://api.anthropic.com", []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}, "ANTHROPIC_BASE_URL"),
	keyRow("alibaba", "Qwen Cloud", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", []string{"DASHSCOPE_API_KEY"}, "DASHSCOPE_BASE_URL"),
	keyRow("alibaba-coding-plan", "Alibaba Cloud (Coding Plan)", "https://coding-intl.dashscope.aliyuncs.com/v1",
		[]string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY"}, "ALIBABA_CODING_PLAN_BASE_URL"),
	keyRow("minimax-cn", "MiniMax (China)", "https://api.minimaxi.com/anthropic", []string{"MINIMAX_CN_API_KEY"}, "MINIMAX_CN_BASE_URL"),
	keyRow("deepseek", "DeepSeek", "https://api.deepseek.com/v1", []string{"DEEPSEEK_API_KEY"}, "DEEPSEEK_BASE_URL"),
	keyRow("xai", "xAI", "https://api.x.ai/v1", []string{"XAI_API_KEY"}, "XAI_BASE_URL"),
	keyRow("nvidia", "NVIDIA NIM", "https://integrate.api.nvidia.com/v1", []string{"NVIDIA_API_KEY"}, "NVIDIA_BASE_URL"),
	keyRow("ai-gateway", "Vercel AI Gateway", "https://ai-gateway.vercel.sh/v1", []string{"AI_GATEWAY_API_KEY"}, "AI_GATEWAY_BASE_URL"),
	keyRow("opencode-zen", "OpenCode Zen", "https://opencode.ai/zen/v1", []string{"OPENCODE_ZEN_API_KEY"}, "OPENCODE_ZEN_BASE_URL"),
	keyRow("opencode-go", "OpenCode Go", "https://opencode.ai/zen/go/v1", []string{"OPENCODE_GO_API_KEY"}, "OPENCODE_GO_BASE_URL"),
	keyRow("kilocode", "Kilo Code", "https://api.kilo.ai/api/gateway", []string{"KILOCODE_API_KEY"}, "KILOCODE_BASE_URL"),
	keyRow("huggingface", "Hugging Face", "https://router.huggingface.co/v1", []string{"HF_TOKEN"}, "HF_BASE_URL"),
	keyRow("xiaomi", "Xiaomi MiMo", "https://api.xiaomimimo.com/v1", []string{"XIAOMI_API_KEY"}, "XIAOMI_BASE_URL"),
	keyRow("tencent-tokenhub", "Tencent TokenHub", "https://tokenhub.tencentmaas.com/v1", []string{"TOKENHUB_API_KEY"}, "TOKENHUB_BASE_URL"),
	keyRow("tencent-tokenplan", "Tencent TokenPlan", "https://api.lkeap.cloud.tencent.com/plan/anthropic", []string{"TOKENPLAN_API_KEY"}, "TOKENPLAN_BASE_URL"),
	keyRow("ollama-cloud", "Ollama Cloud", auth.DefaultOllamaCloudBaseURL, []string{"OLLAMA_API_KEY"}, "OLLAMA_BASE_URL"),
	{ID: "bedrock", Name: "AWS Bedrock", AuthType: authTypeAWSSDK, InferenceBaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com", BaseURLEnvVar: "BEDROCK_BASE_URL"},
	{ID: "vertex", Name: "Google Vertex AI", AuthType: authTypeVertex},
	keyRow("azure-foundry", "Azure Foundry", "", []string{"AZURE_FOUNDRY_API_KEY"}, "AZURE_FOUNDRY_BASE_URL"),
}

var registrySkip = []string{"kimi-coding", "kimi-coding-cn", "zai", "openrouter", ProviderCustom}

var PluginAuthActions = []string{"add", "status", "logout", "refresh"}

func IsBuiltinRow(id string) bool {
	return slices.ContainsFunc(builtinRows, func(r Row) bool { return r.ID == id })
}

func registryAuthType(s *Spec) string {
	if s.RegistryAuthType != "" {
		return s.RegistryAuthType
	}
	return s.AuthTypeOrDefault()
}

func apiKeyEnvFields(s *Spec) ([]string, string) {
	keys := s.KeyEnvVars()
	if len(keys) == 0 {
		keys = slices.Clone(s.EnvVars)
	}
	return keys, s.URLEnvVar()
}

func profileRow(s *Spec) (Row, bool) {
	if slices.Contains(registrySkip, s.Name) {
		return Row{}, false
	}
	authType := registryAuthType(s)
	if authType == authTypeAPIKey {
		if len(s.EnvVars) == 0 {
			return Row{}, false
		}
		keys, urlVar := apiKeyEnvFields(s)
		env := s.BaseURLEnv
		if env == "" {
			env = urlVar
		}
		return Row{ID: s.Name, Name: s.Label(), AuthType: authTypeAPIKey, InferenceBaseURL: s.BaseURL, APIKeyEnvVars: keys, BaseURLEnvVar: env, Mirrored: true}, true
	}
	return Row{ID: s.Name, Name: s.Label(), AuthType: authType, InferenceBaseURL: s.BaseURL, BaseURLEnvVar: s.BaseURLEnv, Mirrored: true}, true
}

func overrideRow(row Row, s *Spec) Row {
	if s.DisplayName != "" {
		row.Name = s.DisplayName
	}
	if s.BaseURL != "" {
		row.InferenceBaseURL = s.BaseURL
	}
	if registryAuthType(s) == authTypeAPIKey && row.AuthType == authTypeAPIKey && len(s.EnvVars) > 0 {
		keys, urlVar := apiKeyEnvFields(s)
		row.APIKeyEnvVars = keys
		if urlVar != "" {
			row.BaseURLEnvVar = urlVar
		}
	}
	return row
}

type rowView struct {
	gen   uint64
	order []string
	rows  map[string]Row
	keys  map[string]string
}

func (v *rowView) insert(row Row, after string) {
	v.rows[row.ID] = row
	v.keys[row.ID] = row.ID
	if at := slices.Index(v.order, after); after != "" && at >= 0 {
		v.order = slices.Insert(v.order, at+1, row.ID)
		return
	}
	v.order = append(v.order, row.ID)
}

func (r *Registry) rowView(ctx context.Context) *rowView {
	l := r.homeLayer(ctx, false)
	gen := r.Generation()
	r.mu.RLock()
	cached := r.rowViews[l.key]
	r.mu.RUnlock()
	if cached != nil && cached.gen == gen {
		return cached
	}
	v := &rowView{gen: gen, rows: map[string]Row{}, keys: map[string]string{}}
	for _, row := range builtinRows {
		v.order = append(v.order, row.ID)
		v.rows[row.ID] = row
		v.keys[row.ID] = row.ID
	}
	r.mu.RLock()
	shared := make([]Profile, 0, len(r.sharedOrder))
	for _, name := range r.sharedOrder {
		shared = append(shared, r.shared[name])
	}
	user := make([]Profile, 0, len(l.order))
	for _, name := range l.order {
		user = append(user, l.profiles[name])
	}
	userAliases := make(map[string]string, len(l.aliases))
	for k, val := range l.aliases {
		userAliases[k] = val
	}
	r.mu.RUnlock()
	for _, p := range shared {
		s := p.Spec()
		if _, ok := v.keys[s.Name]; ok {
			continue
		}
		row, ok := profileRow(s)
		if !ok {
			continue
		}
		v.insert(row, s.RegistryAfter)
		for _, alias := range s.Aliases {
			if _, taken := v.keys[alias]; !taken {
				v.keys[alias] = row.ID
			}
		}
	}
	for _, p := range user {
		s := p.Spec()
		if id, ok := v.keys[s.Name]; ok && id == s.Name {
			row := v.rows[s.Name]
			if !slices.Contains(registrySkip, s.Name) && (row.Mirrored || IsBuiltinRow(s.Name)) {
				v.rows[s.Name] = overrideRow(row, s)
				for _, alias := range s.Aliases {
					if _, taken := v.keys[alias]; !taken || userAliases[alias] == s.Name {
						v.keys[alias] = s.Name
					}
				}
			}
			continue
		}
		row, ok := profileRow(s)
		if !ok {
			continue
		}
		v.insert(row, s.RegistryAfter)
		for _, alias := range s.Aliases {
			if _, taken := v.keys[alias]; !taken || userAliases[alias] == s.Name {
				v.keys[alias] = row.ID
			}
		}
	}
	r.mu.Lock()
	r.rowViews[l.key] = v
	r.mu.Unlock()
	return v
}

func (r *Registry) RegistryRow(ctx context.Context, id string) (Row, bool) {
	if r.safe {
		row, ok := builtinRowByID(id)
		return row, ok
	}
	v := r.rowView(ctx)
	target, ok := v.keys[id]
	if !ok {
		return Row{}, false
	}
	row, ok := v.rows[target]
	return row, ok
}

func builtinRowByID(id string) (Row, bool) {
	for _, row := range builtinRows {
		if row.ID == id {
			return row, true
		}
	}
	return Row{}, false
}

func (r *Registry) RegistryRows(ctx context.Context) []Row {
	if r.safe {
		return slices.Clone(builtinRows)
	}
	v := r.rowView(ctx)
	rows := make([]Row, 0, len(v.order))
	for _, id := range v.order {
		rows = append(rows, v.rows[id])
	}
	return rows
}

var leftCoreProviders = []string{"copilot", "github-copilot", "github-models", "github-model", "github",
	"copilot-acp", "github-copilot-acp", "copilot-acp-agent"}

const (
	leftCorePlugin = "github-copilot"
	leftCoreLabel  = "GitHub Copilot"
)

func (r *Registry) LeftCore(ctx context.Context, name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || !slices.Contains(leftCoreProviders, key) {
		return false
	}
	_, ok := r.Get(ctx, key)
	return !ok
}

type ModelsDevInfo struct {
	Name string
	Env  []string
	API  string
	Doc  string
}

type CatalogHooks interface {
	ModelsDevProvider(ctx context.Context, canonical string) (ModelsDevInfo, bool)
	ClearModelsCache(ctx context.Context, provider string)
}

type PoolSource interface {
	Pool(ctx context.Context, provider string) (*auth.Pool, error)
}

type DirectoryOptions struct {
	Registry *Registry
	Pools    PoolSource
	Catalog  CatalogHooks
	Config   *config.Store
	Homes    *home.Resolver
}

type Directory struct {
	reg     *Registry
	pools   PoolSource
	catalog CatalogHooks
	cfg     *config.Store
	homes   *home.Resolver
	mu      sync.Mutex
}

var ErrDirectoryOptions = errors.New("providers: directory needs a registry, a pool source, catalog hooks, a config store and a home resolver")

func NewDirectory(o DirectoryOptions) (*Directory, error) {
	if o.Registry == nil || o.Pools == nil || o.Catalog == nil || o.Config == nil || o.Homes == nil {
		return nil, ErrDirectoryOptions
	}
	return &Directory{reg: o.Registry, pools: o.Pools, catalog: o.Catalog, cfg: o.Config, homes: o.Homes}, nil
}

type Definition struct {
	ID         string
	Name       string
	Transport  string
	EnvVars    []string
	BaseURL    string
	BaseURLEnv string
	Aggregator bool
	AuthType   string
	Doc        string
	Source     string
}

func (d *Directory) pluginAliasOwner(ctx context.Context, canonical string) string {
	if canonical == "" || isStaticCanonical(canonical) {
		return canonical
	}
	profiles := d.reg.All(ctx)
	named := slices.ContainsFunc(profiles, func(p Profile) bool { return p.Spec().Name == canonical })
	aliased := slices.ContainsFunc(profiles, func(p Profile) bool { return slices.Contains(p.Spec().Aliases, canonical) })
	if named || !aliased {
		return canonical
	}
	owner, ok := d.reg.Get(ctx, canonical)
	if !ok || !slices.Contains(owner.Spec().Aliases, canonical) || owner.Spec().Overlay != nil {
		return canonical
	}
	return owner.Spec().Name
}

func (d *Directory) Definition(ctx context.Context, name string) (Definition, bool) {
	canonical := d.pluginAliasOwner(ctx, d.reg.NormalizeProvider(ctx, name))
	if d.reg.LeftCore(ctx, canonical) {
		return Definition{}, false
	}
	overlay, label, hasOverlay := d.reg.Overlay(ctx, canonical)
	if info, ok := d.catalog.ModelsDevProvider(ctx, canonical); ok {
		env := slices.Clone(info.Env)
		for _, ev := range overlay.ExtraEnv {
			if !slices.Contains(env, ev) {
				env = append(env, ev)
			}
		}
		base := overlay.BaseURLOverride
		if base == "" {
			base = info.API
		}
		return overlayDefinition(canonical, overlay, info.Name, env, base, info.Doc, "models.dev"), true
	}
	if hasOverlay {
		return overlayDefinition(canonical, overlay, label, slices.Clone(overlay.ExtraEnv), overlay.BaseURLOverride, "", "icebath"), true
	}
	def, ok := d.profileDefinition(ctx, canonical)
	if !ok || !(def.BaseURL != "" || (def.AuthType == authTypeAPIKey && len(def.EnvVars) > 0 && def.BaseURLEnv != "")) {
		return Definition{}, false
	}
	return def, true
}

func overlayDefinition(canonical string, ov Overlay, name string, env []string, base, doc, source string) Definition {
	transport := ov.Transport
	if transport == "" {
		transport = "openai_chat"
	}
	authType := ov.AuthType
	if authType == "" {
		authType = authTypeAPIKey
	}
	return Definition{ID: canonical, Name: name, Transport: transport, EnvVars: env, BaseURL: base,
		BaseURLEnv: ov.BaseURLEnv, Aggregator: ov.Aggregator, AuthType: authType, Doc: doc, Source: source}
}

func (d *Directory) profileDefinition(ctx context.Context, name string) (Definition, bool) {
	p, ok := d.reg.Get(ctx, name)
	if !ok {
		return Definition{}, false
	}
	s := p.Spec()
	mode := strings.TrimSpace(string(s.Mode))
	transport, known := ModeTransport(s.Mode)
	if !known {
		transport = mode
		if transport == "" {
			transport = "openai_chat"
		}
	}
	label := s.DisplayName
	if label == "" {
		label = s.Name
	}
	if label == "" {
		label = name
	}
	return Definition{ID: s.Name, Name: label, Transport: transport, EnvVars: s.KeyEnvVars(),
		BaseURL: strings.TrimSpace(s.BaseURL), BaseURLEnv: s.URLEnvVar(), AuthType: s.AuthTypeOrDefault(), Source: "plugin-profile"}, true
}

func (d *Directory) Spec(ctx context.Context, provider string) (auth.SeedSpec, bool) {
	if row, ok := d.reg.RegistryRow(ctx, provider); ok {
		return d.seedSpec(ctx, row), true
	}
	if def, ok := d.Definition(ctx, provider); ok {
		return d.definitionSpec(ctx, def), false
	}
	return auth.SeedSpec{}, false
}

func (d *Directory) Label(ctx context.Context, provider string) string {
	canonical := d.reg.NormalizeProvider(ctx, provider)
	if label, ok := labelOverrides[canonical]; ok {
		return label
	}
	if def, ok := d.Definition(ctx, canonical); ok {
		return def.Name
	}
	return canonical
}

func (d *Directory) IsAggregator(ctx context.Context, provider string) bool {
	normalized := d.reg.NormalizeProvider(ctx, provider)
	if strings.HasPrefix(normalized, "custom:") {
		return true
	}
	def, ok := d.Definition(ctx, normalized)
	return ok && def.Aggregator
}

func (d *Directory) IsRoutingAggregator(ctx context.Context, provider string) bool {
	normalized := d.reg.NormalizeProvider(ctx, provider)
	if IsFlatNamespaceReseller(normalized) {
		return false
	}
	return d.IsAggregator(ctx, normalized)
}

var errNoBedrockOwner = errors.New("providers: the registry was built without the bundled bedrock profile")

func (d *Directory) awsSDKStatus(provider string) func(ctx context.Context) (auth.Status, bool) {
	return func(ctx context.Context) (auth.Status, bool) {
		owner, ok := d.reg.BedrockOwner()
		if !ok {
			return auth.Status{Provider: provider, Error: errNoBedrockOwner.Error()}, true
		}
		return awsSDKStatus(ctx, owner, provider), true
	}
}

func (d *Directory) Specs(ctx context.Context) []auth.SeedSpec {
	specs := []auth.SeedSpec{}
	seen := map[string]bool{}
	for _, row := range d.reg.RegistryRows(ctx) {
		specs = append(specs, d.seedSpec(ctx, row))
		seen[row.ID] = true
	}
	for _, p := range d.reg.All(ctx) {
		name := p.Spec().Name
		if seen[name] {
			continue
		}
		if def, ok := d.Definition(ctx, name); ok && def.ID == name {
			specs = append(specs, d.definitionSpec(ctx, def))
			seen[name] = true
		}
	}
	return specs
}

func (d *Directory) definitionSpec(ctx context.Context, def Definition) auth.SeedSpec {
	return d.withHooks(ctx, auth.SeedSpec{Provider: def.ID, Name: def.Name, AuthType: def.AuthType,
		EnvVars: slices.Clone(def.EnvVars), BaseURL: def.BaseURL, BaseURLEnv: def.BaseURLEnv}, false)
}

func (d *Directory) seedSpec(ctx context.Context, row Row) auth.SeedSpec {
	return d.withHooks(ctx, auth.SeedSpec{Provider: row.ID, Name: row.Name, AuthType: row.AuthType,
		EnvVars: slices.Clone(row.APIKeyEnvVars), BaseURL: row.InferenceBaseURL, BaseURLEnv: row.BaseURLEnvVar}, row.Mirrored)
}

func (d *Directory) withHooks(ctx context.Context, spec auth.SeedSpec, mirrored bool) auth.SeedSpec {
	p, ok := d.reg.Get(ctx, spec.Provider)
	if !ok || p.Spec().Name != spec.Provider {
		switch owner, bundled := d.reg.EntraOwner(); {
		case bundled && spec.Provider == ProviderAzureFoundry:
			spec.Status = owner.AuthStatus
		case mirrored && isOAuthShaped(spec.AuthType):
			spec.Status = d.pluginOAuthStatus(spec.Provider, nil)
		case spec.AuthType == authTypeAWSSDK:
			spec.Status = d.awsSDKStatus(spec.Provider)
		}
		return spec
	}
	s := p.Spec()
	if spec.AuthType == authTypeExternalProcess {
		spec.ProcessCommand = s.Process.Command
		spec.ProcessArgs = slices.Clone(s.Process.Args)
		spec.ProcessCommandEnv = slices.Clone(s.Process.CommandEnv)
		spec.ProcessArgsEnv = s.Process.ArgsEnv
	}
	if h, ok := p.(SecretResolver); ok {
		spec.ResolveSecret = func(ctx context.Context) (auth.Secret, string, bool) {
			key, source, ok, err := h.ResolveSecret(ctx)
			if err != nil {
				return auth.Secret{}, "", false
			}
			return key, source, ok
		}
	}
	if h, ok := p.(BaseURLDeriver); ok {
		spec.RuntimeBaseURL = h.RuntimeBaseURL
	}
	if h, ok := p.(SourceRemover); ok {
		spec.RemoveSource = h.RemoveSource
	}
	if h, ok := p.(AuthEvidencer); ok {
		spec.ExternalAuthEvidence = h.ExternalAuthEvidence
	}
	if h, ok := p.(EntitlementExplainer); ok {
		spec.EntitlementMessage = h.EntitlementMessage
	}
	if h, ok := p.(PoolSeeder); ok {
		spec.SeedPool = func(ctx context.Context, suppressed []string, explicit bool) []auth.Seed {
			seeds, err := h.SeedPool(ctx, suppressed, explicit)
			if err != nil {
				return []auth.Seed{}
			}
			return seeds
		}
	}
	var refresher auth.Refresher
	if h, ok := p.(CredentialRefresher); ok {
		refresher = h.CredentialRefresher()
		spec.Refresh = refresher
	}
	if h, ok := p.(PoolEntryPreparer); ok {
		spec.RuntimeKey = h.RuntimeKey
	}
	if h, ok := p.(StateSeeder); ok {
		spec.StateSeedable = h.StateSeedable
	}
	switch h, ok := p.(StatusReporter); {
	case ok:
		spec.Status = h.AuthStatus
	case spec.Provider == ProviderAzureFoundry && d.reg.entra != nil:
		spec.Status = d.reg.entra.AuthStatus
	case mirrored && isOAuthShaped(spec.AuthType):
		spec.Status = d.pluginOAuthStatus(spec.Provider, refresher)
	case spec.AuthType == authTypeAWSSDK:
		spec.Status = d.awsSDKStatus(spec.Provider)
	}
	return spec
}

func isOAuthShaped(authType string) bool {
	return authType == authTypeOAuthDeviceCode || authType == authTypeOAuthExternal
}

func (d *Directory) pluginOAuthStatus(provider string, refresher auth.Refresher) func(ctx context.Context) (auth.Status, bool) {
	return func(ctx context.Context) (auth.Status, bool) {
		pool, err := d.pools.Pool(ctx, provider)
		if err != nil {
			return auth.Status{Provider: provider, Configured: true, Error: err.Error(),
				Hint: fmt.Sprintf("Run `icebath auth add %s` to sign in.", provider)}, true
		}
		var entries, live, refreshable []auth.Entry
		for _, e := range pool.Entries() {
			if strings.TrimSpace(e.AccessToken.Reveal()) != "" || strings.TrimSpace(e.AgentKey.Reveal()) != "" {
				entries = append(entries, e)
			}
		}
		for _, e := range entries {
			if !entryExpired(e) {
				live = append(live, e)
			}
		}
		if len(live) == 0 && refresher != nil {
			for _, e := range entries {
				if !e.RefreshToken.Empty() {
					refreshable = append(refreshable, e)
				}
			}
		}
		baseURL := ""
		for _, e := range append(slices.Clone(live), refreshable...) {
			if e.BaseURL != "" {
				baseURL = e.BaseURL
				break
			}
		}
		hint := ""
		if len(live) == 0 {
			hint = fmt.Sprintf("Run `icebath auth add %s` to sign in.", provider)
		}
		return auth.Status{Configured: true, Provider: provider, LoggedIn: len(live) > 0,
			NeedsRefresh: len(refreshable) > 0, Accounts: len(entries), BaseURL: baseURL, Hint: hint}, true
	}
}

func entryExpired(e auth.Entry) bool {
	now := time.Now()
	if e.ExpiresAtMS != 0 {
		return e.ExpiresAtMS <= now.UnixMilli()
	}
	if e.ExpiresAt != "" {
		at, ok := auth.ParseISOTimestamp(e.ExpiresAt)
		return ok && !at.After(now)
	}
	return false
}

func (d *Directory) ClearModelsCache(ctx context.Context, provider string) {
	d.catalog.ClearModelsCache(ctx, provider)
}

func (d *Directory) InstallHint(ctx context.Context, provider string) string {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" || !slices.Contains(leftCoreProviders, key) {
		return ""
	}
	homeDir, err := d.homes.Home(ctx)
	if err != nil {
		return ""
	}
	command := "icebath " + home.ProfileSelector(homeDir) + "plugins install " + leftCorePlugin
	if !isDir(home.Plugins.In(homeDir) + "/" + leftCorePlugin) {
		return fmt.Sprintf("%s moved out of core into the '%s' plugin: install it with `%s`.", leftCoreLabel, leftCorePlugin, command)
	}
	if cfg, err := d.cfg.LoadReadonly(home.WithOverride(ctx, homeDir)); err == nil && cfg.Plugins.Disabled != nil &&
		slices.Contains(*cfg.Plugins.Disabled, leftCorePlugin) {
		return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is installed but disabled: run `%s`.",
			leftCoreLabel, leftCorePlugin, strings.Replace(command, " install ", " enable ", 1))
	}
	detail := ""
	if loadErr := d.reg.LoadError(ctx, leftCorePlugin); loadErr != "" {
		detail = " (" + loadErr + ")"
	}
	return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is installed but failed to load%s: run `%s`, or see `icebath logs` for the cause.",
		leftCoreLabel, leftCorePlugin, detail, strings.Replace(command, " install ", " update ", 1))
}

func (d *Directory) IsRefreshableOAuthProvider(ctx context.Context, provider string) bool {
	if auth.IsRefreshableOAuthProvider(provider) {
		return true
	}
	p, ok := d.reg.Get(ctx, provider)
	if !ok {
		return false
	}
	_, ok = p.(CredentialRefresher)
	return ok
}

type AuthHandlerError struct {
	Provider string
	Action   string
	Cause    error
}

func (e *AuthHandlerError) Error() string {
	return fmt.Sprintf("%s auth handler failed for `%s`: %v", e.Provider, e.Action, e.Cause)
}

func (e *AuthHandlerError) Unwrap() error { return e.Cause }

type MissingAuthHandlerError struct {
	Provider string
	AuthType string
	Action   string
}

func (e *MissingAuthHandlerError) Error() string {
	return fmt.Sprintf("Provider '%s' declares auth_type '%s' but its plugin ships no auth handler, so `icebath auth %s %s` cannot be handled. "+
		"Add an auth handler to its provider profile (see the model-provider plugin guide).", e.Provider, e.AuthType, e.Action, e.Provider)
}

func (d *Directory) DispatchAuth(ctx context.Context, action string, args AuthArgs, provider string) (bool, error) {
	p, ok := d.reg.Get(ctx, provider)
	if !ok {
		return false, nil
	}
	h, ok := p.(AuthHandler)
	if !ok {
		return false, nil
	}
	owned, err := h.HandleAuth(ctx, action, args)
	if err != nil {
		var exit *AuthHandlerError
		if errors.As(err, &exit) {
			return false, err
		}
		return false, &AuthHandlerError{Provider: provider, Action: action, Cause: err}
	}
	return owned, nil
}

func (d *Directory) MissingAuthHandler(ctx context.Context, provider, action string) error {
	row, ok := d.reg.RegistryRow(ctx, provider)
	if !ok || !row.Mirrored || row.ID != provider {
		return nil
	}
	p, ok := d.reg.Get(ctx, provider)
	if !ok || registryAuthType(p.Spec()) == authTypeAPIKey {
		return nil
	}
	if _, has := p.(AuthHandler); has {
		return nil
	}
	return &MissingAuthHandlerError{Provider: provider, AuthType: p.Spec().AuthTypeOrDefault(), Action: action}
}
