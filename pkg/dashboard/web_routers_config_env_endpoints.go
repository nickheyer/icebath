package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Live credential probes keyed by env var, the URL and whether the key rides as a bearer or a query
var credentialProbes = map[string][2]string{
	"OPENROUTER_API_KEY": {"https://openrouter.ai/api/v1/key", "bearer"},
	"OPENAI_API_KEY":     {"https://api.openai.com/v1/models", "bearer"},
	"XAI_API_KEY":        {"https://api.x.ai/v1/models", "bearer"},
	"GEMINI_API_KEY":     {"https://generativelanguage.googleapis.com/v1beta/models", "query"},
}

var customEndpointSlugRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// Lower cased slug of a custom endpoint name, fallback when nothing survives
func customEndpointID(raw any, fallback string) string {
	slug := pystr.Lower(strings.Trim(customEndpointSlugRE.ReplaceAllString(provider.CoerceProviderID(raw), "-"), "-_"))
	if slug == "" {
		return fallback
	}
	return slug
}

// Providers entry of an endpoint id by its stored key first, then by its legacy slug
func resolveCustomEndpointEntry(providers any, endpointID string) (string, map[string]any) {
	storedKey, entry := provider.FindProviderEntry(providers, endpointID)
	if entry != nil {
		return storedKey, entry
	}
	normalized := customEndpointID(endpointID, "custom")
	if normalized == endpointID {
		return "", nil
	}
	return provider.FindProviderEntry(providers, normalized)
}

// Model ids of an endpoint entry, its default first and repeats dropped
func modelsFromCustomEndpointEntry(entry map[string]any) []string {
	var models []string
	switch raw := entry["models"].(type) {
	case map[string]any:
		for _, k := range config.MappingKeys(raw) {
			models = append(models, pystr.Strip(k))
		}
	case []any:
		for _, m := range raw {
			models = append(models, pystr.Strip(pyrepr.Str(m)))
		}
	}
	defaultModel := pystr.Strip(pyrepr.Str(pyrepr.Or(pyrepr.Or(entry["model"], entry["default_model"]), "")))
	if defaultModel != "" {
		models = append([]string{defaultModel}, models...)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, m := range models {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// Whether a provider or model block holds a key, with the preview the panel shows
func apiKeyDisplay(entry map[string]any) (bool, any) {
	if plaintext := pystr.Strip(pyrepr.Str(pyrepr.Or(entry["api_key"], ""))); plaintext != "" {
		return true, redactedCredentialPreview(plaintext)
	}
	if keyEnv := pystr.Strip(pyrepr.Str(pyrepr.Or(entry["key_env"], ""))); keyEnv != "" {
		return true, "${" + keyEnv + "}"
	}
	return false, nil
}

// On disk api_key of a providers entry before env expansion, nil when absent
func rawProviderAPIKey(ctx context.Context, endpointID string) (any, error) {
	raw, err := webReadRawConfig(ctx)
	if err != nil {
		return nil, err
	}
	_, entry := provider.FindProviderEntry(raw["providers"], endpointID)
	if entry == nil {
		return nil, nil
	}
	return entry["api_key"], nil
}

var envRefSearchRE = regexp.MustCompile(`\$\{[^}]+\}`)

// Whether an endpoint's on disk api_key is an env reference template
func configAPIKeyIsEnvRef(ctx context.Context, endpointID string) (bool, error) {
	raw, err := rawProviderAPIKey(ctx, endpointID)
	if err != nil {
		return false, err
	}
	s, ok := raw.(string)
	return ok && envRefSearchRE.MatchString(s), nil
}

var desktopAPIModes = map[string]bool{"chat_completions": true, "codex_responses": true, "anthropic_messages": true}

// Canonical transport a providers entry pins, empty for runtime auto detection
func endpointAPIMode(entry map[string]any) string {
	raw := pyrepr.Str(pyrepr.Or(pyrepr.Or(entry["api_mode"], entry["transport"]), ""))
	mode := pystr.Lower(provider.CanonicalAPIMode(raw))
	if desktopAPIModes[mode] {
		return mode
	}
	return ""
}

// Panel row of one custom endpoint
func endpointRow(endpointID, name, baseURL, model string, models []string, contextLength any, discoverModels bool,
	keyEntry map[string]any, isCurrent bool, source string) *pyjson.Object {
	hasAPIKey, preview := apiKeyDisplay(keyEntry)
	return pyjson.ObjectOf("id", endpointID, "name", name, "base_url", baseURL, "model", model, "models", stringsAny(models),
		"api_mode", endpointAPIMode(keyEntry), "context_length", configJSON(contextLength), "discover_models", discoverModels,
		"has_api_key", hasAPIKey, "api_key_preview", preview, "is_current", isCurrent, "source", source)
}

// Whether model.provider names a providers entry by its key or its custom name spelling
func modelNamesProvider(modelCfg map[string]any, providerKey string, entry map[string]any) bool {
	names := map[string]bool{pystr.Lower(provider.CoerceProviderID(providerKey)): true}
	if entry != nil {
		if name := provider.CoerceProviderID(entry["name"]); name != "" {
			names[pystr.Lower(name)] = true
		}
	}
	current := pystr.Lower(pystr.Strip(pyrepr.Str(pyrepr.Or(modelCfg["provider"], ""))))
	return names[strings.TrimPrefix(current, "custom:")]
}

// Custom endpoint panel body of a config, providers rows, legacy rows and a bare custom main slot
func customEndpointResponse(cfg map[string]any) *pyjson.Object {
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok {
		modelCfg = map[string]any{}
	}
	currentProvider := pyrepr.Str(pyrepr.Or(modelCfg["provider"], ""))
	currentModelRaw, held := modelCfg["default"]
	if !held {
		currentModelRaw = modelCfg["name"]
		if _, has := modelCfg["name"]; !has {
			currentModelRaw = ""
		}
	}
	currentModel := pyrepr.Str(pyrepr.Or(currentModelRaw, ""))
	currentBaseURL := pyrepr.Str(pyrepr.Or(modelCfg["base_url"], ""))
	endpoints := []*pyjson.Object{}
	if providers, ok := cfg["providers"].(map[string]any); ok {
		for _, providerID := range config.MappingKeys(providers) {
			rawEntry, ok := providers[providerID].(map[string]any)
			if !ok {
				continue
			}
			baseURL := pystr.Strip(pyrepr.Str(pyrepr.Or(pyrepr.Or(pyrepr.Or(rawEntry["base_url"], rawEntry["url"]), rawEntry["api"]), "")))
			if baseURL == "" {
				continue
			}
			models := modelsFromCustomEndpointEntry(rawEntry)
			firstModel := ""
			if len(models) > 0 {
				firstModel = models[0]
			}
			discover, held := rawEntry["discover_models"]
			if !held {
				discover = true
			}
			endpoints = append(endpoints, endpointRow(providerID, pyrepr.Str(pyrepr.Or(rawEntry["name"], providerID)), baseURL,
				pyrepr.Str(pyrepr.Or(pyrepr.Or(pyrepr.Or(rawEntry["model"], rawEntry["default_model"]), firstModel), "")),
				models, rawEntry["context_length"], pyrepr.Truthy(discover), rawEntry,
				modelNamesProvider(modelCfg, providerID, rawEntry), "providers"))
		}
	}
	isBareCustom := pystr.Lower(currentProvider) == "custom" && currentBaseURL != ""
	seenIDs := map[string]bool{}
	for _, e := range endpoints {
		seenIDs[e.GetOr("id", "").(string)] = true
	}
	for _, entry := range provider.CompatibleCustomProviders(cfg) {
		if pyrepr.Truthy(entry["provider_key"]) {
			continue
		}
		name := pyrepr.Str(entry["name"])
		endpointID := customEndpointID(entry["name"], "custom")
		if seenIDs[endpointID] {
			continue
		}
		seenIDs[endpointID] = true
		models := modelsFromCustomEndpointEntry(entry)
		baseURL := pyrepr.Str(entry["base_url"])
		isCurrent := isBareCustom && strings.TrimRight(baseURL, "/") == strings.TrimRight(currentBaseURL, "/")
		firstModel := ""
		if len(models) > 0 {
			firstModel = models[0]
		}
		discover, held := entry["discover_models"]
		if !held {
			discover = true
		}
		endpoints = append(endpoints, endpointRow(endpointID, name, baseURL,
			pyrepr.Str(pyrepr.Or(pyrepr.Or(entry["model"], firstModel), "")), models, entry["context_length"],
			pyrepr.Truthy(discover), entry, isCurrent, "custom_providers"))
	}
	if isBareCustom {
		covered := false
		for _, e := range endpoints {
			if e.GetOr("id", "") == "custom" || e.GetOr("is_current", false) == true {
				covered = true
			}
		}
		if !covered {
			models := []string{}
			if currentModel != "" {
				models = []string{currentModel}
			}
			direct := endpointRow("custom", "Custom", currentBaseURL, currentModel, models, modelCfg["context_length"], true,
				modelCfg, true, "direct-config")
			endpoints = append([]*pyjson.Object{direct}, endpoints...)
		}
	}
	rows := make([]any, len(endpoints))
	for i, e := range endpoints {
		rows[i] = e
	}
	return pyjson.ObjectOf("endpoints", rows, "current",
		pyjson.ObjectOf("provider", currentProvider, "model", currentModel, "base_url", currentBaseURL))
}

// Removes and returns the legacy custom_providers row whose name slugs to the provider key
func popLegacyCustomProvider(cfg map[string]any, providerKey string) map[string]any {
	legacy, ok := cfg["custom_providers"].([]any)
	if !ok {
		return nil
	}
	for i, raw := range legacy {
		entry, ok := raw.(map[string]any)
		if ok && customEndpointID(pyrepr.Str(pyrepr.Or(entry["name"], "")), "custom") == providerKey {
			cfg["custom_providers"] = append(legacy[:i:i], legacy[i+1:]...)
			return entry
		}
	}
	return nil
}

// Drops the main slot mirror of a removed provider when model names it
func detachMainModelFromProvider(cfg map[string]any, providerKey string, entry map[string]any) {
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok || !modelNamesProvider(modelCfg, providerKey, entry) {
		return
	}
	for _, field := range []string{"provider", "base_url", "api_key", "key_env"} {
		delete(modelCfg, field)
	}
	setConfigKey(cfg, "model", modelCfg)
}

var envRefFullRE = regexp.MustCompile(`\A\$\{([^}]+)\}\z`)

// Writes a custom endpoint into providers, moving its key into the .env and optionally making it the default
func writeCustomEndpoint(ctx context.Context, cfg map[string]any, body *pyjson.Object) (string, map[string]any, error) {
	name := bodyStrip(body, "name")
	baseURL := strings.TrimRight(bodyStrip(body, "base_url"), "/")
	model := bodyStrip(body, "model")
	if name == "" {
		return "", nil, NewHTTPException(400, "name required")
	}
	if baseURL == "" {
		return "", nil, NewHTTPException(400, "base_url required")
	}
	parsed, err := pyurl.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Netloc == "" {
		return "", nil, NewHTTPException(400, "base_url must include scheme and host")
	}
	if model == "" {
		return "", nil, NewHTTPException(400, "model required")
	}
	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		providers = map[string]any{}
	}
	idOrName := bodyStr(body, "id")
	if idOrName == "" {
		idOrName = bodyStr(body, "name")
	}
	storedKey, existing := resolveCustomEndpointEntry(providers, idOrName)
	found := existing != nil
	var endpointID string
	if found {
		endpointID = provider.CoerceProviderID(storedKey)
	} else {
		endpointID = customEndpointID(idOrName, "custom")
		if legacy := popLegacyCustomProvider(cfg, endpointID); legacy != nil {
			if converted := provider.CustomProviderEntryToProviderConfig(legacy, endpointID); converted != nil {
				existing = converted
			} else if restored, ok := cfg["custom_providers"].([]any); ok {
				cfg["custom_providers"] = append(restored, legacy)
			}
		}
	}
	if existing == nil {
		existing = map[string]any{}
	}
	entry := copyConfigMap(existing)
	setConfigKey(entry, "name", name)
	setConfigKey(entry, "base_url", baseURL)
	setConfigKey(entry, "model", model)
	setConfigKey(entry, "discover_models", body.GetOr("discover_models", true) == true)
	if apiMode, ok := body.GetOr("api_mode", nil).(string); ok {
		delete(entry, "transport")
		if apiMode != "" {
			setConfigKey(entry, "api_mode", apiMode)
		} else {
			delete(entry, "api_mode")
		}
	}
	details := pyjson.NewObject()
	if list, ok := body.GetOr("model_details", nil).([]any); ok {
		for _, raw := range list {
			d := raw.(*pyjson.Object)
			if id := bodyStrip(d, "id"); id != "" {
				details.Set(id, d)
			}
		}
	}
	modelsMap := map[string]any{}
	if existingModels, ok := entry["models"].(map[string]any); ok {
		modelsMap = copyConfigMap(existingModels)
	}
	var candidates []string
	if list, ok := body.GetOr("models", nil).([]any); ok {
		for _, m := range list {
			candidates = append(candidates, m.(string))
		}
	}
	candidates = append(append(candidates, details.Keys...), model)
	for _, candidate := range candidates {
		modelID := pystr.Strip(candidate)
		if modelID == "" {
			continue
		}
		row := map[string]any{}
		if current, ok := modelsMap[modelID].(map[string]any); ok {
			row = copyConfigMap(current)
		}
		if raw, ok := details.Get(modelID); ok {
			detail := raw.(*pyjson.Object)
			for _, k := range []string{"canonical_model", "reasoning_effort"} {
				if v, ok := detail.GetOr(k, nil).(string); ok && pystr.Strip(v) != "" {
					setConfigKey(row, k, pystr.Strip(v))
				}
			}
		}
		setConfigKey(modelsMap, modelID, row)
	}
	setConfigKey(entry, "models", modelsMap)
	if raw, ok := details.Get(model); ok {
		alias := raw.(*pyjson.Object)
		canonical := bodyStrip(alias, "canonical_model")
		if canonical != "" && canonical != model {
			effort := pystr.Lower(bodyStrip(alias, "reasoning_effort"))
			if config.ParseReasoningEffort(effort) != nil {
				agentCfg, ok := cfg["agent"].(map[string]any)
				if !ok {
					agentCfg = map[string]any{}
				}
				overrides := map[string]any{}
				if current, ok := agentCfg["reasoning_overrides"].(map[string]any); ok {
					overrides = copyConfigMap(current)
				}
				setConfigKey(overrides, canonical, effort)
				setConfigKey(agentCfg, "reasoning_overrides", overrides)
				setConfigKey(cfg, "agent", agentCfg)
			}
			model = canonical
			setConfigKey(entry, "model", model)
			if _, held := modelsMap[model]; !held {
				setConfigKey(modelsMap, model, map[string]any{})
			}
		}
	}
	if n, ok := body.GetOr("context_length", nil).(int64); ok && n > 0 {
		setConfigKey(entry, "context_length", int(n))
		setConfigKey(modelsMap[model].(map[string]any), "context_length", int(n))
	}
	envVar := config.CustomEndpointKeyEnv(endpointID)
	h := home.Dir(ctx)
	if apiKey, ok := body.GetOr("api_key", nil).(string); ok {
		submitted := pystr.Strip(apiKey)
		if submitted != "" {
			if envRefFullRE.MatchString(submitted) || isRedactedCredentialPreview(submitted) {
				return "", nil, NewHTTPException(400, redactedCredentialWriteDetail)
			}
			if err := config.SaveEnvValueContext(ctx, h, envVar, submitted); err != nil {
				return "", nil, err
			}
			setConfigKey(entry, "key_env", envVar)
			delete(entry, "api_key")
		} else {
			if _, err := config.RemoveHomeEnvValue(ctx, h, envVar); err != nil {
				return "", nil, err
			}
			delete(entry, "key_env")
			delete(entry, "api_key")
		}
	} else if pystr.Strip(pyrepr.Str(pyrepr.Or(entry["api_key"], ""))) != "" {
		isRef, err := configAPIKeyIsEnvRef(ctx, endpointID)
		if err != nil {
			return "", nil, err
		}
		if !isRef {
			plaintext, ok := entry["api_key"].(string)
			if !ok {
				return "", nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(entry["api_key"]))
			}
			if err := config.SaveEnvValueContext(ctx, h, envVar, pystr.Strip(plaintext)); err != nil {
				return "", nil, err
			}
			setConfigKey(entry, "key_env", envVar)
			delete(entry, "api_key")
		}
	}
	if found && storedKey != endpointID {
		delete(providers, storedKey)
	}
	setConfigKey(providers, endpointID, entry)
	setConfigKey(cfg, "providers", providers)
	if body.GetOr("make_default", false) == true {
		result, err := provider.ValidatedMainModelSelection(ctx, cfg, endpointID, model, baseURL, "", nil)
		if err != nil {
			return "", nil, asHTTPError(err)
		}
		current, held := cfg["model"]
		if !held {
			current = map[string]any{}
		}
		modelCfg, err := provider.ApplyMainModelAssignment(ctx, current, result, "")
		if err != nil {
			return "", nil, asHTTPError(err)
		}
		setConfigKey(cfg, "model", modelCfg)
		if pyrepr.Truthy(entry["key_env"]) && modelCfg != nil {
			setConfigKey(modelCfg, "key_env", entry["key_env"])
			if err := provider.DropModelKey(ctx, modelCfg, "api_key"); err != nil {
				return "", nil, err
			}
		}
	}
	return endpointID, entry, nil
}

// Sections a custom endpoint default assignment writes together
var routeSections = []string{"model", "providers", "custom_providers"}

// Registers the custom endpoint list, save, activate, delete and probe routes
func addCustomEndpointRoutes(r *APIRouter) {
	failDetail := func(detail string) *string { return &detail }
	r.Get("/api/providers/custom-endpoints", &Endpoint{
		Name:   "list_custom_endpoints",
		Doc:    "Return configured OpenAI-compatible custom endpoints for Desktop.",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			sctx, err := withConfigProfile(ctx, c.Str("profile"))
			if err == nil {
				var cfg map[string]any
				if cfg, err = webLoadConfig(sctx); err == nil {
					return customEndpointResponse(cfg), nil
				}
			}
			return nil, httpFailure(err, "GET /api/providers/custom-endpoints failed", 500, "", failDetail("Failed to list custom endpoints"))
		},
	})
	r.Post("/api/providers/custom-endpoints", &Endpoint{
		Name:   "upsert_custom_endpoint",
		Doc:    "Create or update a v12+ ``providers`` custom endpoint entry.",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Body:   []BodyParam{Body("body", customEndpointUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			resp, err := upsertCustomEndpoint(ctx, c.Str("profile"), c.Object("body"))
			if err != nil {
				return nil, httpFailure(err, "POST /api/providers/custom-endpoints failed", 500, "", failDetail("Failed to save custom endpoint"))
			}
			return resp, nil
		},
	})
	r.Post("/api/providers/custom-endpoints/{endpoint_id}/activate", &Endpoint{
		Name:   "activate_custom_endpoint",
		Doc:    "Set a configured custom endpoint as the default model provider.",
		Params: []Param{PathParam("endpoint_id", Str), QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			id := c.Str("endpoint_id")
			resp, err := activateCustomEndpoint(ctx, c.Str("profile"), id)
			if err != nil {
				return nil, httpFailure(err, "POST /api/providers/custom-endpoints/"+id+"/activate failed", 500, "",
					failDetail("Failed to activate custom endpoint"))
			}
			return resp, nil
		},
	})
	r.Delete("/api/providers/custom-endpoints/{endpoint_id}", &Endpoint{
		Name:   "delete_custom_endpoint",
		Doc:    "Remove a configured custom endpoint from ``providers``.",
		Params: []Param{PathParam("endpoint_id", Str), QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			id := c.Str("endpoint_id")
			resp, err := deleteCustomEndpoint(ctx, c.Str("profile"), id)
			if err != nil {
				return nil, httpFailure(err, "DELETE /api/providers/custom-endpoints/"+id+" failed", 500, "",
					failDetail("Failed to delete custom endpoint"))
			}
			return resp, nil
		},
	})
	r.Post("/api/providers/custom-endpoints/validate", &Endpoint{
		Name: "validate_custom_endpoint",
		Doc:  "Probe a custom endpoint by calling its OpenAI-compatible /models URL.",
		Body: []BodyParam{Body("body", customEndpointUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return validateCustomEndpoint(ctx, c.Object("body")), nil
		},
	})
}

// Context scoped to the config profile, the scope error when the profile is unknown
func withConfigProfile(ctx context.Context, profile string) (context.Context, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	return sctx, err
}

// Saves a custom endpoint under the mutation lock and counts a newly created one as a provider setup
func upsertCustomEndpoint(ctx context.Context, profile string, body *pyjson.Object) (*pyjson.Object, error) {
	sctx, err := withConfigProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	lctx, unlock := provider.LockConfigMutation(sctx)
	resp, endpointID, created, h, err := func() (*pyjson.Object, string, bool, string, error) {
		defer unlock()
		cfg, err := webLoadConfig(lctx)
		if err != nil {
			return nil, "", false, "", err
		}
		providers, ok := cfg["providers"].(map[string]any)
		if !ok {
			providers = map[string]any{}
		}
		idOrName := bodyStr(body, "id")
		if idOrName == "" {
			idOrName = bodyStr(body, "name")
		}
		_, existing := resolveCustomEndpointEntry(providers, idOrName)
		endpointID, _, err := writeCustomEndpoint(lctx, cfg, body)
		if err != nil {
			return nil, "", false, "", err
		}
		saveCtx := lctx
		if body.GetOr("make_default", false) == true {
			saveCtx = config.RefusingLockedOmissions(lctx, routeSections...)
		}
		if err := webSaveConfig(saveCtx, cfg, config.SaveOptions{}); err != nil {
			return nil, "", false, "", err
		}
		return customEndpointResponse(cfg), endpointID, existing == nil, home.Dir(lctx), nil
	}()
	if err != nil {
		return nil, err
	}
	if created {
		observability.RecordProviderSetupDone(ctx, observability.WebSetupSurface(), "custom", h, true)
	}
	resp.Set("ok", true)
	resp.Set("id", endpointID)
	return resp, nil
}

// Makes a configured or legacy custom endpoint the default main model provider
func activateCustomEndpoint(ctx context.Context, profile, endpointID string) (*pyjson.Object, error) {
	sctx, err := withConfigProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	lctx, unlock := provider.LockConfigMutation(sctx)
	defer unlock()
	cfg, err := webLoadConfig(lctx)
	if err != nil {
		return nil, err
	}
	storedKey, entry := resolveCustomEndpointEntry(cfg["providers"], endpointID)
	var providerKey string
	if entry == nil {
		providerKey = customEndpointID(endpointID, "custom")
		if legacy := popLegacyCustomProvider(cfg, providerKey); legacy != nil {
			entry = provider.CustomProviderEntryToProviderConfig(legacy, providerKey)
		}
		if entry == nil {
			return nil, NewHTTPException(404, "custom endpoint not found")
		}
		providers, ok := cfg["providers"].(map[string]any)
		if !ok {
			providers = map[string]any{}
		}
		setConfigKey(providers, providerKey, entry)
		setConfigKey(cfg, "providers", providers)
	} else {
		providerKey = provider.CoerceProviderID(storedKey)
	}
	models := modelsFromCustomEndpointEntry(entry)
	firstModel := ""
	if len(models) > 0 {
		firstModel = models[0]
	}
	model := pystr.Strip(pyrepr.Str(pyrepr.Or(pyrepr.Or(pyrepr.Or(entry["model"], entry["default_model"]), firstModel), "")))
	baseURL := pystr.Strip(pyrepr.Str(pyrepr.Or(pyrepr.Or(entry["base_url"], entry["api"]), "")))
	if model == "" || baseURL == "" {
		return nil, NewHTTPException(400, "custom endpoint is incomplete")
	}
	current, held := cfg["model"]
	if !held {
		current = map[string]any{}
	}
	result, err := provider.ValidatedMainModelSelection(lctx, cfg, providerKey, model, baseURL, "", nil)
	if err != nil {
		return nil, asHTTPError(err)
	}
	modelCfg, err := provider.ApplyMainModelAssignment(lctx, current, result, "")
	if err != nil {
		return nil, asHTTPError(err)
	}
	if pyrepr.Truthy(entry["key_env"]) {
		setConfigKey(modelCfg, "key_env", entry["key_env"])
		if err := provider.DropModelKey(lctx, modelCfg, "api_key"); err != nil {
			return nil, err
		}
	} else if pyrepr.Truthy(entry["api_key"]) {
		rawKey := ""
		if raw, err := rawProviderAPIKey(lctx, providerKey); err == nil {
			rawKey = pystr.Strip(pyrepr.Str(pyrepr.Or(raw, "")))
		}
		if strings.HasPrefix(rawKey, "${") && strings.HasSuffix(rawKey, "}") {
			setConfigKey(modelCfg, "api_key", rawKey)
		} else {
			setConfigKey(modelCfg, "api_key", entry["api_key"])
		}
	}
	setConfigKey(cfg, "model", modelCfg)
	if err := webSaveConfig(config.RefusingLockedOmissions(lctx, routeSections...), cfg, config.SaveOptions{}); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "provider", providerKey, "model", model), nil
}

// Removes a custom endpoint, its main slot mirror and its .env key
func deleteCustomEndpoint(ctx context.Context, profile, endpointID string) (*pyjson.Object, error) {
	sctx, err := withConfigProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	lctx, unlock := provider.LockConfigMutation(sctx)
	defer unlock()
	cfg, err := webLoadConfig(lctx)
	if err != nil {
		return nil, err
	}
	storedKey, entry := resolveCustomEndpointEntry(cfg["providers"], endpointID)
	var providerKey string
	if providers, ok := cfg["providers"].(map[string]any); entry != nil && ok {
		providerKey = provider.CoerceProviderID(storedKey)
		delete(providers, storedKey)
		setConfigKey(cfg, "providers", providers)
	} else {
		providerKey = customEndpointID(endpointID, "custom")
		if popLegacyCustomProvider(cfg, providerKey) == nil {
			return nil, NewHTTPException(404, "custom endpoint not found")
		}
	}
	detachMainModelFromProvider(cfg, providerKey, entry)
	if _, err := config.RemoveHomeEnvValue(lctx, home.Dir(lctx), config.CustomEndpointKeyEnv(providerKey)); err != nil {
		return nil, err
	}
	if err := webSaveConfig(lctx, cfg, config.SaveOptions{}); err != nil {
		return nil, err
	}
	resp := customEndpointResponse(cfg)
	resp.Set("ok", true)
	return resp, nil
}

// Probe of a custom endpoint's models list and the transport route its mode posts to
func validateCustomEndpoint(ctx context.Context, body *pyjson.Object) *pyjson.Object {
	baseURL := strings.TrimRight(bodyStrip(body, "base_url"), "/")
	if baseURL == "" {
		return pyjson.ObjectOf("ok", false, "reachable", true, "message", "Enter an endpoint URL first.", "models", []any{})
	}
	headers := [][2]string{{"Accept", "application/json"}}
	if key := bodyStrip(body, "api_key"); key != "" {
		headers = append(headers, [2]string{"Authorization", "Bearer " + key})
	}
	resolved, resp := probeOpenAICompatibleModels(ctx, baseURL, headers)
	if resp == nil {
		return pyjson.ObjectOf("ok", false, "reachable", false, "message", fmt.Sprintf("Could not reach %s/models.", baseURL), "models", []any{})
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return pyjson.ObjectOf("ok", false, "reachable", true, "message", "The endpoint rejected the API key.", "models", []any{})
	}
	if !httpxIsSuccess(resp) {
		return pyjson.ObjectOf("ok", false, "reachable", true, "message", fmt.Sprintf("Endpoint returned HTTP %d.", resp.StatusCode), "models", []any{})
	}
	entries := parseModelEntries(resp)
	ids := make([]any, len(entries))
	details := make([]any, len(entries))
	for i, e := range entries {
		ids[i] = e.GetOr("id", "")
		details[i] = e
	}
	if len(ids) == 0 {
		return pyjson.ObjectOf("ok", false, "reachable", true, "message", noModelsProbeMessage(resp, resolved),
			"models", []any{}, "model_details", []any{}, "resolved_base_url", resolved)
	}
	mode := pystr.Lower(provider.CanonicalAPIMode(bodyStr(body, "api_mode")))
	if mode == "" {
		mode = autoAPIMode(resolved)
	}
	probeModel := bodyStrip(body, "model")
	if probeModel == "" {
		probeModel = ids[0].(string)
	}
	missing := probeTransportRoute(ctx, endpointProbeClient(resolved), resolved, mode, probeModel, headers)
	result := pyjson.ObjectOf("ok", true, "reachable", true, "message", "", "models", ids, "model_details", details,
		"transport_checked", mode, "resolved_base_url", resolved)
	if missing != "" {
		result.Set("ok", false)
		result.Set("message", missing)
	}
	return result
}

// Whether a response status is 2xx, the httpx is_success test
func httpxIsSuccess(resp *pyhttpx.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Why a successful models probe yielded no usable models, naming non JSON, non list and empty bodies
func noModelsProbeMessage(resp *pyhttpx.Response, baseURL string) string {
	url := baseURL + "/models"
	contentType := pystr.Strip(strings.SplitN(resp.Header.Get("content-type"), ";", 2)[0])
	payload, err := resp.JSON()
	if err != nil {
		payload = nil
	}
	obj, isDict := payload.(*pyjson.Object)
	_, isList := payload.([]any)
	if !isDict && !isList {
		kind := contentType
		if kind == "" {
			kind = "a non-JSON body"
		}
		return fmt.Sprintf("Connected to %s, but it returned %s instead of a JSON model list — "+
			"the URL may be an SPA catch-all or a wrong base URL, not an OpenAI-compatible API.", url, kind)
	}
	data := payload
	if isDict {
		data = obj.GetOr("data", nil)
	}
	if _, ok := data.([]any); !ok {
		return fmt.Sprintf("Connected to %s, but its response is not an OpenAI-compatible model list.", url)
	}
	return fmt.Sprintf("Connected to %s, but it advertised no models. Start a model on that endpoint and try again.", url)
}

// GET of base/models then the v1 toggled alternate, returning the base that served it and its response
func probeOpenAICompatibleModels(ctx context.Context, baseURL string, headers [][2]string) (string, *pyhttpx.Response) {
	base := strings.TrimRight(baseURL, "/")
	alternate := base + "/v1"
	if strings.HasSuffix(pystr.Lower(base), "/v1") {
		alternate = strings.TrimRight(base[:len(base)-3], "/")
	}
	resolved := base
	var resp *pyhttpx.Response
	client := endpointProbeClient(base)
	for _, candidate := range []string{base, alternate} {
		candidateResp, err := probeRequest(ctx, client, http.MethodGet, candidate+"/models", headers, nil, 8*time.Second)
		if err != nil {
			continue
		}
		if resp == nil || httpxIsSuccess(candidateResp) || resp.StatusCode == 404 {
			resolved, resp = candidate, candidateResp
		}
		if httpxIsSuccess(candidateResp) {
			break
		}
	}
	return resolved, resp
}

// Request through a probe client with ordered headers and an optional JSON body
func probeRequest(ctx context.Context, client *http.Client, method, url string, headers [][2]string, payload any, timeout time.Duration) (*pyhttpx.Response, error) {
	var body *bytes.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, body)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	return pyhttpx.Do(client, req, timeout)
}

var transportRoutes = map[string]string{"chat_completions": "/chat/completions", "codex_responses": "/responses",
	"anthropic_messages": "/messages"}
var transportLabels = map[string]string{"chat_completions": "Chat Completions", "codex_responses": "Responses API",
	"anthropic_messages": "Anthropic Messages"}

// Transport the runtime falls back to for an endpoint with no pinned api mode
func autoAPIMode(baseURL string) string {
	if mode := string(provider.DetectAPIModeForURL(baseURL)); mode != "" {
		return mode
	}
	return "chat_completions"
}

// One token POST to the mode's route, a failure message when the host does not serve it
func probeTransportRoute(ctx context.Context, client *http.Client, baseURL, mode, model string, headers [][2]string) string {
	route, ok := transportRoutes[mode]
	if !ok {
		return ""
	}
	var payload any
	switch mode {
	case "anthropic_messages":
		payload = pyjson.ObjectOf("model", model, "max_tokens", 1, "messages", []any{pyjson.ObjectOf("role", "user", "content", "hi")})
		token := ""
		for _, h := range headers {
			if h[0] == "Authorization" {
				token = strings.TrimPrefix(h[1], "Bearer ")
			}
		}
		headers = append(append([][2]string{}, headers...), [2]string{"anthropic-version", "2023-06-01"})
		if token != "" {
			headers = append(headers, [2]string{"x-api-key", token})
		}
	case "codex_responses":
		payload = pyjson.ObjectOf("model", model, "input", "hi", "max_output_tokens", 16)
	default:
		payload = pyjson.ObjectOf("model", model, "max_tokens", 1, "messages", []any{pyjson.ObjectOf("role", "user", "content", "hi")})
	}
	resp, err := probeRequest(ctx, client, http.MethodPost, baseURL+route, headers, payload, 8*time.Second)
	if err != nil {
		return ""
	}
	if resp.StatusCode != 404 && resp.StatusCode != 405 && resp.StatusCode != 501 {
		return ""
	}
	return fmt.Sprintf("%s/models answered, but POST %s returned HTTP %d: this host does not serve the %s API. "+
		"Pick the API mode it does serve.", baseURL, route, resp.StatusCode, transportLabels[mode])
}

// Probe client constructor of the endpoint validators, the module attribute hermes tests patch
var endpointProbeClient = newEndpointProbeClient

// Probe client of a user entered endpoint, local endpoints ignoring the environment proxy
func newEndpointProbeClient(url string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	if provider.IsLocalEndpoint(url) {
		transport.Proxy = nil
	}
	return &http.Client{Transport: transport, CheckRedirect: pyhttpx.RedirectPolicy(false)}
}

// Live probe of a provider credential, ok false and reachable true meaning the key was refused
func validateProviderCredential(ctx context.Context, body *pyjson.Object) *pyjson.Object {
	key := bodyStrip(body, "key")
	value := bodyStrip(body, "value")
	if value == "" {
		return pyjson.ObjectOf("ok", false, "reachable", true, "message", "Enter a value first.")
	}
	if key == "OPENAI_BASE_URL" {
		var headers [][2]string
		if apiKey := bodyStrip(body, "api_key"); apiKey != "" {
			headers = [][2]string{{"Authorization", "Bearer " + apiKey}}
		}
		resolved, resp := probeOpenAICompatibleModels(ctx, value, headers)
		url := resolved + "/models"
		if resp == nil {
			return pyjson.ObjectOf("ok", false, "reachable", false, "message", fmt.Sprintf("Could not reach %s.", url))
		}
		entries := parseModelEntries(resp)
		models := make([]any, len(entries))
		details := make([]any, len(entries))
		for i, e := range entries {
			models[i] = e.GetOr("id", "")
			details[i] = e
		}
		if len(models) == 0 && !httpxIsSuccess(resp) {
			return pyjson.ObjectOf("ok", false, "reachable", true, "message", fmt.Sprintf("%s answered HTTP %d.", url, resp.StatusCode), "models", []any{})
		}
		return pyjson.ObjectOf("ok", true, "reachable", true, "message", "", "models", models, "model_details", details,
			"resolved_base_url", resolved)
	}
	probe, ok := credentialProbes[key]
	if !ok {
		return pyjson.ObjectOf("ok", true, "reachable", false, "message", "")
	}
	url, auth := probe[0], probe[1]
	if key == "GEMINI_API_KEY" {
		cut := url
		if i := strings.LastIndex(url, "/models"); i >= 0 {
			cut = url[:i]
		}
		url = provider.NormalizeGeminiBaseURL(cut) + "/models"
	}
	headers := [][2]string{{"Accept", "application/json"}}
	if auth == "bearer" {
		headers = append(headers, [2]string{"Authorization", "Bearer " + value})
	} else {
		url = pyhttpx.MergeParams(url, pyhttpx.Params{{"key", value}})
	}
	resp, err := probeRequest(ctx, &http.Client{CheckRedirect: pyhttpx.RedirectPolicy(false)}, http.MethodGet, url, headers, nil, 10*time.Second)
	if err != nil {
		return pyjson.ObjectOf("ok", false, "reachable", false, "message", "Could not reach the provider to verify the key.")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return pyjson.ObjectOf("ok", false, "reachable", true, "message", "That API key was rejected. Double-check it and try again.")
	}
	if resp.StatusCode == 429 || httpxIsSuccess(resp) {
		return pyjson.ObjectOf("ok", true, "reachable", true, "message", "")
	}
	return pyjson.ObjectOf("ok", false, "reachable", true, "message", fmt.Sprintf("Provider returned HTTP %d for this key.", resp.StatusCode))
}
