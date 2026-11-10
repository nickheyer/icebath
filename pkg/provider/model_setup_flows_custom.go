package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// errFlowCancelled is the Ctrl-C or EOF a wrapped prompt raised
var errFlowCancelled = errors.New("KeyboardInterrupt")

// Custom flow seams, swappable so tests script the endpoint probe and the context length resolver
var (
	customProbeAPIModelsFn         = ProbeAPIModels
	promptCustomAPIModeSelectionFn = promptCustomAPIModeSelection
	saveCustomProviderFn           = saveCustomProvider
	pickNamedCustomModelFn         = pickNamedCustomModel
	customContextLengthFn          = func(ctx context.Context, req ContextLengthRequest) (int, error) {
		return GetModelContextLength(ctx, req), nil
	}
)

// customAPIMode is one row of the custom provider api mode menu, an empty value meaning auto detect
type customAPIMode struct {
	value, label, description string
	answers                   []string
}

var customAPIModes = []customAPIMode{
	{"", "Auto-detect", "Use Hermes URL heuristics; best for standard OpenAI-compatible endpoints.", []string{"1", "auto", "detect", "auto-detect"}},
	{"chat_completions", "Chat Completions", "Use /chat/completions for standard OpenAI-compatible servers.", []string{"2", "chat", "chat_completions", "completions"}},
	{"codex_responses", "Responses / Codex", "Use /responses for Codex-compatible tool-calling backends.", []string{"3", "responses", "codex", "codex_responses"}},
	{"anthropic_messages", "Anthropic Messages", "Use /v1/messages for Anthropic-compatible endpoints.", []string{"4", "anthropic", "anthropic_messages", "messages"}},
}

var trailingV1 = regexp.MustCompile(`/v1/?(\n?)\z`)

// Host and path of a URL without the scheme or trailing slash for menu labels
func shortURL(u string) string {
	return strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(u, "https://", ""), "http://", ""), "/")
}

// Display name from a custom endpoint URL, Local or RunPod labelled by host
func autoProviderName(baseURL string) string {
	name := strings.SplitN(trailingV1.ReplaceAllString(shortURL(baseURL), "${1}"), "/", 2)[0]
	if strings.Contains(name, "localhost") || strings.Contains(name, "127.0.0.1") {
		return fmt.Sprintf("Local (%s)", name)
	}
	if strings.Contains(strings.ToLower(name), "runpod") {
		return fmt.Sprintf("RunPod (%s)", name)
	}
	return pystr.Capitalize(name)
}

// Custom provider api mode from the menu, empty for auto detect, an error on Ctrl-C or EOF
func promptCustomAPIModeSelection(r *ModelFlowRun, baseURL, currentAPIMode string) (string, error) {
	detectedMode := string(detectAPIModeForURL(baseURL))
	defaultMode := pystr.Lower(pystr.Strip(currentAPIMode))
	if defaultMode == "" {
		defaultMode = detectedMode
	}
	flowSay(r.Out, "", "Select API compatibility mode:")
	for idx, mode := range customAPIModes {
		markers := []string{}
		if detectedMode != "" && mode.value == detectedMode {
			markers = append(markers, "detected")
		}
		if mode.value == defaultMode {
			markers = append(markers, "current")
		}
		suffix := ""
		if len(markers) > 0 {
			suffix = fmt.Sprintf(" [%s]", strings.Join(markers, " / "))
		}
		flowSay(r.Out, fmt.Sprintf("  %d. %s%s", idx+1, mode.label, suffix), fmt.Sprintf("     %s", mode.description))
	}
	raw, err := flowRawInputFn(r.Out, "Choice [1-4, Enter to keep current/detected]: ")
	if err != nil {
		fmt.Fprintln(r.Out, "\nCancelled.")
		return "", errFlowCancelled
	}
	raw = pystr.Lower(pystr.Strip(raw))
	if raw == "" {
		return defaultMode, nil
	}
	for _, mode := range customAPIModes {
		for _, answer := range mode.answers {
			if raw == answer {
				return mode.value, nil
			}
		}
	}
	fmt.Fprintf(r.Out, "Invalid API mode choice: %s. Falling back to auto-detect.\n", raw)
	return "", nil
}

// The value persisted for a custom provider key, the reference, the key env template or the resolved key
func customProviderAPIKeyConfigValue(providerInfo map[string]any, resolvedAPIKey string) string {
	if ref := pystr.Strip(pyOrStr(providerInfo["api_key_ref"])); ref != "" {
		return ref
	}
	keyEnv := pystr.Strip(pyOrStr(providerInfo["key_env"]))
	if keyEnv != "" && pystr.Strip(pyOrStr(providerInfo["api_key"])) == "" {
		return "${" + keyEnv + "}"
	}
	return pystr.Strip(resolvedAPIKey)
}

// The value persisted for a custom provider URL, the reference when one exists
func customProviderBaseURLConfigValue(providerInfo map[string]any, resolvedBaseURL string) string {
	if ref := pystr.Strip(pyOrStr(providerInfo["base_url_ref"])); ref != "" {
		return ref
	}
	return pystr.Strip(resolvedBaseURL)
}

// Saves a custom endpoint to custom_providers deduplicated by base URL, updating an existing entry
func saveCustomProvider(r *ModelFlowRun, baseURL, apiKey, model string, contextLength int, name, apiMode, keyEnv string) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	providers, _ := cfg["custom_providers"].([]any)
	if !truthy(cfg["custom_providers"]) || providers == nil {
		providers = []any{}
	}
	for _, raw := range providers {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		entryBase, err := pyStrReceiver(dictGet(entry, "base_url", ""), "rstrip")
		if err != nil {
			return err
		}
		if strings.TrimRight(entryBase, "/") != strings.TrimRight(baseURL, "/") {
			continue
		}
		changed := false
		if model != "" && entry["model"] != model {
			pyDictSet(entry, "model", model)
			changed = true
		}
		if model != "" && contextLength != 0 {
			pyDictSet(ensureDictSection(entry, "models"), model, contextLengthEntry(contextLength))
			changed = true
		}
		if apiMode != "" {
			if entry["api_mode"] != apiMode {
				pyDictSet(entry, "api_mode", apiMode)
				changed = true
			}
		} else if _, held := entry["api_mode"]; held {
			delete(entry, "api_mode")
			changed = true
		}
		if keyEnv != "" && (entry["key_env"] != keyEnv || truthy(entry["api_key"])) {
			pyDictSet(entry, "key_env", keyEnv)
			delete(entry, "api_key")
			changed = true
		}
		if changed {
			pyDictSet(cfg, "custom_providers", providers)
			return config.Save(r.home(), cfg)
		}
		return nil
	}
	if name == "" {
		name = autoProviderName(baseURL)
	}
	entry := map[string]any{"name": name, "base_url": baseURL}
	config.SetMappingKeys(entry, []string{"name", "base_url"})
	if keyEnv != "" {
		pyDictSet(entry, "key_env", keyEnv)
	} else if apiKey != "" {
		pyDictSet(entry, "api_key", apiKey)
	}
	if model != "" {
		pyDictSet(entry, "model", model)
	}
	if apiMode != "" {
		pyDictSet(entry, "api_mode", apiMode)
	}
	if model != "" && contextLength != 0 {
		models := map[string]any{}
		pyDictSet(models, model, contextLengthEntry(contextLength))
		pyDictSet(entry, "models", models)
	}
	providers = append(providers, entry)
	pyDictSet(cfg, "custom_providers", providers)
	if err := config.Save(r.home(), cfg); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "  💾 Saved to custom providers as \"%s\" (edit in config.yaml)\n", name)
	return nil
}

// Python str receiver of a method call, an AttributeError for a value that is no str
func pyStrReceiver(v any, method string) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("AttributeError: '%s' object has no attribute '%s'", config.PyTypeName(v), method)
}

// The models entry of one context length
func contextLengthEntry(contextLength int) map[string]any {
	return map[string]any{"context_length": contextLength}
}

// Token count for display, 128000 as 128K and 1048576 as 1M
func formatContextLength(tokens int) string {
	for _, unit := range []struct {
		suffix string
		div    float64
	}{{"M", 1_000_000}, {"K", 1_000}} {
		if float64(tokens) >= unit.div {
			val := float64(tokens) / unit.div
			rounded := math.RoundToEven(val)
			if math.Abs(val-rounded) < 0.05 {
				return fmt.Sprintf("%d%s", int(rounded), unit.suffix)
			}
			return fmt.Sprintf("%.1f%s", val, unit.suffix)
		}
	}
	return fmt.Sprint(tokens)
}

// 128k or 128,000 as an int, false when blank, non positive or unparsable with a warning
func parseContextLength(r *ModelFlowRun, text string) (int, bool) {
	if text == "" {
		return 0, false
	}
	cleaned := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text, ",", ""), "k", "000"), "K", "000")
	value, err := parsePyInt(cleaned)
	if err != nil {
		fmt.Fprintf(r.Out, "Invalid context length: %s — will auto-detect.\n", text)
		return 0, false
	}
	if value > 0 {
		return value, true
	}
	return 0, false
}

// Tells what the auto detect resolver found for the model at the base URL, silent when the probe fails
func reportContextLengthDetection(r *ModelFlowRun, modelName, baseURL, apiKey string) {
	var detected int
	var err error
	func() {
		defer func() {
			if recover() != nil {
				err = errors.New("probe failed")
			}
		}()
		detected, err = customContextLengthFn(r.ctx(), ContextLengthRequest{Model: modelName, BaseURL: baseURL, APIKey: apiKey})
	}()
	if err != nil {
		return
	}
	if detected > 0 && detected != DefaultFallbackContext {
		fmt.Fprintf(r.Out, "  Context length auto-detected: %s tokens\n", formatContextLength(detected))
		return
	}
	fmt.Fprintf(r.Out, "  Context length: not detected — using the default %s tokens (set model.context_length in config.yaml to override)\n",
		formatContextLength(DefaultFallbackContext))
}

// Verifies a custom endpoint through the models probe and reports, the working fallback base replacing the URL
func probeCustomEndpoint(r *ModelFlowRun, effectiveKey, effectiveURL string) (ProbeResult, string) {
	probe := customProbeAPIModelsFn(r.ctx(), effectiveKey, effectiveURL, 5*time.Second, "", nil)
	switch {
	case probe.UsedFallback && probe.ResolvedBaseURL != "":
		fmt.Fprintf(r.Out, "Warning: endpoint verification worked at %s/models, not the exact URL you entered. Saving the working base URL instead.\n", probe.ResolvedBaseURL)
		effectiveURL = probe.ResolvedBaseURL
	case probe.Found:
		fmt.Fprintf(r.Out, "Verified endpoint via %s (%d model(s) visible)\n", probe.ProbedURL, len(probe.Models))
	default:
		fmt.Fprintf(r.Out, "Warning: could not verify this endpoint via %s. Hermes will still save it.\n", probe.ProbedURL)
		if suggested := probe.SuggestedBaseURL; suggested != "" {
			if strings.HasSuffix(suggested, "/v1") {
				fmt.Fprintf(r.Out, "  If this server expects /v1 in the path, try base URL: %s\n", suggested)
			} else {
				fmt.Fprintf(r.Out, "  If /v1 should not be in the base URL, try: %s\n", suggested)
			}
		}
	}
	return probe, effectiveURL
}

// Model name step of the custom flow, confirm one detection, number pick from several, or type one
func pickDetectedModel(r *ModelFlowRun, detected []string) (string, error) {
	manual := "Model name (e.g. gpt-4, llama-3-70b): "
	if len(detected) == 1 {
		fmt.Fprintf(r.Out, "  Detected model: %s\n", detected[0])
		answer, err := flowRawInputFn(r.Out, "  Use this model? [Y/n]: ")
		if err != nil {
			return "", errFlowCancelled
		}
		switch pystr.Lower(pystr.Strip(answer)) {
		case "", "y", "yes":
			return detected[0], nil
		}
		typed, err := flowLineInputFn(manual)
		if err != nil {
			return "", errFlowCancelled
		}
		return pystr.Strip(typed), nil
	}
	if len(detected) > 1 {
		fmt.Fprintln(r.Out, "  Available models:")
		for i, m := range detected {
			fmt.Fprintf(r.Out, "    %d. %s\n", i+1, m)
		}
		pick, err := flowRawInputFn(r.Out, fmt.Sprintf("  Select model [1-%d] or type name: ", len(detected)))
		if err != nil {
			return "", errFlowCancelled
		}
		pick = pystr.Strip(pick)
		if pystr.StrIsDigit(pick) {
			n, perr := parsePyInt(pick)
			if perr != nil {
				return "", perr
			}
			if n >= 1 && n <= len(detected) {
				return detected[n-1], nil
			}
		}
		return pick, nil
	}
	typed, err := flowLineInputFn(manual)
	if err != nil {
		return "", errFlowCancelled
	}
	return pystr.Strip(typed), nil
}

// Custom endpoint wizard, URL, key, probe, api mode and model, saved to custom_providers for the menu
func modelFlowCustom(r *ModelFlowRun) error {
	currentURL := r.envValue("OPENAI_BASE_URL")
	currentKey := r.envValue("OPENAI_API_KEY")
	fmt.Fprintln(r.Out, "Custom OpenAI-compatible endpoint configuration:")
	if currentURL != "" {
		fmt.Fprintf(r.Out, "  Current URL: %s\n", currentURL)
	}
	if currentKey != "" {
		fmt.Fprintf(r.Out, "  Current key: %s...\n", prefixRunes(currentKey, 8))
	}
	fmt.Fprintln(r.Out)
	urlShown := currentURL
	if urlShown == "" {
		urlShown = "e.g. https://api.example.com/v1"
	}
	baseURL, err := flowLineInputFn(fmt.Sprintf("API base URL [%s]: ", urlShown))
	if err != nil {
		fmt.Fprintln(r.Out, "\nCancelled.")
		return nil
	}
	baseURL = pystr.Strip(baseURL)
	keyShown := "optional"
	if currentKey != "" {
		keyShown = prefixRunes(currentKey, 8) + "..."
	}
	apiKey, err := flowSecretInputFn(fmt.Sprintf("API key [%s]: ", keyShown))
	if err != nil {
		fmt.Fprintln(r.Out, "\nCancelled.")
		return nil
	}
	apiKey = pystr.Strip(apiKey)
	if baseURL == "" && currentURL == "" {
		fmt.Fprintln(r.Out, "No URL provided. Cancelled.")
		return nil
	}
	effectiveURL := baseURL
	if effectiveURL == "" {
		effectiveURL = currentURL
	}
	if !hasHTTPPrefix(effectiveURL) {
		fmt.Fprintf(r.Out, "Invalid URL: %s (must start with http:// or https://)\n", effectiveURL)
		return nil
	}
	effectiveKey := apiKey
	if effectiveKey == "" {
		effectiveKey = currentKey
	}
	urlLower := strings.ToLower(strings.TrimRight(effectiveURL, "/"))
	looksLocal := false
	for _, h := range []string{"localhost", "127.0.0.1", "0.0.0.0", ":11434", ":8080", ":5000"} {
		if strings.Contains(urlLower, h) {
			looksLocal = true
			break
		}
	}
	if looksLocal && !strings.HasSuffix(urlLower, "/v1") {
		flowSay(r.Out, "", "  Hint: Did you mean to add /v1 at the end?",
			"  Most local model servers (Ollama, vLLM, llama.cpp) require it.", fmt.Sprintf("  e.g. %s/v1", strings.TrimRight(effectiveURL, "/")))
		answer, ok := flowAskFn(r.Out, "  Add /v1? [Y/n]: ", flowAskOptions{Raw: true, Silent: true})
		if !ok {
			answer = "n"
		}
		switch strings.ToLower(answer) {
		case "", "y", "yes":
			effectiveURL = strings.TrimRight(effectiveURL, "/") + "/v1"
			fmt.Fprintf(r.Out, "  Updated URL: %s\n", effectiveURL)
		}
		fmt.Fprintln(r.Out)
	}
	probe, effectiveURL := probeCustomEndpoint(r, effectiveKey, effectiveURL)
	currentAPIMode := ""
	if modelCfg, ok := r.Config["model"].(map[string]any); ok {
		currentAPIMode = pystr.Strip(pyOrStr(modelCfg["api_mode"]))
	}
	apiMode, err := promptCustomAPIModeSelectionFn(r, effectiveURL, currentAPIMode)
	if err != nil {
		return err
	}
	if apiMode != "" {
		fmt.Fprintf(r.Out, "  API mode: %s\n", apiMode)
	} else {
		fmt.Fprintln(r.Out, "  API mode: auto-detect")
	}
	modelName, err := pickDetectedModel(r, probe.Models)
	if err != nil {
		if !errors.Is(err, errFlowCancelled) {
			return err
		}
		fmt.Fprintln(r.Out, "\nCancelled.")
		return nil
	}
	contextLengthStr, err := flowLineInputFn("Context length in tokens [leave blank for auto-detect]: ")
	if err != nil {
		fmt.Fprintln(r.Out, "\nCancelled.")
		return nil
	}
	defaultName := autoProviderName(effectiveURL)
	displayName, err := flowLineInputFn(fmt.Sprintf("Display name [%s]: ", defaultName))
	if err != nil {
		fmt.Fprintln(r.Out, "\nCancelled.")
		return nil
	}
	displayName = pystr.Strip(displayName)
	if displayName == "" {
		displayName = defaultName
	}
	contextLength, hasContext := parseContextLength(r, pystr.Strip(contextLengthStr))
	if !hasContext && modelName != "" {
		reportContextLengthDetection(r, modelName, effectiveURL, effectiveKey)
	}
	customKeyEnv := ""
	if effectiveKey != "" {
		identity := ""
		if parts, perr := pyURLParse(effectiveURL, ""); perr == nil {
			identity, _, _ = pyNetlocHostPort(parts.netloc)
		}
		port, hasPort, perr := pyURLPort(effectiveURL)
		if perr != nil {
			return perr
		}
		if hasPort && port != 0 {
			identity = fmt.Sprintf("%s_%d", identity, port)
		}
		customKeyEnv = config.CustomEndpointKeyEnv(identity)
		if err := r.saveEnv(customKeyEnv, effectiveKey); err != nil {
			return err
		}
		fmt.Fprintf(r.Out, "  API key saved to .env as %s\n", customKeyEnv)
	}
	applyEndpoint := func(model map[string]any) {
		pyDictSet(model, "provider", "custom")
		pyDictSet(model, "base_url", effectiveURL)
		delete(model, "key_env")
		delete(model, "api_key_env")
		if customKeyEnv != "" {
			pyDictSet(model, "api_key", "${"+customKeyEnv+"}")
		}
		if apiMode != "" {
			pyDictSet(model, "api_mode", apiMode)
		} else {
			delete(model, "api_mode")
		}
	}
	if modelName != "" {
		if err := flowSaveModelChoiceFn(r.Env, modelName); err != nil {
			return err
		}
		cfg, model, err := loadConfigModelSection(r.home())
		if err != nil {
			return err
		}
		applyEndpoint(model)
		if err := commitModelConfig(r.Env, cfg); err != nil {
			return err
		}
		copied := map[string]any{}
		for k, v := range model {
			copied[k] = v
		}
		config.SetMappingKeys(copied, config.MappingKeys(model))
		pyDictSet(r.Config, "model", copied)
		fmt.Fprintf(r.Out, "Default model set to: %s (via %s)\n", modelName, effectiveURL)
	} else {
		if baseURL != "" || apiKey != "" {
			if err := r.store().DeactivateProvider(); err != nil {
				return err
			}
		}
		callerModel, ok := r.Config["model"].(map[string]any)
		if !ok {
			callerModel = map[string]any{}
			if raw := r.Config["model"]; truthy(raw) {
				callerModel["default"] = raw
			}
		}
		applyEndpoint(callerModel)
		pyDictSet(r.Config, "model", callerModel)
		fmt.Fprintln(r.Out, "Endpoint saved. Use `/model` in chat or `icebath model` to set a model.")
	}
	if err := saveCustomProviderFn(r, effectiveURL, effectiveKey, modelName, contextLength, displayName, apiMode, customKeyEnv); err != nil {
		return err
	}
	pruneReplacedCustomModelConfigCredentials(r.Env, effectiveURL, displayName)
	return nil
}

// Model ids from a custom_providers models mapping or list, marker keys skipped
func configuredModelIDs(cfgModels any) []string {
	out := []string{}
	switch models := cfgModels.(type) {
	case map[string]any:
		for _, m := range config.MappingKeys(models) {
			if m == "__explicit_model_allowlist__" || m == "__discovered_model_catalog__" || pystr.Strip(m) == "" {
				continue
			}
			out = append(out, m)
		}
	case []any:
		for _, entry := range models {
			modelID := ""
			switch e := entry.(type) {
			case map[string]any:
				raw := e["id"]
				if !pyTruthy(raw) {
					raw = e["model"]
				}
				modelID = pystr.Strip(pyOrStr(raw))
			case string:
				modelID = pystr.Strip(e)
			}
			if modelID != "" {
				out = append(out, modelID)
			}
		}
	}
	return out
}

// Env file of the run's home for command token sources
func (r *ModelFlowRun) fileEnv() map[string]string {
	return config.LoadEnvFile(r.home() + "/.env")
}

// Live catalog probe for a named custom endpoint, native tags for Ollama, the live catalog persisted
func discoverNamedCustomModels(r *ModelFlowRun, providerInfo map[string]any, apiKey string, configured []string, explicitCatalog bool) ([]string, bool) {
	ctx := r.ctx()
	name, baseURL := anyString(providerInfo["name"]), anyString(providerInfo["base_url"])
	source := BuildCommandTokenSource(anyString(providerInfo["key_cmd"]), name, r.home(), r.fileEnv)
	if source != nil {
		apiKey = modelsMaterializeProbeAPIKey(ctx, source)
	} else {
		apiKey = modelsMaterializeProbeAPIKey(ctx, apiKey)
	}
	apiMode := anyString(providerInfo["api_mode"])
	providerKey := strings.TrimSpace(anyString(providerInfo["provider_key"]))
	fmt.Fprintln(r.Out, "Fetching available models...")
	nativeCatalogProvider := "custom"
	if strings.ToLower(providerKey) == "ollama" || strings.ToLower(strings.TrimSpace(name)) == "ollama" {
		nativeCatalogProvider = "ollama"
	}
	extraHeaders := NormalizeExtraHeaders(providerInfo["extra_headers"])
	if extraHeaders == nil {
		extraHeaders = map[string]string{}
	}
	candidate := map[string]string{}
	for k, v := range getOllamaNativeHeadersFn(ctx, baseURL, apiKey) {
		candidate[k] = v
	}
	for key := range candidate {
		for existing := range extraHeaders {
			if strings.EqualFold(key, existing) {
				delete(candidate, key)
				break
			}
		}
	}
	for k, v := range extraHeaders {
		candidate[k] = v
	}
	callerHasAuthorization := false
	for key := range extraHeaders {
		if strings.EqualFold(key, "authorization") {
			callerHasAuthorization = true
		}
	}
	if apiKey != "" && !callerHasAuthorization {
		for key := range candidate {
			if strings.EqualFold(key, "authorization") {
				delete(candidate, key)
			}
		}
		candidate["Authorization"] = "Bearer " + apiKey
	}
	orNil := func(h map[string]string) map[string]string {
		if len(h) == 0 {
			return nil
		}
		return h
	}
	useNative := shouldUseOllamaNativeCatalogFn(ctx, nativeCatalogProvider, baseURL, orNil(candidate))
	nativeHeadersArg := orNil(extraHeaders)
	if useNative {
		nativeHeadersArg = orNil(candidate)
	}
	nativeCatalogEmpty := false
	var live []string
	if useNative {
		if explicitCatalog && len(configured) > 0 {
			live = configured
		} else {
			fetched, ok := fetchOllamaLocalModelsFn(ctx, baseURL, 8*time.Second, nativeHeadersArg)
			live = fetched
			nativeCatalogEmpty = ok && len(fetched) == 0
			if !ok {
				live, _ = flowFetchAPIModelsFn(ctx, apiKey, normalizeOpenAIBaseURL(baseURL), 8*time.Second, apiMode, nativeHeadersArg)
				nativeCatalogEmpty = false
			}
		}
	} else {
		live, _ = flowFetchAPIModelsFn(ctx, apiKey, baseURL, 8*time.Second, apiMode, nativeHeadersArg)
	}
	var models []string
	switch {
	case explicitCatalog:
		models = configured
	case nativeCatalogEmpty:
		models = []string{}
	case len(live) > 0:
		models = live
	default:
		models = configured
	}
	if len(live) > 0 {
		func() {
			defer func() { _ = recover() }()
			_, _, identity := entryCredentials(ctx, providerInfo, "key_env", "api_key_env")
			var identityPtr *string
			if identity != "" {
				identityPtr = &identity
			}
			pickerSaveDiscoveredFn(ctx, baseURL, live, apiMode, orNil(extraHeaders), identityPtr)
		}()
	}
	return models, nativeCatalogEmpty
}

// Searchable radiolist over the models with a numbered prompt without curses, false when cancelled
func pickNamedCustomModel(r *ModelFlowRun, name string, models []string, savedModel string) (string, bool) {
	defaultIdx := 0
	for i, m := range models {
		if savedModel != "" && m == savedModel {
			defaultIdx = i
			break
		}
	}
	fmt.Fprintf(r.Out, "Found %d model(s):\n\n", len(models))
	items := make([]string, 0, len(models)+1)
	for _, m := range models {
		if m == savedModel {
			items = append(items, m+" (current)")
		} else {
			items = append(items, m)
		}
	}
	items = append(items, "Cancel")
	idx, ok := flowRadiolist(fmt.Sprintf("Select model from %s:", name), plainPickerRows(items), defaultIdx, PickerRadiolistOptions{Searchable: true})
	if ok {
		fmt.Fprintln(r.Out)
	} else {
		for i, m := range models {
			current := ""
			if m == savedModel {
				current = " (current)"
			}
			fmt.Fprintf(r.Out, "  %d. %s%s\n", i+1, m, current)
		}
		flowSay(r.Out, fmt.Sprintf("  %d. Cancel", len(models)+1), "")
		val, err := flowRawInputFn(r.Out, fmt.Sprintf("Choice [1-%d]: ", len(models)+1))
		if err != nil {
			fmt.Fprintln(r.Out, "\nCancelled.")
			return "", false
		}
		val = strings.TrimSpace(val)
		if val == "" {
			fmt.Fprintln(r.Out, "Cancelled.")
			return "", false
		}
		n, perr := parsePyInt(val)
		if perr != nil {
			fmt.Fprintln(r.Out, "\nCancelled.")
			return "", false
		}
		idx = n - 1
	}
	if idx < 0 || idx >= len(models) {
		fmt.Fprintln(r.Out, "Cancelled.")
		return "", false
	}
	return models[idx], true
}

// Named custom provider flow, catalog probe with the saved model preselected, then activate and persist
func modelFlowNamedCustom(r *ModelFlowRun, providerInfo map[string]any) error {
	name := anyString(providerInfo["name"])
	baseURL := anyString(providerInfo["base_url"])
	apiMode := anyString(providerInfo["api_mode"])
	apiKey := anyString(providerInfo["api_key"])
	keyEnv := anyString(providerInfo["key_env"])
	savedModel := anyString(providerInfo["model"])
	providerKey := strings.TrimSpace(anyString(providerInfo["provider_key"]))
	if apiKey == "" && keyEnv != "" {
		apiKey = GetSecretStr(r.ctx(), keyEnv, "")
	}
	configAPIKey := customProviderAPIKeyConfigValue(providerInfo, apiKey)
	discover := true
	switch d := dictGet(providerInfo, "discover_models", true).(type) {
	case string:
		switch pystr.Lower(d) {
		case "false", "no", "0":
			discover = false
		}
	default:
		discover = truthy(d)
	}
	cfgModels := providerInfo["models"]
	explicitCatalog := modelsConfigIsAllowlist(cfgModels, entryModelsDiscovered(providerInfo))
	configured := configuredModelIDs(cfgModels)
	flowSay(r.Out, fmt.Sprintf("  Provider: %s", name), fmt.Sprintf("  URL:      %s", baseURL))
	if savedModel != "" {
		fmt.Fprintf(r.Out, "  Current:  %s\n", savedModel)
	}
	fmt.Fprintln(r.Out)
	nativeCatalogEmpty := false
	var models []string
	if !discover {
		models = configured
		if len(models) == 0 && savedModel != "" {
			models = []string{savedModel}
		}
		fmt.Fprintf(r.Out, "Using configured models (discover_models: false): %d\n", len(models))
	} else {
		models, nativeCatalogEmpty = discoverNamedCustomModels(r, providerInfo, apiKey, configured, explicitCatalog)
	}
	var modelName string
	switch {
	case len(models) > 0:
		picked, ok := pickNamedCustomModelFn(r, name, models, savedModel)
		if !ok {
			return nil
		}
		modelName = picked
	case savedModel != "" && !nativeCatalogEmpty:
		fmt.Fprintln(r.Out, "Could not fetch models from endpoint.")
		typed, ok := flowAskFn(r.Out, fmt.Sprintf("Model name [%s]: ", savedModel), flowAskOptions{})
		if !ok {
			return nil
		}
		modelName = typed
		if modelName == "" {
			modelName = savedModel
		}
	default:
		fmt.Fprintln(r.Out, "Could not fetch models from endpoint. Enter model name manually.")
		typed, ok := flowAskFn(r.Out, "Model name: ", flowAskOptions{})
		if !ok {
			return nil
		}
		if typed == "" {
			fmt.Fprintln(r.Out, "No model specified. Cancelled.")
			return nil
		}
		modelName = typed
	}
	if err := flowSaveModelChoiceFn(r.Env, modelName); err != nil {
		return err
	}
	cfg, model, err := loadConfigModelSection(r.home())
	if err != nil {
		return err
	}
	delete(model, "key_env")
	delete(model, "api_key_env")
	if providerKey != "" {
		pyDictSet(model, "provider", CustomProviderSlug(name, providerKey))
		delete(model, "base_url")
		delete(model, "api_key")
	} else {
		pyDictSet(model, "provider", "custom")
		pyDictSet(model, "base_url", customProviderBaseURLConfigValue(providerInfo, baseURL))
		if configAPIKey != "" {
			pyDictSet(model, "api_key", configAPIKey)
		}
	}
	if apiMode != "" {
		pyDictSet(model, "api_mode", apiMode)
	} else {
		delete(model, "api_mode")
	}
	if err := commitModelConfig(r.Env, cfg); err != nil {
		return err
	}
	if providerKey != "" {
		cfg, err := r.loadConfig()
		if err != nil {
			return err
		}
		providersCfg, _ := cfg["providers"].(map[string]any)
		if providerEntry, ok := providersCfg[providerKey].(map[string]any); ok {
			pyDictSet(providerEntry, "default_model", modelName)
			hadInlineAPIKey := pystr.Strip(pyOrStr(providerInfo["api_key_ref"])) != "" || pystr.Strip(pyOrStr(providerInfo["api_key"])) != ""
			if hadInlineAPIKey && configAPIKey != "" && pystr.Strip(pyOrStr(providerEntry["api_key"])) == "" {
				pyDictSet(providerEntry, "api_key", configAPIKey)
			}
			if keyEnv != "" && pystr.Strip(pyOrStr(providerEntry["key_env"])) == "" {
				pyDictSet(providerEntry, "key_env", keyEnv)
			}
			pyDictSet(cfg, "providers", providersCfg)
			if err := config.Save(r.home(), cfg); err != nil {
				return err
			}
		}
	} else if err := saveCustomProviderFn(r, baseURL, configAPIKey, modelName, 0, "", apiMode, ""); err != nil {
		return err
	}
	flowSay(r.Out, fmt.Sprintf("\n✅ Model set to: %s", modelName), fmt.Sprintf("   Provider: %s (%s)", name, baseURL))
	return nil
}
