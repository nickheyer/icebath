package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Azure flow seams, swappable so tests script the credential probe, the endpoint detection and the context lookup
var (
	azureDescribeCredentialFn = DescribeActiveEntraCredential
	azureDetectFn             = DetectAzureEndpoint
	azureContextLengthFn      = AzureLookupContextLength
)

// OpenAI-style for chat completions, Anthropic-style otherwise
func azureModeLabel(mode string) string {
	if mode == "chat_completions" {
		return "OpenAI-style"
	}
	return "Anthropic-style"
}

// azureCurrent is the active Azure Foundry settings shown as prompt defaults
type azureCurrent struct {
	baseURL  string
	apiMode  string
	authMode string
	entra    map[string]any
	apiKey   string
}

// Active Azure Foundry settings from the config model section and the .env key
func azureCurrentSettings(r *ModelFlowRun) azureCurrent {
	cur := azureCurrent{authMode: "api_key", entra: map[string]any{}, apiKey: r.envValue("AZURE_FOUNDRY_API_KEY")}
	if modelCfg, ok := r.Config["model"].(map[string]any); ok && modelCfg["provider"] == "azure-foundry" {
		cur.baseURL = pyOrStr(modelCfg["base_url"])
		cur.apiMode = pyOrStr(modelCfg["api_mode"])
		cur.authMode = "api_key"
		if pyTruthy(modelCfg["auth_mode"]) {
			cur.authMode = pyStr(modelCfg["auth_mode"])
		}
		if cur.authMode = pystr.Lower(pystr.Strip(cur.authMode)); cur.authMode == "" {
			cur.authMode = "api_key"
		}
		if entra, ok := modelCfg["entra"].(map[string]any); ok {
			cur.entra = entra
		}
	}
	return cur
}

// azureEntraPreflight is the preflight outcome, cancelled when the user stopped, fallback when the adapter is missing
type azureEntraPreflight struct {
	tokenProvider  func(ctx context.Context) (string, error)
	entraOverrides map[string]any
	cancelled      bool
}

// Entra ID credential preflight, the token provider and the persisted scope override
func azureEntraPreflightStep(r *ModelFlowRun, currentEntra map[string]any) azureEntraPreflight {
	fmt.Fprintln(r.Out)
	entraOverrides := map[string]any{}
	persistedScopeOverride := pystr.Strip(pyOrStr(currentEntra["scope"]))
	entraScope := persistedScopeOverride
	if entraScope == "" {
		entraScope = ScopeAIAzureDefault
	}
	if persistedScopeOverride != "" {
		entraOverrides["scope"] = persistedScopeOverride
	}
	flowSay(r.Out, "", "◐ Probing Microsoft Entra ID credential chain (up to 10s)...")
	cfg := NewEntraIdentityConfig(entraScope, true)
	info := azureDescribeCredentialFn(r.ctx(), &cfg, entraScope, 10*time.Second)
	if truthy(info["ok"]) {
		envSources := anyStrings(info["env_sources"])
		tag := "default chain"
		if len(envSources) > 0 {
			tag = strings.Join(envSources, ", ")
		}
		fmt.Fprintf(r.Out, "✓ Entra ID token acquired (%s, scope=%s)\n", tag, entraScope)
	} else {
		errText := anyString(info["error"])
		if errText == "" {
			errText = "credential chain exhausted"
		}
		hint := anyString(info["hint"])
		if hint == "" {
			hint = "Run `az login`, attach a managed identity to this VM, or set AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET."
		}
		flowSay(r.Out, fmt.Sprintf("⚠ %s", errText), fmt.Sprintf("  Hint: %s", hint))
		ans, ok := flowAskFn(r.Out, "Save Entra config anyway and validate later? [Y/n]: ", flowAskOptions{Raw: true})
		if !ok {
			return azureEntraPreflight{cancelled: true}
		}
		switch pystr.Lower(ans) {
		case "", "y", "yes":
		default:
			fmt.Fprintln(r.Out, "Cancelled.")
			return azureEntraPreflight{cancelled: true}
		}
	}
	var tokenProvider func(ctx context.Context) (string, error)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Fprintf(r.Out, "⚠ Could not build token provider for probing: %v\n", rec)
				tokenProvider = nil
			}
		}()
		built, err := BuildEntraTokenProviderIn(r.Ctx, &cfg, entraScope, true)
		if err != nil {
			fmt.Fprintf(r.Out, "⚠ Could not build token provider for probing: %v\n", err)
			return
		}
		tokenProvider = built
	}()
	return azureEntraPreflight{tokenProvider: tokenProvider, entraOverrides: entraOverrides}
}

// Model or deployment step of the Azure flow, false when cancelled, the ValueError of a digit int rejects
func azurePickModel(r *ModelFlowRun, discoveredModels []string, currentModel string) (string, bool, error) {
	if len(discoveredModels) == 0 {
		shown := currentModel
		if shown == "" {
			shown = "e.g. gpt-5.4, claude-sonnet-4-6"
		}
		modelName, ok := flowAskFn(r.Out, fmt.Sprintf("Model / deployment name [%s]: ", shown), flowAskOptions{})
		if !ok {
			return "", false, nil
		}
		if modelName == "" {
			return currentModel, true, nil
		}
		return modelName, true, nil
	}
	fmt.Fprintln(r.Out, "Available models on this endpoint:")
	shownCount := min(len(discoveredModels), 30)
	for i, mid := range discoveredModels[:shownCount] {
		fmt.Fprintf(r.Out, "  %2d. %s\n", i+1, mid)
	}
	if len(discoveredModels) > 30 {
		fmt.Fprintf(r.Out, "  ... and %d more (type name manually if not shown)\n", len(discoveredModels)-30)
	}
	fmt.Fprintln(r.Out)
	def := currentModel
	if def == "" {
		def = discoveredModels[0]
	}
	pick, ok := flowAskFn(r.Out, fmt.Sprintf("Pick by number, or type a deployment name [%s]: ", def), flowAskOptions{Raw: true})
	if !ok {
		return "", false, nil
	}
	if pick == "" {
		return def, true, nil
	}
	if pystr.StrIsDigit(pick) {
		n, err := parsePyInt(pick)
		if err != nil {
			return "", false, err
		}
		if n >= 1 && n <= shownCount {
			return discoveredModels[n-1], true, nil
		}
	}
	return pick, true, nil
}

// Probes the endpoint for the transport and models, a manual api format pick when detection is incomplete
func azureDetectTransport(r *ModelFlowRun, effectiveURL, effectiveKey string, tokenProvider func(ctx context.Context) (string, error), currentAPIMode string) (string, []string, bool) {
	flowSay(r.Out, "", "◐ Probing endpoint to auto-detect transport and models...")
	detection := azureDetectFn(r.ctx(), effectiveURL, effectiveKey, tokenProvider)
	discovered := append([]string{}, detection.Models...)
	apiMode := string(detection.APIMode)
	if apiMode != "" {
		fmt.Fprintf(r.Out, "✓ Detected API transport: %s\n", azureModeLabel(apiMode))
		if detection.Reason != "" {
			fmt.Fprintf(r.Out, "    (%s)\n", detection.Reason)
		}
		if len(discovered) > 0 {
			fmt.Fprintf(r.Out, "✓ Found %d deployed model(s) on this endpoint\n", len(discovered))
		}
		return apiMode, discovered, true
	}
	flowSay(r.Out, fmt.Sprintf("⚠ Auto-detection incomplete: %s", detection.Reason), "",
		"Select the API format your Azure Foundry endpoint uses:",
		"  1. OpenAI-style  (POST /v1/chat/completions)",
		"     For: GPT models, Llama, Mistral, and most open models",
		"  2. Anthropic-style  (POST /v1/messages)",
		"     For: Claude models deployed via Anthropic API format")
	defaultChoice := "1"
	if currentAPIMode == "anthropic_messages" {
		defaultChoice = "2"
	}
	modeChoice, ok := flowAskFn(r.Out, fmt.Sprintf("API format [1/2] (%s): ", defaultChoice), flowAskOptions{Raw: true})
	if !ok {
		return "", nil, false
	}
	if modeChoice == "" {
		modeChoice = defaultChoice
	}
	if modeChoice == "2" {
		return "anthropic_messages", discovered, true
	}
	return "chat_completions", discovered, true
}

// Azure Foundry wizard, endpoint, auth mode, transport detection, model, context length and persist
func modelFlowAzureFoundry(r *ModelFlowRun) error {
	cur := azureCurrentSettings(r)
	flowSay(r.Out, "", "Azure Foundry Configuration", strings.Repeat("=", 50), "",
		"Azure Foundry can host models with either OpenAI-style or",
		"Anthropic-style API endpoints.  Hermes will probe your",
		"endpoint to auto-detect the transport and the deployed",
		"models when possible.", "")
	if cur.baseURL != "" {
		fmt.Fprintf(r.Out, "  Current endpoint:  %s\n", cur.baseURL)
	}
	if cur.apiMode != "" {
		fmt.Fprintf(r.Out, "  Current API mode:  %s\n", azureModeLabel(cur.apiMode))
	}
	if cur.authMode == "entra_id" {
		fmt.Fprintln(r.Out, "  Current auth mode: Microsoft Entra ID (keyless)")
	} else if cur.apiKey != "" {
		fmt.Fprintf(r.Out, "  Current auth mode: API key (%s...)\n", prefixRunes(cur.apiKey, 8))
	}
	fmt.Fprintln(r.Out)
	placeholder := cur.baseURL
	if placeholder == "" {
		placeholder = "e.g. https://<resource>.openai.azure.com/openai/v1 or https://<resource>.services.ai.azure.com/anthropic"
	}
	baseURL, ok := flowAskFn(r.Out, fmt.Sprintf("API endpoint URL [%s]: ", placeholder), flowAskOptions{})
	if !ok {
		return nil
	}
	if baseURL == "" {
		baseURL = cur.baseURL
	}
	effectiveURL := strings.TrimRight(baseURL, "/")
	if effectiveURL == "" {
		fmt.Fprintln(r.Out, "No endpoint URL provided. Cancelled.")
		return nil
	}
	if !hasHTTPPrefix(effectiveURL) {
		fmt.Fprintf(r.Out, "Invalid URL: %s (must start with http:// or https://)\n", effectiveURL)
		return nil
	}
	flowSay(r.Out, "", "Authentication:", "  1. API key                  (AZURE_FOUNDRY_API_KEY in .env)",
		"  2. Microsoft Entra ID       (managed identity / workload identity / az login)",
		"     Recommended by Microsoft. Works for both OpenAI-style and Anthropic-style endpoints.",
		"     Requires the 'Azure AI User' role on the Foundry resource.")
	authDefault := "1"
	if cur.authMode == "entra_id" {
		authDefault = "2"
	}
	authChoice, ok := flowAskFn(r.Out, fmt.Sprintf("Authentication mode [1/2] (%s): ", authDefault), flowAskOptions{Raw: true})
	if !ok {
		return nil
	}
	if authChoice == "" {
		authChoice = authDefault
	}
	useEntra := authChoice == "2"
	effectiveKey := ""
	entraOverrides := map[string]any{}
	var tokenProvider func(ctx context.Context) (string, error)
	if useEntra {
		preflight := azureEntraPreflightStep(r, cur.entra)
		if preflight.cancelled {
			return nil
		}
		tokenProvider, entraOverrides = preflight.tokenProvider, preflight.entraOverrides
	}
	if !useEntra {
		fmt.Fprintln(r.Out)
		shown := "required"
		if cur.apiKey != "" {
			shown = prefixRunes(cur.apiKey, 8) + "..."
		}
		apiKey, ok := flowAskFn(r.Out, fmt.Sprintf("API key [%s]: ", shown), flowAskOptions{Secret: true})
		if !ok {
			return nil
		}
		effectiveKey = apiKey
		if effectiveKey == "" {
			effectiveKey = cur.apiKey
		}
		if effectiveKey == "" {
			fmt.Fprintln(r.Out, "No API key provided. Cancelled.")
			return nil
		}
	}
	apiMode, discovered, ok := azureDetectTransport(r, effectiveURL, effectiveKey, tokenProvider, cur.apiMode)
	if !ok {
		return nil
	}
	fmt.Fprintln(r.Out)
	effectiveModel, ok, err := azurePickModel(r, discovered, r.CurrentModel)
	if err != nil || !ok {
		return err
	}
	if effectiveModel == "" {
		fmt.Fprintln(r.Out, "No model name provided. Cancelled.")
		return nil
	}
	ctxLen := azureContextLengthFn(r.ctx(), effectiveModel, effectiveURL, effectiveKey, tokenProvider)
	if !useEntra {
		if err := r.saveEnv("AZURE_FOUNDRY_API_KEY", effectiveKey); err != nil {
			return err
		}
	}
	cfg, model, err := loadConfigModelSection(r.home())
	if err != nil {
		return err
	}
	pyDictSet(model, "provider", "azure-foundry")
	pyDictSet(model, "base_url", effectiveURL)
	pyDictSet(model, "api_mode", apiMode)
	pyDictSet(model, "default", effectiveModel)
	authMode := "api_key"
	if useEntra {
		authMode = "entra_id"
	}
	pyDictSet(model, "auth_mode", authMode)
	clearModelEndpointCredentials(model, true, false, false)
	cleanEntra := map[string]any{}
	if scope := anyString(entraOverrides["scope"]); scope != "" {
		cleanEntra["scope"] = scope
	}
	if useEntra && len(cleanEntra) > 0 {
		pyDictSet(model, "entra", cleanEntra)
	} else {
		delete(model, "entra")
	}
	if ctxLen != 0 {
		pyDictSet(model, "context_length", ctxLen)
	}
	if err := commitModelConfig(r.Env, cfg); err != nil {
		return err
	}
	if r.Config != nil {
		copied := map[string]any{}
		for k, v := range model {
			copied[k] = v
		}
		config.SetMappingKeys(copied, config.MappingKeys(model))
		pyDictSet(r.Config, "model", copied)
	}
	for _, v := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY"} {
		if r.envValue(v) != "" {
			if err := r.saveEnv(v, ""); err != nil {
				return err
			}
		}
	}
	authLabel := "API key"
	if useEntra {
		authLabel = "Microsoft Entra ID (keyless)"
	}
	ctxLine := "    Context length: not auto-detected (will fall back at runtime)"
	if ctxLen != 0 {
		ctxLine = fmt.Sprintf("    Context length: %s tokens", pystr.GroupThousands(int64(ctxLen)))
	}
	flowSay(r.Out, "", "✓ Azure Foundry configured:", fmt.Sprintf("    Endpoint:       %s", effectiveURL),
		fmt.Sprintf("    API mode:       %s", azureModeLabel(apiMode)),
		fmt.Sprintf("    Auth:           %s", authLabel),
		fmt.Sprintf("    Model:          %s", effectiveModel),
		ctxLine, "")
	return nil
}
