package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ModelFlowRun is one provider wizard invocation, the loaded config, the current model and the CLI namespace
type ModelFlowRun struct {
	Ctx          context.Context
	Env          PoolEnv
	Out          io.Writer
	Config       map[string]any
	CurrentModel string
	Args         map[string]any
}

// Flow helpers swappable so tests script credentials, catalogs, prompts and persistence
var (
	envBaseURLFn              = envBaseURL
	promptBaseURLOverrideFn   = promptBaseURLOverride
	ensureFlowAPIKeyFn        = ensureFlowAPIKey
	modelsDevMergedFn         = modelsDevMerged
	flowFetchAPIModelsFn      = FetchAPIModels
	flowPricingFn             = GetPricingForProvider
	pickModelOrPromptFn       = pickModelOrPrompt
	finishModelFn             = finishModel
	apiKeyProviderModelListFn = apiKeyProviderModelList
	probeGeminiTierFn         = probeGeminiTier
	flowAskFn                 = flowAsk
	flowXAIStatusFn           = GetXAIOAuthAuthStatus
	flowLoginXAIFn            = LoginXAIOAuth
	flowResolveXAIFn          = resolveXAIOAuthRuntimeCredentials
	flowStdinIsTTYFn          = func() bool { return streamIsTTY(os.Stdin) }
	flowLoadPoolFn            = LoadPool
	flowGetEnvValueFn         = config.GetEnvValue
	flowLoginNousFn           = LoginNous
	flowRunLoginFn            = runLogin
	flowNousAuthStateFn       = func(r *ModelFlowRun) (map[string]any, error) { return r.store().GetProviderAuthState("nous") }
	flowResolveNousCredsFn    = resolveNousRuntimeCredentials
	flowCuratedNousIDsFn      = GetCuratedNousModelIDs
	flowEnableToolGatewayFn   = PromptEnableToolGateway
	flowCodexStatusFn         = GetCodexAuthStatus
	flowLoginCodexFn          = LoginOpenAICodex
	flowResolveCodexFn        = resolveCodexRuntimeCredentials
	flowCodexModelIDsFn       = GetCodexModelIDs
)

// Free tier refusal copy printed before the Gemini provider is left unsaved
var geminiFreeTierNotice = []string{
	"", "❌ This Google API key is on the free tier (<= 250 requests/day for gemini-2.5-flash).",
	"   Hermes typically makes 3-10 API calls per user turn (tool iterations + auxiliary tasks),",
	"   so the free tier is exhausted after a handful of messages and cannot sustain",
	"   an agent session.", "",
	"   To use Gemini with Hermes, enable billing on your Google Cloud project and regenerate",
	"   the key in a billing-enabled project: https://aistudio.google.com/apikey", "",
	"   Alternatives with workable free usage: DeepSeek, OpenRouter (free models), Groq, Nous.", "",
	"Not saving Gemini as the default provider."}

func (r *ModelFlowRun) ctx() context.Context {
	if r.Ctx != nil {
		return r.Ctx
	}
	return r.Env.ctx()
}

func (r *ModelFlowRun) home() string { return envHome(r.Env) }

func (r *ModelFlowRun) store() *Store {
	if r.Env.Store != nil {
		return r.Env.Store
	}
	return NewStore(r.home())
}

// The home's config tree as a fresh read
func (r *ModelFlowRun) loadConfig() (config.Tree, error) {
	h := r.home()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = config.Tree{}
	}
	return cfg, nil
}

// The registry provider config of an id, an error when the registry lacks it
func (r *ModelFlowRun) registryProfile(providerID string) (Profile, error) {
	pconfig, ok := registryProfileCtx(r.ctx(), providerID)
	if !ok {
		return Profile{}, fmt.Errorf("KeyError: %q", providerID)
	}
	return pconfig, nil
}

// Dotenv value of a key, empty when unset
func (r *ModelFlowRun) envValue(key string) string {
	v, _, _ := flowGetEnvValueFn(r.ctx(), r.home(), key)
	return v
}

func (r *ModelFlowRun) saveEnv(key, value string) error {
	return config.SaveEnvValue(r.home(), key, value)
}

func hasHTTPPrefix(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// Base URL override from .env then the process environment, empty when unset
func envBaseURL(r *ModelFlowRun, baseURLEnv string) string {
	if baseURLEnv == "" {
		return ""
	}
	if v := r.envValue(baseURLEnv); v != "" {
		return v
	}
	return os.Getenv(baseURLEnv)
}

// Optional Base URL prompt, a valid override saved to the env var and returned
func promptBaseURLOverride(r *ModelFlowRun, effectiveBase, baseURLEnv string, persistEnv bool) (string, error) {
	override, _ := flowAskFn(r.Out, fmt.Sprintf("Base URL [%s]: ", effectiveBase), flowAskOptions{CancelMsg: optStr("")})
	if override != "" && baseURLEnv != "" {
		if !hasHTTPPrefix(override) {
			fmt.Fprintln(r.Out, "  Invalid URL — must start with http:// or https://. Keeping current value.")
		} else {
			if persistEnv {
				if err := r.saveEnv(baseURLEnv, override); err != nil {
					return "", err
				}
			}
			return override, nil
		}
	}
	return effectiveBase, nil
}

// Tells how many live models a source returned
func reportLiveModels(out io.Writer, modelList []string, source string) {
	if len(modelList) > 0 {
		fmt.Fprintf(out, "  Found %d model(s) from %s\n", len(modelList), source)
	}
}

// OpenRouter provider, ensure the api key then pick a model with live pricing
func modelFlowOpenrouter(r *ModelFlowRun) error {
	pconfig := Profile{Name: "openrouter", DisplayName: "OpenRouter", AuthType: AuthTypeAPIKey, KeyEnv: []string{"OPENROUTER_API_KEY"}}
	existingKey, resolved, abort, err := ensureFlowAPIKeyFn(r.Env, r.Out, "openrouter", pconfig, []string{"Get one at: https://openrouter.ai/keys", ""})
	if err != nil || abort {
		return err
	}
	models := OpenRouterModelIDs(r.ctx(), true)
	pricing := flowPricingFn(r.ctx(), "openrouter", "", true, false)
	confirmKey := resolved
	if confirmKey == "" {
		confirmKey = existingKey
	}
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: models, CurrentModel: r.CurrentModel, Pricing: pricing,
		ConfirmProvider: "openrouter", ConfirmBaseURL: home.OpenRouterBaseURL, ConfirmAPIKey: confirmKey})
	_, err = finishModelFn(r.Env, r.Out, selected, "openrouter", fmt.Sprintf("Default model set to: %s (via OpenRouter)", selected), flowNoChange,
		persistModelOptions{BaseURL: optStr(home.OpenRouterBaseURL), APIMode: optStr("chat_completions")})
	return err
}

// Vercel AI Gateway provider, ensure the api key then pick a model with pricing, inline credentials untouched
func modelFlowAIGateway(r *ModelFlowRun) error {
	pconfig, err := r.registryProfile("ai-gateway")
	if err != nil {
		return err
	}
	existingKey := r.envValue("AI_GATEWAY_API_KEY")
	if existingKey == "" {
		flowSay(r.Out, "Create API key here: https://vercel.com/d?to=%2F%5Bteam%5D%2F%7E%2Fai-gateway&title=AI+Gateway",
			"Add a payment method to get $5 in free credits.", "")
	}
	_, abort, err := promptAPIKeyFn(r.Env, r.Out, pconfig, existingKey, "ai-gateway", "")
	if err != nil || abort {
		return err
	}
	models := AIGatewayModelIDs(r.ctx(), true)
	pricing := flowPricingFn(r.ctx(), "ai-gateway", "", true, false)
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: models, CurrentModel: r.CurrentModel, Pricing: pricing})
	_, err = finishModelFn(r.Env, r.Out, selected, "ai-gateway", fmt.Sprintf("Default model set to: %s (via Vercel AI Gateway)", selected), flowNoChange,
		persistModelOptions{BaseURL: optStr(home.AIGatewayBaseURL), APIMode: optStr("chat_completions"), KeepCreds: true})
	return err
}

// Mixture of Agents virtual provider, pick a preset, persist it and print the breakdown
func modelFlowMoA(r *ModelFlowRun) error {
	var rawMoA any
	if r.Config != nil {
		rawMoA = r.Config["moa"]
	} else {
		rawMoA = map[string]any{}
	}
	moa := normalizeMoAConfig(rawMoA)
	presets, _ := moa["presets"].(map[string]any)
	if len(presets) == 0 {
		fmt.Fprintln(r.Out, "No MoA presets configured. Run `icebath moa configure <name>` first.")
		return nil
	}
	names := config.MappingKeys(presets)
	defaultName := anyString(moa["default_preset"])
	if defaultName == "" {
		defaultName = names[0]
	}
	rows := make([]string, 0, len(names))
	defaultIdx := 0
	for i, n := range names {
		preset, _ := presets[n].(map[string]any)
		agg, _ := preset["aggregator"].(map[string]any)
		aggLabel := ""
		if len(agg) > 0 {
			aggLabel = fmt.Sprintf("%s:%s", pyStr(agg["provider"]), pyStr(agg["model"]))
		}
		refs, _ := preset["reference_models"].([]any)
		suffix := ""
		if n == defaultName {
			suffix = "  ← default"
			defaultIdx = i
		}
		rows = append(rows, fmt.Sprintf("%s  (acting: %s, %d refs)%s", n, aggLabel, len(refs), suffix))
	}
	title := "Select a Mixture of Agents preset:"
	idx, ok := cursesChoice(title, rows, defaultIdx)
	if !ok {
		printNumbered(r.Out, title, rows, defaultIdx)
		raw, answered := flowAskFn(r.Out, fmt.Sprintf("  Choice [1-%d]: ", len(rows)), flowAskOptions{Raw: true, CancelMsg: optStr(flowNoChange)})
		if !answered {
			return nil
		}
		if raw == "" {
			idx = defaultIdx
		} else {
			n, perr := parsePyInt(raw)
			if perr != nil {
				fmt.Fprintln(r.Out, flowNoChange)
				return nil
			}
			idx = max(0, min(len(rows)-1, n-1))
		}
	}
	if idx < 0 {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	selectedName := names[idx]
	cfg, model, err := loadConfigModelSection(r.home())
	if err != nil {
		return err
	}
	pyDictSet(model, "default", selectedName)
	pyDictSet(model, "provider", "moa")
	clearModelEndpointCredentials(model, true, true, false)
	delete(model, "base_url")
	if err := commitModelConfig(r.Env, cfg); err != nil {
		return err
	}
	if err := flowSaveModelChoiceFn(r.Env, selectedName); err != nil {
		return err
	}
	preset, _ := presets[selectedName].(map[string]any)
	flowSay(r.Out, "", fmt.Sprintf("Default model set to: %s (via Mixture of Agents)", selectedName),
		fmt.Sprintf("  Preset: %s", selectedName), "  Reference models (advise once per user turn):")
	refs, _ := preset["reference_models"].([]any)
	for i, slot := range refs {
		m, _ := slot.(map[string]any)
		fmt.Fprintf(r.Out, "    %d. %s:%s\n", i+1, pyStr(m["provider"]), pyStr(m["model"]))
	}
	agg, _ := preset["aggregator"].(map[string]any)
	fmt.Fprintf(r.Out, "  Aggregator:  %s:%s  (acting model — runs every step and carries almost all of the cost)\n", pyStr(agg["provider"]), pyStr(agg["model"]))
	return nil
}

// Python int of a menu answer, a ValueError for anything int rejects and huge values saturated
func parsePyInt(s string) (int, error) {
	n, ok := pystr.IntText(s)
	if !ok {
		return 0, fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", config.PyRepr(s))
	}
	if n.IsInt64() {
		return int(n.Int64()), nil
	}
	if n.Sign() < 0 {
		return math.MinInt64 + 1, nil
	}
	return math.MaxInt64, nil
}

// Nous login arguments from the CLI namespace with the 15 second default timeout
func nousLoginArgs(args map[string]any) NousLoginArgs {
	if args == nil {
		args = map[string]any{}
	}
	return NousLoginArgs{PortalURL: anyString(args["portal_url"]), InferenceURL: anyString(args["inference_url"]),
		ClientID: anyString(args["client_id"]), Scope: anyString(args["scope"]), NoBrowser: truthy(args["no_browser"]),
		Timeout: authTimeout(args, 15*time.Second), CABundle: anyString(args["ca_bundle"]), Insecure: truthy(args["insecure"])}
}

// nousPickerCatalog is the free or paid tier picker catalog with the unavailable rows and the policy narrowing flag
type nousPickerCatalog struct {
	modelIDs           []string
	pricing            PricingTable
	unavailableModels  []string
	unavailableMessage string
	policyNarrowed     bool
}

// Free or paid tier catalog for the Nous picker, nil when nothing is selectable with the reason printed
func nousModelCatalog(r *ModelFlowRun, freeTier bool, portalURL string, modelIDs []string, pricing PricingTable) *nousPickerCatalog {
	ctx := r.ctx()
	unavailableModels := []string{}
	unavailableMessage := ""
	policyAllowed, _ := nousPolicyAllowedIDsFn(ctx, false)
	if freeTier {
		func() {
			defer func() {
				if recover() != nil {
					unavailableMessage = ""
				}
			}()
			info := GetNousPortalAccountInfo(ctx, true)
			unavailableMessage = FormatNousPortalEntitlementMessage(info, "paid Nous models", false)
		}()
		modelIDs, pricing = unionPortalFreeFn(ctx, modelIDs, pricing, portalURL, false)
	} else {
		modelIDs, pricing = unionPortalPaidFn(ctx, modelIDs, pricing, portalURL, false)
		modelIDs = UnionWithNousOnSaleModels(modelIDs, pricing)
	}
	before := modelIDs
	modelIDs = RestrictToNousPolicy(modelIDs, policyAllowed, true)
	policyNarrowed := !equalStrings(modelIDs, before)
	if freeTier {
		modelIDs, unavailableModels = PartitionNousModelsByTier(modelIDs, pricing, true)
	}
	if len(modelIDs) == 0 && len(unavailableModels) == 0 {
		fmt.Fprintln(r.Out, "No models available for Nous Portal after filtering.")
		noteSetupFailure("no_models")
		return nil
	}
	if freeTier && len(modelIDs) == 0 {
		fmt.Fprintln(r.Out, "No free models currently available.")
		if len(unavailableModels) > 0 {
			u := portalURL
			if u == "" {
				u = DefaultNousPortalURL
			}
			msg := unavailableMessage
			if msg == "" {
				msg = fmt.Sprintf("Upgrade at %s to access paid models.", strings.TrimRight(u, "/"))
			}
			fmt.Fprintln(r.Out, msg)
		}
		return nil
	}
	return &nousPickerCatalog{modelIDs: modelIDs, pricing: pricing, unavailableModels: unavailableModels, unavailableMessage: unavailableMessage, policyNarrowed: policyNarrowed}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Nous runtime credentials, nil after printing the diagnosis and re-logging in when the session expired
func nousVerifiedCredentials(r *ModelFlowRun) map[string]any {
	creds, err := flowResolveNousCredsFn(r.Env, false, "")
	if err == nil {
		return creds
	}
	var authErr *AuthError
	relogin := errors.As(err, &authErr) && authErr.ReloginRequired
	msg := err.Error()
	if authErr != nil {
		msg = formatAuthError(authErr)
	}
	if relogin {
		flowSay(r.Out, fmt.Sprintf("Session expired: %s", msg), "Re-authenticating with Nous Portal...\n")
		if lerr := flowLoginNousFn(r.Env, r.Out, nousLoginArgs(nil)); lerr != nil {
			fmt.Fprintf(r.Out, "Re-login failed: %v\n", lerr)
		}
		noteSetupFailure("auth")
		return nil
	}
	fmt.Fprintf(r.Out, "Could not verify credentials: %s\n", msg)
	noteSetupFailure("auth")
	return nil
}

// Nous persist step, model choice and provider state, the model section rewritten on a fresh config
func nousPersistSelection(r *ModelFlowRun, selected string, creds map[string]any) (config.Tree, error) {
	if err := flowSaveModelChoiceFn(r.Env, selected); err != nil {
		return nil, err
	}
	inferenceURL := anyString(creds["base_url"])
	if _, err := updateConfigForProvider(r.Env, "nous", inferenceURL, "", false); err != nil {
		return nil, err
	}
	cfg, err := r.loadConfig()
	if err != nil {
		return nil, err
	}
	modelCfg := map[string]any{}
	switch current := cfg["model"].(type) {
	case map[string]any:
		for k, v := range current {
			modelCfg[k] = v
		}
		config.SetMappingKeys(modelCfg, config.MappingKeys(current))
	case string:
		if pystr.Strip(current) != "" {
			pyDictSet(modelCfg, "default", pystr.Strip(current))
		}
	}
	pyDictSet(modelCfg, "provider", "nous")
	pyDictSet(modelCfg, "default", selected)
	if pystr.Strip(inferenceURL) != "" {
		pyDictSet(modelCfg, "base_url", strings.TrimRight(inferenceURL, "/"))
	} else {
		delete(modelCfg, "base_url")
	}
	clearModelEndpointCredentials(modelCfg, true, true, false)
	pyDictSet(cfg, "model", modelCfg)
	if r.envValue("OPENAI_BASE_URL") != "" {
		if err := r.saveEnv("OPENAI_BASE_URL", ""); err != nil {
			return nil, err
		}
		if err := r.saveEnv("OPENAI_API_KEY", ""); err != nil {
			return nil, err
		}
	}
	if err := config.Save(r.home(), cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Nous Portal provider, log in when signed out, else the curated picker over verified credentials
func modelFlowNous(r *ModelFlowRun) error {
	state, _ := flowNousAuthStateFn(r)
	if len(state) == 0 || !truthy(state["access_token"]) || guestWithoutFreeTierIn(r.Env, state) {
		flowSay(r.Out, "Not logged into Nous Portal. Starting login...", "")
		pconfig, err := r.registryProfile("nous")
		if err != nil {
			return err
		}
		flowRunLoginFn(r.Out, &pconfig, func() error {
			if err := flowLoginNousFn(r.Env, r.Out, nousLoginArgs(r.Args)); err != nil {
				return err
			}
			func() {
				defer func() {
					if p := recover(); isSetupControlFlow(p) {
						panic(p)
					}
				}()
				cfg, cerr := r.loadConfig()
				if cerr != nil {
					return
				}
				_, _ = flowEnableToolGatewayFn(r.Env, r.Out, cfg, true)
			}()
			return nil
		})
		return nil
	}
	modelIDs := flowCuratedNousIDsFn(r.ctx())
	if len(modelIDs) == 0 {
		fmt.Fprintln(r.Out, "No curated models available for Nous Portal.")
		return nil
	}
	creds := nousVerifiedCredentials(r)
	if creds == nil {
		return nil
	}
	pricing := flowPricingFn(r.ctx(), "nous", "", false, false)
	freeTier := checkNousFreeTierFn(r.ctx(), true, false)
	if !freeTier {
		if fresh, err := flowResolveNousCredsFn(r.Env, true, ""); err == nil && len(fresh) > 0 {
			creds = fresh
		}
	}
	portalURL := ""
	if st, err := flowNousAuthStateFn(r); err == nil {
		portalURL = anyString(st["portal_base_url"])
	}
	catalog := nousModelCatalog(r, freeTier, portalURL, modelIDs, pricing)
	if catalog == nil {
		return nil
	}
	if notice := nousPolicyNotice(r.Env, catalog.policyNarrowed); notice != "" {
		fmt.Fprintln(r.Out, notice)
	}
	usage := NousPickerUsage(true, false)
	modelIDs = catalog.modelIDs
	if usage != nil {
		modelIDs = featuredFirst(modelIDs, usage.FeaturedModelIDs)
	}
	fmt.Fprintf(r.Out, "Showing %d curated models — use \"Enter custom model name\" for others.\n", len(modelIDs))
	prompt := ModelSelectionPrompt{ModelIDs: modelIDs, CurrentModel: r.CurrentModel, Pricing: catalog.pricing, UnavailableModels: catalog.unavailableModels,
		PortalURL: portalURL, UnavailableMessage: catalog.unavailableMessage, ConfirmProvider: "nous",
		ConfirmBaseURL: anyString(creds["base_url"]), ConfirmAPIKey: anyString(creds["api_key"])}
	if usage != nil {
		prompt.Usage = usage
	}
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, prompt)
	if selected == "" {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	cfg, err := nousPersistSelection(r, selected, creds)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "Default model set to: %s (via Nous Portal)\n", selected)
	if usage != nil {
		if notice := largeModelNotice(r.Env, selected, "nous"); notice != "" {
			fmt.Fprintf(r.Out, "⚠ %s\n", notice)
		}
	}
	_, err = flowEnableToolGatewayFn(r.Env, r.Out, cfg, true)
	return err
}

// ModelFlowNous runs the Nous Portal login and model pick for a config on the setup console of the context home
func ModelFlowNous(ctx context.Context, cfg map[string]any) error {
	env := homePoolEnv(home.Dir(ctx))
	env.Ctx = ctx
	return modelFlowNous(&ModelFlowRun{Ctx: ctx, Env: env, Out: SetupOutput(), Config: cfg})
}

// OpenAI Codex provider, ensure logged in then pick a model over the pooled or legacy token
func modelFlowOpenAICodex(r *ModelFlowRun) error {
	pconfig, err := r.registryProfile("openai-codex")
	if err != nil {
		return err
	}
	loggedIn := truthy(flowCodexStatusFn(r.Env)["logged_in"])
	login := func(force bool) error { return flowLoginCodexFn(r.Env, r.Out, CodexLoginArgs{}, force) }
	recheck := func() bool { return truthy(flowCodexStatusFn(r.Env)["logged_in"]) }
	if !oauthGate(r.Out, loggedIn, "OpenAI Codex", &pconfig, login, "", recheck) {
		return nil
	}
	var token, base string
	func() {
		defer func() { _ = recover() }()
		status := flowCodexStatusFn(r.Env)
		if truthy(status["logged_in"]) {
			token, base = anyString(status["api_key"]), anyString(status["base_url"])
		}
	}()
	if token == "" {
		if creds, cerr := flowResolveCodexFn(r.Env, CodexResolveOptions{}); cerr == nil {
			token, base = anyString(creds["api_key"]), anyString(creds["base_url"])
		}
	}
	codexModels, _ := flowCodexModelIDsFn(r.ctx(), token, base)
	confirmBase := base
	if confirmBase == "" {
		confirmBase = DefaultCodexBaseURL
	}
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: codexModels, CurrentModel: r.CurrentModel,
		ConfirmProvider: "openai-codex", ConfirmBaseURL: confirmBase, ConfirmAPIKey: token})
	return activateProviderModel(r.Env, r.Out, selected, "openai-codex", DefaultCodexBaseURL,
		fmt.Sprintf("Default model set to: %s (via OpenAI Codex)", selected), optStr(flowNoChange))
}

// xAI Grok OAuth provider, ensure logged in then pick a model at the resolved or default base URL
func modelFlowXAIOAuth(r *ModelFlowRun) error {
	pconfig, err := r.registryProfile("xai-oauth")
	if err != nil {
		return err
	}
	args := r.Args
	if args == nil {
		args = map[string]any{}
	}
	loginArgs := XAILoginArgs{NoBrowser: truthy(args["no_browser"]), Timeout: authTimeout(args, 0)}
	loggedIn := truthy(flowXAIStatusFn(r.Env)["logged_in"])
	login := func(force bool) error { return flowLoginXAIFn(r.Env, r.Out, loginArgs, force) }
	if !oauthGate(r.Out, loggedIn, "xAI Grok OAuth (SuperGrok / Premium+)", &pconfig, login, "xAI OAuth", nil) {
		return nil
	}
	baseURL := DefaultXAIOAuthBaseURL
	if creds, cerr := flowResolveXAIFn(r.Env, false, true, 0); cerr == nil {
		if resolved := strings.TrimRight(strings.TrimSpace(anyString(creds["base_url"])), "/"); resolved != "" {
			baseURL = resolved
		}
	}
	models := ProviderModelIDs(r.ctx(), "xai-oauth", false).IDs
	current := r.CurrentModel
	if current == "" {
		current = "grok-4.6"
		if len(models) > 0 {
			current = models[0]
		}
	}
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: models, CurrentModel: current})
	return activateProviderModel(r.Env, r.Out, selected, "xai-oauth", baseURL,
		fmt.Sprintf("Default model set to: %s (via xAI Grok OAuth — SuperGrok / Premium+)", selected), optStr(flowNoChange))
}

// Qwen OAuth provider, reuse the local Qwen CLI login then pick a live or curated model
func modelFlowQwenOAuth(r *ModelFlowRun) error {
	status := getQwenAuthStatus(r.Env)
	if !truthy(status["logged_in"]) {
		lines := []string{"Not logged into Qwen CLI OAuth.", "Run: qwen auth qwen-oauth"}
		if truthy(status["auth_file"]) {
			lines = append(lines, fmt.Sprintf("Expected credentials file: %s", anyString(status["auth_file"])))
		}
		if truthy(status["error"]) {
			lines = append(lines, fmt.Sprintf("Error: %s", anyString(status["error"])))
		}
		flowSay(r.Out, lines...)
		return nil
	}
	var models []string
	if creds, err := resolveQwenRuntimeCredentials(r.Env, true); err == nil {
		models, _ = flowFetchAPIModelsFn(r.ctx(), anyString(creds["api_key"]), anyString(creds["base_url"]), 5*time.Second, "", nil)
	}
	if len(models) == 0 {
		models = append([]string{}, defaultQwenPortalModels...)
	}
	def := r.CurrentModel
	if def == "" {
		def = "qwen3-coder-plus"
		if len(models) > 0 {
			def = models[0]
		}
	}
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: models, CurrentModel: def, ConfirmProvider: "qwen-oauth", ConfirmBaseURL: DefaultQwenBaseURL})
	return activateProviderModel(r.Env, r.Out, selected, "qwen-oauth", DefaultQwenBaseURL, fmt.Sprintf("Default model set to: %s (via Qwen OAuth)", selected), optStr(flowNoChange))
}

// MiniMax OAuth provider, ensure logged in then pick a curated model at the resolved base URL
func modelFlowMinimaxOAuth(r *ModelFlowRun) error {
	state, _ := r.store().GetProviderAuthState("minimax-oauth")
	if len(state) == 0 || !truthy(state["access_token"]) {
		flowSay(r.Out, "Not logged into MiniMax. Starting OAuth login...", "")
		args := r.Args
		if args == nil {
			args = map[string]any{}
		}
		region := anyString(args["region"])
		if region == "" {
			region = "global"
		}
		mockArgs := MinimaxLoginArgs{Region: region, NoBrowser: truthy(args["no_browser"]), Timeout: authTimeout(args, 15*time.Second)}
		pconfig, err := r.registryProfile("minimax-oauth")
		if err != nil {
			return err
		}
		if !flowRunLoginFn(r.Out, &pconfig, func() error { return LoginMinimaxOAuth(r.Env, r.Out, mockArgs) }) {
			return nil
		}
	}
	creds, err := resolveMinimaxOAuthRuntimeCredentials(r.Env)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			fmt.Fprintln(r.Out, formatAuthError(authErr))
			noteSetupFailure("auth")
			return nil
		}
		return err
	}
	baseURL := anyString(creds["base_url"])
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: providerModels["minimax-oauth"], CurrentModel: r.CurrentModel,
		ConfirmProvider: "minimax-oauth", ConfirmBaseURL: baseURL})
	return activateProviderModel(r.Env, r.Out, selected, "minimax-oauth", baseURL, fmt.Sprintf("✓ Using MiniMax model: %s", selected), nil)
}

// Kimi or Moonshot provider, the endpoint chosen by the key prefix without a URL prompt
func modelFlowKimi(r *ModelFlowRun) error {
	providerID := "kimi-coding"
	pconfig, err := r.registryProfile(providerID)
	if err != nil {
		return err
	}
	baseURLEnv := pconfig.BaseURLEnv
	_, existingKey, abort, err := ensureFlowAPIKeyFn(r.Env, r.Out, providerID, pconfig, nil)
	if err != nil || abort {
		return err
	}
	isCodingPlan := strings.HasPrefix(existingKey, "sk-kimi-")
	effectiveBase := pconfig.BaseURL
	if isCodingPlan {
		effectiveBase = KimiCodeBaseURL
		fmt.Fprintf(r.Out, "  Detected Kimi Coding Plan key → %s\n", effectiveBase)
	} else {
		fmt.Fprintf(r.Out, "  Using Moonshot endpoint → %s\n", effectiveBase)
	}
	if baseURLEnv != "" && r.envValue(baseURLEnv) != "" {
		if err := r.saveEnv(baseURLEnv, ""); err != nil {
			return err
		}
	}
	fmt.Fprintln(r.Out)
	catalogKey, via := "moonshot", "Moonshot"
	if isCodingPlan {
		catalogKey, via = "kimi-coding", "Kimi Coding"
	}
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, providerModels[catalogKey], "Enter model name: ",
		ModelSelectionPrompt{CurrentModel: r.CurrentModel, ConfirmProvider: providerID, ConfirmBaseURL: effectiveBase, ConfirmAPIKey: existingKey})
	_, err = finishModelFn(r.Env, r.Out, selected, providerID, fmt.Sprintf("Default model set to: %s (via %s)", selected, via), flowNoChange,
		persistModelOptions{BaseURL: optStr(effectiveBase), DropAPIMode: true})
	return err
}

// StepFun Step Plan provider with region specific endpoints and the live plus curated catalog
func modelFlowStepfun(r *ModelFlowRun) error {
	providerID := "stepfun"
	pconfig, err := r.registryProfile(providerID)
	if err != nil {
		return err
	}
	baseURLEnv := pconfig.BaseURLEnv
	_, existingKey, abort, err := ensureFlowAPIKeyFn(r.Env, r.Out, providerID, pconfig, nil)
	if err != nil || abort {
		return err
	}
	currentBase := envBaseURLFn(r, baseURLEnv)
	if currentBase == "" {
		if modelCfg, ok := r.Config["model"].(map[string]any); ok {
			currentBase = pystr.Strip(pyOrStr(modelCfg["base_url"]))
		}
	}
	regionBase := currentBase
	if regionBase == "" {
		regionBase = pconfig.BaseURL
	}
	currentRegion := inferStepfunRegion(regionBase)
	type regionRow struct{ key, label string }
	regions := []regionRow{{"international", fmt.Sprintf("International (%s)", stepfunBaseURLForRegion("international"))},
		{"china", fmt.Sprintf("China (%s)", stepfunBaseURLForRegion("china"))}}
	ordered := []regionRow{}
	for _, rr := range regions {
		if rr.key == currentRegion {
			ordered = append(ordered, regionRow{rr.key, rr.label + "  ← currently active"})
		}
	}
	for _, rr := range regions {
		if rr.key != currentRegion {
			ordered = append(ordered, rr)
		}
	}
	ordered = append(ordered, regionRow{"cancel", "Cancel"})
	labels := make([]string, len(ordered))
	for i, rr := range ordered {
		labels[i] = rr.label
	}
	regionIdx, ok := setupPromptProviderChoiceFn(r.Out, labels, 0, "Select provider:")
	if !ok || ordered[regionIdx].key == "cancel" {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	effectiveBase := stepfunBaseURLForRegion(ordered[regionIdx].key)
	if baseURLEnv != "" {
		if err := r.saveEnv(baseURLEnv, effectiveBase); err != nil {
			return err
		}
	}
	name := profileDisplayName(pconfig)
	modelList := ProviderModelIDs(r.ctx(), providerID, false).IDs
	if len(modelList) == 0 {
		modelList, _ = flowFetchAPIModelsFn(r.ctx(), existingKey, effectiveBase, 5*time.Second, "", nil)
	}
	if len(modelList) > 0 {
		fmt.Fprintf(r.Out, "  Found %d model(s) from %s API\n", len(modelList), name)
	} else {
		modelList = providerModels[providerID]
		if len(modelList) > 0 {
			fmt.Fprintf(r.Out, "  Could not auto-detect models from %s API — showing Step Plan fallback catalog.\n", name)
		}
	}
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, modelList, "Model name: ",
		ModelSelectionPrompt{CurrentModel: r.CurrentModel, ConfirmProvider: providerID, ConfirmBaseURL: effectiveBase, ConfirmAPIKey: existingKey})
	model, err := finishModelFn(r.Env, r.Out, selected, providerID, fmt.Sprintf("Default model set to: %s (via %s)", selected, name), flowNoChange,
		persistModelOptions{BaseURL: optStr(effectiveBase), DropAPIMode: true})
	if err != nil {
		return err
	}
	if model != nil && r.Config != nil {
		copied := map[string]any{}
		for k, v := range model {
			copied[k] = v
		}
		config.SetMappingKeys(copied, config.MappingKeys(model))
		pyDictSet(r.Config, "model", copied)
	}
	return nil
}

// Google Vertex AI provider, credential source, project and region prompts, then the curated picker
func modelFlowVertex(r *ModelFlowRun) error {
	saPath := r.envValue("VERTEX_CREDENTIALS_PATH")
	if saPath == "" {
		saPath = r.envValue("GOOGLE_APPLICATION_CREDENTIALS")
	}
	saPath = pystr.Strip(saPath)
	if saPath != "" {
		fmt.Fprintf(r.Out, "  Vertex credentials: service account JSON (%s) ✓\n", saPath)
	} else {
		flowSay(r.Out, "  Vertex credentials: Application Default Credentials (ADC)",
			"    Vertex uses OAuth2, not a static API key. Either:",
			"      • run 'gcloud auth application-default login', or",
			"      • set VERTEX_CREDENTIALS_PATH in ~/.icebath/.env to a service account JSON")
	}
	fmt.Fprintln(r.Out)
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	vertexCfg, _ := cfg["vertex"].(map[string]any)
	currentProject := pystr.Strip(pyOrStr(vertexCfg["project_id"]))
	shown := currentProject
	if shown == "" {
		shown = "from credentials"
	}
	projectInput, ok := flowAskFn(r.Out, fmt.Sprintf("  GCP project ID [%s]: ", shown), flowAskOptions{CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	projectID := projectInput
	if projectID == "" {
		projectID = currentProject
	}
	currentRegion := "global"
	if pyTruthy(vertexCfg["region"]) {
		currentRegion = pyStr(vertexCfg["region"])
	}
	if currentRegion = pystr.Strip(currentRegion); currentRegion == "" {
		currentRegion = "global"
	}
	regionInput, ok := flowAskFn(r.Out, fmt.Sprintf("  Vertex region [%s]: ", currentRegion), flowAskOptions{CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	region := regionInput
	if region == "" {
		region = currentRegion
	}
	modelList := providerModels["vertex"]
	if len(modelList) == 0 {
		modelList = []string{"google/gemini-3-pro-preview", "google/gemini-3-flash-preview"}
	}
	host := "aiplatform.googleapis.com"
	if region != "global" {
		host = region + "-aiplatform.googleapis.com"
	}
	baseURLPreview := fmt.Sprintf("https://%s/v1beta1/projects/<project>/locations/%s/endpoints/openapi", host, region)
	selected, _ := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: modelList, CurrentModel: r.CurrentModel, ConfirmProvider: "vertex", ConfirmBaseURL: baseURLPreview})
	finish := func(cfg config.Tree, _ map[string]any) {
		vcfg := ensureDictSection(cfg, "vertex")
		pyDictSet(vcfg, "project_id", projectID)
		pyDictSet(vcfg, "region", region)
	}
	_, err = finishModelFn(r.Env, r.Out, selected, "vertex", fmt.Sprintf("  Default model set to: %s (via Google Vertex AI, %s)", selected, region), "  No change.",
		persistModelOptions{DropBaseURL: true, DropAPIMode: true, Finish: finish})
	return err
}

// Picker for the official Z.AI endpoints plus a custom proxy, the current base on cancel or an invalid URL
func selectZaiEndpoint(r *ModelFlowRun, currentBase string) string {
	normalizedCurrent := strings.TrimRight(strings.TrimSpace(currentBase), "/")
	defaultIdx := 0
	if normalizedCurrent != "" {
		defaultIdx = len(ZaiEndpoints)
	}
	choices := make([]string, 0, len(ZaiEndpoints)+1)
	for i, ep := range ZaiEndpoints {
		if normalizedCurrent != "" && normalizedCurrent == strings.TrimRight(ep.BaseURL, "/") && defaultIdx == len(ZaiEndpoints) {
			defaultIdx = i
		}
		choices = append(choices, fmt.Sprintf("%s (%s)", ep.Label, ep.BaseURL))
	}
	choices = append(choices, "Custom proxy URL")
	selected, ok := setupPromptProviderChoiceFn(r.Out, choices, defaultIdx, "Select Z.AI / GLM endpoint:")
	if !ok {
		return currentBase
	}
	if selected != len(ZaiEndpoints) {
		return strings.TrimRight(ZaiEndpoints[selected].BaseURL, "/")
	}
	override, _ := flowAskFn(r.Out, fmt.Sprintf("Custom base URL [%s]: ", currentBase), flowAskOptions{CancelMsg: optStr("")})
	if override == "" {
		return currentBase
	}
	if !hasHTTPPrefix(override) {
		fmt.Fprintln(r.Out, "  Invalid URL — must start with http:// or https://. Keeping current value.")
		return currentBase
	}
	return strings.TrimRight(override, "/")
}

// Gemini free tier gate, false after the refusal notice when the probe reports a free tier key
func geminiTierOK(r *ModelFlowRun, existingKey string, pconfig Profile, baseURLEnv string) bool {
	fmt.Fprintln(r.Out, "  Checking Gemini API tier...")
	baseURL := envBaseURLFn(r, baseURLEnv)
	if baseURL == "" {
		baseURL = pconfig.BaseURL
	}
	tier := probeGeminiTierFn(r.ctx(), existingKey, baseURL, "gemini-3.7-flash", 10*time.Second)
	if tier == "free" {
		flowSay(r.Out, geminiFreeTierNotice...)
		return false
	}
	if tier == "paid" {
		flowSay(r.Out, "  Tier check: paid ✓", "")
	} else {
		flowSay(r.Out, "  Tier check: could not verify (proceeding anyway).", "")
	}
	return true
}

// LM Studio catalog from the live probe only, an auth error explained and treated as empty
func lmstudioModels(r *ModelFlowRun, _ Profile, _ []string, apiKey, baseURL string) ([]string, error) {
	modelList, err := FetchLMStudioModels(r.ctx(), apiKey, baseURL, 5*time.Second)
	if err != nil {
		var authErr *AuthError
		if !errors.As(err, &authErr) {
			return nil, err
		}
		flowSay(r.Out, fmt.Sprintf("  LM Studio rejected the request: %s", authErr.Error()), "  Set LM_API_KEY (or update it) to match the server's bearer token.")
		modelList = []string{}
	}
	reportLiveModels(r.Out, modelList, "LM Studio")
	return modelList, nil
}

// Ollama Cloud catalog from a forced live refresh so new models appear at once
func ollamaCloudModels(r *ModelFlowRun, _ Profile, _ []string, apiKey, baseURL string) ([]string, error) {
	modelList := FetchOllamaCloudModels(r.ctx(), OllamaCloudFetch{APIKey: apiKey, BaseURL: baseURL, ForceRefresh: true})
	reportLiveModels(r.Out, modelList, "Ollama Cloud")
	return modelList, nil
}

// Novita catalog, live first, then models.dev, then curated
func novitaModels(r *ModelFlowRun, pconfig Profile, curated []string, apiKey, baseURL string) ([]string, error) {
	live, _ := flowFetchAPIModelsFn(r.ctx(), apiKey, baseURL, 5*time.Second, "", nil)
	if len(live) > 0 {
		reportLiveModels(r.Out, live, fmt.Sprintf("%s API", profileDisplayName(pconfig)))
		return live, nil
	}
	modelList := modelsDevMergedFn(r.ctx(), "novita", curated)
	if len(modelList) > 0 {
		reportLiveModels(r.Out, modelList, "models.dev registry")
		return modelList, nil
	}
	showCurated(r.Out, curated)
	return curated, nil
}

// Provider id to its own model list resolution over the curated list, probe key and effective base
var specialModelLists = map[string]func(r *ModelFlowRun, pconfig Profile, curated []string, apiKey, baseURL string) ([]string, error){
	"lmstudio":     lmstudioModels,
	"ollama-cloud": ollamaCloudModels,
	"novita":       novitaModels,
}

// Model list for an api key provider, models.dev then curated then the profile owned catalog or the generic probe
func apiKeyProviderModelList(r *ModelFlowRun, providerID string, pconfig Profile, existingKey, keyEnv, effectiveBase string) ([]string, error) {
	curated := StaticCatalog(r.ctx(), providerID)
	apiKeyForProbe := existingKey
	if apiKeyForProbe == "" && keyEnv != "" {
		apiKeyForProbe = r.envValue(keyEnv)
	}
	if special, ok := specialModelLists[providerID]; ok {
		return special(r, pconfig, curated, apiKeyForProbe, effectiveBase)
	}
	modelList := modelsDevMergedFn(r.ctx(), providerID, curated)
	if len(modelList) > 0 {
		reportLiveModels(r.Out, modelList, "models.dev registry")
		return modelList, nil
	}
	if len(curated) >= 8 {
		showCurated(r.Out, curated)
		return curated, nil
	}
	if profile := getProviderProfile(r.ctx(), providerID); profile != nil {
		rows, _ := ProbeProfileCatalog(r.ctx(), providerID, *profile, apiKeyForProbe, effectiveBase)
		if len(rows.IDs) > 0 {
			reportLiveModels(r.Out, rows.IDs, fmt.Sprintf("%s catalog", profileDisplayName(pconfig)))
			return rows.IDs, nil
		}
		showCurated(r.Out, curated)
		return append([]string{}, curated...), nil
	}
	live, _ := flowFetchAPIModelsFn(r.ctx(), apiKeyForProbe, effectiveBase, 5*time.Second, "", nil)
	if len(live) > 0 && len(live) >= len(curated) {
		reportLiveModels(r.Out, live, fmt.Sprintf("%s API", profileDisplayName(pconfig)))
		return live, nil
	}
	showCurated(r.Out, curated)
	return curated, nil
}

// Generic flow for api key providers, key, optional base URL, catalog, picker and persist
func modelFlowAPIKeyProvider(r *ModelFlowRun, providerID string) error {
	pconfig, err := r.registryProfile(providerID)
	if err != nil {
		return err
	}
	keyEnv := ""
	if len(pconfig.KeyEnv) > 0 {
		keyEnv = pconfig.KeyEnv[0]
	}
	baseURLEnv := pconfig.BaseURLEnv
	isOpencode := providerID == "opencode-zen" || providerID == "opencode-go"
	_, existingKey, abort, err := ensureFlowAPIKeyFn(r.Env, r.Out, providerID, pconfig, nil)
	if err != nil || abort {
		return err
	}
	if providerID == "gemini" && existingKey != "" && !geminiTierOK(r, existingKey, pconfig, baseURLEnv) {
		return nil
	}
	currentBase := envBaseURLFn(r, baseURLEnv)
	if currentBase == "" {
		if cfg, cerr := r.loadConfig(); cerr == nil {
			if m, ok := cfg["model"].(map[string]any); ok && pystr.Lower(pystr.Strip(pyOrStr(m["provider"]))) == providerID {
				currentBase = pystr.Strip(pyOrStr(m["base_url"]))
			}
		}
	}
	effectiveBase := currentBase
	if effectiveBase == "" {
		effectiveBase = pconfig.BaseURL
	}
	if providerID == "actual" {
		if modelCfg, ok := r.Config["model"].(map[string]any); ok && NormalizeProvider(pyOrStr(modelCfg["provider"])) == providerID {
			if configured := pystr.Strip(pyOrStr(modelCfg["base_url"])); configured != "" {
				effectiveBase = configured
			}
		}
	}
	if providerID == "zai" {
		chosenBase := selectZaiEndpoint(r, effectiveBase)
		if chosenBase != "" && chosenBase != effectiveBase && baseURLEnv != "" {
			if err := r.saveEnv(baseURLEnv, chosenBase); err != nil {
				return err
			}
		}
		effectiveBase = chosenBase
	} else {
		effectiveBase, err = promptBaseURLOverrideFn(r, effectiveBase, baseURLEnv, providerID != "actual")
		if err != nil {
			return err
		}
	}
	modelList, err := apiKeyProviderModelListFn(r, providerID, pconfig, existingKey, keyEnv, effectiveBase)
	if err != nil {
		return err
	}
	currentModel := r.CurrentModel
	if isOpencode {
		seen := map[string]bool{}
		deduped := []string{}
		for _, mid := range modelList {
			normalized := NormalizeOpenCodeModelID(providerID, mid)
			if normalized == "" || seen[normalized] {
				continue
			}
			seen[normalized] = true
			deduped = append(deduped, normalized)
		}
		modelList = deduped
		currentModel = NormalizeOpenCodeModelID(providerID, currentModel)
	}
	var pricing PricingTable
	if len(modelList) > 0 {
		func() {
			defer func() {
				if recover() != nil {
					pricing = nil
				}
			}()
			pricing = flowPricingFn(r.ctx(), providerID, "", false, false)
		}()
	}
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, modelList, "Model name: ",
		ModelSelectionPrompt{CurrentModel: currentModel, Pricing: pricing, ConfirmProvider: providerID, ConfirmBaseURL: effectiveBase, ConfirmAPIKey: existingKey})
	if selected != "" && isOpencode {
		selected = NormalizeOpenCodeModelID(providerID, selected)
	}
	opts := persistModelOptions{BaseURL: optStr(effectiveBase), DropAPIMode: !isOpencode}
	if selected != "" && isOpencode {
		opts.APIMode = optStr(string(OpenCodeModelMode(providerID, selected)))
	}
	_, err = finishModelFn(r.Env, r.Out, selected, providerID, fmt.Sprintf("Default model set to: %s (via %s)", selected, profileDisplayName(pconfig)), flowNoChange, opts)
	return err
}

// Interactive Anthropic auth by OAuth subscription or api key, false when the flow must stop
func anthropicAuthenticate(r *ModelFlowRun) (bool, error) {
	h := r.home()
	save := func(name, value string) error { return config.SaveEnvValue(h, name, value) }
	flowSay(r.Out, "", "  Choose authentication method:", "", "    1. Claude Pro/Max subscription (OAuth login)",
		"    2. Anthropic API key (pay-per-token)", "    3. Cancel", "")
	choice, ok := flowAskFn(r.Out, "  Choice [1/2/3]: ", flowAskOptions{Raw: true, CancelMsg: optStr("")})
	if !ok {
		return false, nil
	}
	switch choice {
	case "1":
		return runAnthropicOAuthFlow(r.ctx(), r.Env, r.Out, save)
	case "2":
		flowSay(r.Out, "", "  Get an API key at: https://platform.claude.com/settings/keys", "")
		apiKey, ok := flowAskFn(r.Out, "  API key (sk-ant-...): ", flowAskOptions{Secret: true, CancelMsg: optStr("")})
		if !ok {
			return false, nil
		}
		if apiKey == "" {
			fmt.Fprintln(r.Out, "  Cancelled.")
			return false, nil
		}
		if err := config.SaveAnthropicAPIKey(r.ctx(), h, apiKey, save); err != nil {
			return false, err
		}
		fmt.Fprintln(r.Out, "  ✓ API key saved.")
		return true, nil
	}
	fmt.Fprintln(r.Out, "  No change.")
	return false, nil
}

// Anthropic provider over OAuth subscription, api key or Claude Code credentials
func modelFlowAnthropic(r *ModelFlowRun) error {
	existingKey := GetAnthropicKey(r.ctx())
	ccAvailable := false
	func() {
		defer func() { _ = recover() }()
		ccCreds := anthReadClaudeCodeCredentialsFn(r.Env)
		if len(ccCreds) > 0 && anthIsClaudeCodeTokenValidFn(ccCreds) {
			ccAvailable = true
		}
	}()
	existingIsStaleOAuth := existingKey != "" && isOAuthToken(existingKey) && !ccAvailable
	hasCreds := (existingKey != "" && !existingIsStaleOAuth) || ccAvailable
	needsAuth := !hasCreds
	if hasCreds {
		if existingKey != "" {
			sourceSuffix := ""
			if pconfig, ok := registryProfileCtx(r.ctx(), "anthropic"); ok {
				for _, v := range pconfig.KeyEnv {
					if strings.TrimSpace(os.Getenv(v)) == existingKey {
						sourceSuffix = FormatSecretSourceSuffix(r.home(), v)
						if sourceSuffix != "" {
							break
						}
					}
				}
			}
			fmt.Fprintf(r.Out, "  Anthropic credentials: %s... ✓%s\n", prefixRunes(existingKey, 12), sourceSuffix)
		} else if ccAvailable {
			fmt.Fprintln(r.Out, "  Claude Code credentials: ✓ (auto-detected)")
		}
		fmt.Fprintln(r.Out)
		switch promptAuthCredentialsChoice(r.Out, "Anthropic credentials:") {
		case "reauth":
			needsAuth = true
		case "cancel":
			return nil
		}
	}
	if needsAuth {
		ok, err := anthropicAuthenticate(r)
		if err != nil || !ok {
			return err
		}
	}
	fmt.Fprintln(r.Out)
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, providerModels["anthropic"], "Model name (e.g., claude-sonnet-4-20250514): ",
		ModelSelectionPrompt{CurrentModel: r.CurrentModel, ConfirmProvider: "anthropic"})
	_, err := finishModelFn(r.Env, r.Out, selected, "anthropic", fmt.Sprintf("Default model set to: %s (via Anthropic)", selected), flowNoChange,
		persistModelOptions{DropBaseURL: true, DropAPIMode: true})
	return err
}

// Setup status of a profile through its hook, nil when the profile has none
func profileSetupStatus(ctx context.Context, profile Profile) (map[string]any, error) {
	if profile.SetupStatusHook == nil {
		return nil, nil
	}
	return profile.SetupStatusHook(ctx, nil)
}

// Shell quoted join of a command's words
func shlexJoin(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = pyshlex.Quote(w)
	}
	return strings.Join(quoted, " ")
}

// Logged in shows the plan, logged out runs the CLI login on a TTY or prints the instruction
func externalProcessLoginGate(r *ModelFlowRun, profile Profile, status map[string]any) (bool, error) {
	if truthy(status["logged_in"]) {
		plan := ""
		if truthy(status["plan"]) {
			plan = fmt.Sprintf(" (%s)", anyString(status["plan"]))
		}
		flowSay(r.Out, fmt.Sprintf("  %s credentials: ✓%s", profile.DisplayName, plan), "")
		return true, nil
	}
	login := anyStrings(status["login_command"])
	detail := anyString(status["detail"])
	if len(login) == 0 || !flowStdinIsTTYFn() {
		flowSay(r.Out, fmt.Sprintf("  ✗ %s", detail))
		return false, nil
	}
	tail := login
	if len(tail) > 2 {
		tail = login[len(login)-2:]
	}
	flowSay(r.Out, fmt.Sprintf("  %s.", strings.SplitN(detail, ".", 2)[0]), fmt.Sprintf("  Starting `%s` (press Ctrl-C to cancel)...", shlexJoin(tail)), "")
	if !runLoginCommand(r.ctx(), login) {
		fmt.Fprintln(r.Out, "Login cancelled or failed.")
		return false, nil
	}
	probe, err := profileSetupStatus(r.ctx(), profile)
	if err != nil {
		return false, err
	}
	if !truthy(probe["logged_in"]) {
		fmt.Fprintln(r.Out, "Login failed.")
		noteSetupFailure("auth")
		return false, nil
	}
	flowSay(r.Out, "", fmt.Sprintf("  %s credentials: ✓", profile.DisplayName), "")
	return true, nil
}

// Runs the CLI login on the terminal, false when it could not start or Ctrl-C interrupted it
func runLoginCommand(ctx context.Context, login []string) bool {
	cmd := exec.CommandContext(ctx, login[0], login[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-interrupts:
		_ = cmd.Process.Kill()
		<-done
		return false
	case err := <-done:
		var exitErr *exec.ExitError
		return err == nil || errors.As(err, &exitErr)
	}
}

// pluginFlowCredentials is the base URL and api key a plugin flow's credential step produced
type pluginFlowCredentials struct {
	baseURL string
	apiKey  string
}

// External process provider credential step, the CLI described, resolved and gated on its login
func pluginFlowExternalProcess(r *ModelFlowRun, providerID string, profile Profile) (*pluginFlowCredentials, error) {
	status := GetExternalProcessProviderStatus(r.Env, providerID)
	name := profile.DisplayName
	if name == "" {
		name = providerID
	}
	command := anyString(status["command"])
	if command == "" {
		command = profile.ProcessCommand
	}
	resolved := anyString(status["resolved_command"])
	if resolved == "" {
		resolved = anyString(status["command"])
	}
	if resolved == "" {
		resolved = "(not found)"
	}
	marker := anyString(status["base_url"])
	if marker == "" {
		marker = profile.BaseURL
	}
	flowSay(r.Out, fmt.Sprintf("  %s delegates Hermes turns to a local `%s` process.", name, command),
		fmt.Sprintf("  Command: %s", resolved), fmt.Sprintf("  Backend marker: %s", marker), "")
	runtime, err := authResolver(r.Env).resolveExternalProcessRuntime(providerID, providerID)
	if err != nil {
		flowSay(r.Out, fmt.Sprintf("  ⚠ %s", err.Error()))
		return nil, nil
	}
	probe, err := profileSetupStatus(r.ctx(), profile)
	if err != nil {
		return nil, err
	}
	if probe != nil {
		if !truthy(probe["available"]) {
			flowSay(r.Out, fmt.Sprintf("  ✗ %s", anyString(probe["detail"])))
			return nil, nil
		}
		ok, gerr := externalProcessLoginGate(r, profile, probe)
		if gerr != nil || !ok {
			return nil, gerr
		}
	}
	baseURL := runtime.BaseURL
	if baseURL == "" {
		baseURL = profile.BaseURL
	}
	return &pluginFlowCredentials{baseURL: baseURL}, nil
}

// OAuth plugin provider credential step, signed in through the pool or the auth status
func pluginFlowOAuth(r *ModelFlowRun, providerID string, profile Profile) (*pluginFlowCredentials, error) {
	status := GetAuthStatus(r.Env, providerID)
	if !truthy(status["logged_in"]) {
		name := profile.DisplayName
		if name == "" {
			name = providerID
		}
		hint := anyString(status["hint"])
		if missing := pluginMissingAuthHandlerError(providerID, "add"); missing != nil {
			hint = missing.Error()
		} else if hint == "" {
			hint = fmt.Sprintf("Run `icebath auth add %s` to sign in.", providerID)
		}
		flowSay(r.Out, fmt.Sprintf("  ⚠ Not signed in to %s.", name), fmt.Sprintf("  %s", hint))
		return nil, nil
	}
	baseURL, apiKey := "", ""
	if entry, ok := flowLoadPoolFn(r.Env, providerID).Select(""); ok {
		baseURL, apiKey = entry.RuntimeBaseURL(), entry.RuntimeAPIKey()
	}
	if baseURL == "" {
		baseURL = anyString(status["base_url"])
	}
	if baseURL == "" {
		baseURL = profile.BaseURL
	}
	return &pluginFlowCredentials{baseURL: baseURL, apiKey: apiKey}, nil
}

// Auth type to the credential step of the generic plugin flow
var pluginFlowCredentialSteps = map[string]func(r *ModelFlowRun, providerID string, profile Profile) (*pluginFlowCredentials, error){
	AuthTypeExternalProcess: pluginFlowExternalProcess,
	AuthTypeOAuthDeviceCode: pluginFlowOAuth,
	AuthTypeOAuthExternal:   pluginFlowOAuth,
}

// The registered profile's own model setup flow, nil when the profile lacks one
func profileModelSetupFlow(ctx context.Context, providerID string) func(ctx context.Context, cfg any, currentModel string, args any) (any, error) {
	profile := getProviderProfile(ctx, providerID)
	if profile == nil {
		return nil
	}
	return profile.ModelSetupFlow
}

// True when the id is a registered profile whose auth type the generic plugin flow handles
func isProfilePluginFlowProvider(providerID string) bool {
	profile, ok := pluginProfile(providerID)
	if !ok {
		return false
	}
	_, handled := pluginFlowCredentialSteps[registryAuthType(profile)]
	return handled
}

// Live ids and per id notes, the profile's annotated discovery winning over its plain fetch
func pluginFlowLiveRows(r *ModelFlowRun, profile Profile, apiKey, baseURL string) ([]string, map[string]string) {
	if profile.DiscoverModelsHook != nil {
		rows, err := profile.DiscoverModelsHook(r.ctx(), nil)
		if err == nil && len(rows) > 0 {
			name := profile.DisplayName
			if name == "" {
				name = profile.Name
			}
			flowSay(r.Out, fmt.Sprintf("  Models below come from your %s account.", name), "")
			ids := make([]string, 0, len(rows))
			notes := map[string]string{}
			for _, row := range rows {
				id := anyString(row["id"])
				ids = append(ids, id)
				if truthy(row["note"]) {
					notes[id] = anyString(row["note"])
				}
			}
			return ids, notes
		}
	}
	var live []string
	func() {
		defer func() { _ = recover() }()
		if ids, ok := profileFetchModelsFn(r.ctx(), profile, apiKey, baseURL, 0); ok {
			live = ids
		}
	}()
	return live, map[string]string{}
}

// Generic flow for a registered plugin profile, credential step by auth type, merged catalog, persist
func modelFlowPluginProvider(r *ModelFlowRun, providerID string) error {
	profile, ok := pluginProfile(providerID)
	if !ok {
		return fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'auth_type'")
	}
	step, handled := pluginFlowCredentialSteps[registryAuthType(profile)]
	if !handled {
		return fmt.Errorf("KeyError: %q", registryAuthType(profile))
	}
	creds, err := step(r, providerID, profile)
	if err != nil || creds == nil {
		return err
	}
	live, notes := pluginFlowLiveRows(r, profile, creds.apiKey, creds.baseURL)
	rows, _ := MergeProfileCatalog(r.ctx(), providerID, profile, live)
	modelList := rows.IDs
	if modelList == nil {
		modelList = []string{}
	}
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, modelList, "Model name: ",
		ModelSelectionPrompt{CurrentModel: r.CurrentModel, ConfirmProvider: providerID, ConfirmBaseURL: creds.baseURL, ConfirmAPIKey: creds.apiKey, Notes: notes})
	name := profile.DisplayName
	if name == "" {
		name = providerID
	}
	opts := persistModelOptions{}
	if creds.baseURL != "" {
		opts.BaseURL = optStr(creds.baseURL)
	}
	if string(profile.Mode) != "" {
		opts.APIMode = optStr(string(profile.Mode))
	}
	_, err = finishModelFn(r.Env, r.Out, selected, providerID, fmt.Sprintf("Default model set to: %s (via %s)", selected, name), flowNoChange, opts)
	return err
}
