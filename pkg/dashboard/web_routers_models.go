package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var modelCapabilityFields = []string{"supports_tools", "supports_vision", "supports_reasoning", "context_window",
	"max_output_tokens", "model_family"}

var moaPresetFields = []string{"reference_temperature", "aggregator_temperature", "reference_timeout",
	"degraded_reference_policy", "fanout", "enabled"}

// Blocking budget of the context length resolution behind the model info route
var modelInfoProbeBudget = 5 * time.Second

// Context length resolver the model info route probes, the module attribute hermes tests patch
var modelInfoContextLength = provider.GetModelContextLength

// Picker inputs and payload builders and the assignment steps of the model routes, the names hermes tests patch
var (
	loadPickerContext        = provider.LoadPickerContext
	buildModelOptionsPayload = provider.BuildModelOptionsPayload
	buildModelsPayload       = provider.BuildModelsPayload
	prepareMainAssignment    = provider.PrepareMainAssignment
	applyModelAssignmentSync = provider.ApplyModelAssignmentSync
	combinedSelectionWarning = provider.CombinedSelectionWarning
)

// Model info answer of a config with no model set
func emptyModelInfo() *pyjson.Object {
	return pyjson.ObjectOf("model", "", "provider", "", "auto_context_length", int64(0), "config_context_length", int64(0),
		"effective_context_length", int64(0), "capabilities", pyjson.NewObject())
}

// Error of fn, a panic inside it turned into an error the way Python code catches an Exception
func raised(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

// Model and provider of the config model section, a plain string section naming the model alone
func mainModelFields(modelCfg any) (any, any) {
	if m, ok := modelCfg.(map[string]any); ok {
		model, held := m["default"]
		if !held {
			if model, held = m["name"]; !held {
				model = ""
			}
		}
		prov, held := m["provider"]
		if !held {
			prov = ""
		}
		return model, prov
	}
	if pyrepr.Truthy(modelCfg) {
		return pyrepr.Str(modelCfg), ""
	}
	return "", ""
}

// Effective config of the requested profile read under its scope, the module attribute hermes tests patch
var loadConfigScoped = func(ctx context.Context, profile string) (map[string]any, error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	return webLoadConfig(sctx)
}

// Text of a config scalar the resolvers take, null reading as empty and any other non string refused
func modelRouteText(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	}
	return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))
}

// Auto detected context length within the route budget, zero once the budget passes while the probe runs on
func boundedContextLengthProbe(ctx context.Context, model, baseURL, providerName string) (int, error) {
	type outcome struct {
		n   int
		err error
	}
	done := make(chan outcome, 1)
	probe := modelInfoContextLength
	go func() {
		var n int
		err := raised(func() error {
			n = probe(context.WithoutCancel(ctx), provider.ContextLengthRequest{Model: model, BaseURL: baseURL, Provider: providerName})
			return nil
		})
		done <- outcome{n, err}
	}()
	timer := time.NewTimer(modelInfoProbeBudget)
	defer timer.Stop()
	select {
	case got := <-done:
		return got.n, got.err
	case <-timer.C:
		at := baseURL
		if at == "" {
			at = "<default>"
		}
		routersLog().Warn(fmt.Sprintf("GET /api/model/info: context-length probe for %s at %s exceeded %.1fs — returning unknown",
			pyrepr.Repr(model), at, modelInfoProbeBudget.Seconds()))
		return 0, nil
	}
}

// Positive int of a config value the way isinstance int reads it, a true bool kept as is, else zero
func positiveConfigInt(v any) any {
	switch n := v.(type) {
	case bool:
		if n {
			return true
		}
	case int:
		if n > 0 {
			return int64(n)
		}
	case int64:
		if n > 0 {
			return n
		}
	case uint64:
		if n > 0 {
			return n
		}
	}
	return int64(0)
}

// Capability view of a model as the info route lists it, empty for an unknown model or a failed lookup
func modelInfoCapabilities(ctx context.Context, providerName, model any) *pyjson.Object {
	caps := pyjson.NewObject()
	_ = raised(func() error {
		p, err := modelRouteText(providerName)
		if err != nil {
			return err
		}
		m, err := modelRouteText(model)
		if err != nil {
			return err
		}
		mc, found := provider.ModelsDevCapabilities(ctx, p, m, false)
		if !found {
			return nil
		}
		var vision, reasoning, maxOut any
		if mc.SupportsVision != nil {
			vision = *mc.SupportsVision
		}
		if mc.SupportsReasoning != nil {
			reasoning = *mc.SupportsReasoning
		}
		if mc.MaxOutputTokens != 0 {
			maxOut = int64(mc.MaxOutputTokens)
		}
		values := map[string]any{"supports_tools": mc.SupportsTools, "supports_vision": vision, "supports_reasoning": reasoning,
			"context_window": int64(mc.ContextWindow), "max_output_tokens": maxOut, "model_family": mc.ModelFamily}
		for _, name := range modelCapabilityFields {
			caps.Set(name, values[name])
		}
		return nil
	})
	return caps
}

// Configured model with its auto detected and configured context lengths and its models.dev capabilities
func getModelInfo(ctx context.Context, profile string) (any, error) {
	var out any
	err := raised(func() error {
		cfg, err := loadConfigScoped(ctx, profile)
		if err != nil {
			return err
		}
		modelCfg, held := cfg["model"]
		if !held {
			modelCfg = ""
		}
		modelName, providerName := mainModelFields(modelCfg)
		var baseURL, configCtx any = "", nil
		if m, ok := modelCfg.(map[string]any); ok {
			if baseURL, held = m["base_url"]; !held {
				baseURL = ""
			}
			configCtx = m["context_length"]
		}
		if !pyrepr.Truthy(modelName) {
			info := emptyModelInfo()
			info.Set("provider", configJSON(providerName))
			out = info
			return nil
		}
		autoCtx := 0
		if probeErr := raised(func() error {
			m, err := modelRouteText(modelName)
			if err != nil {
				return err
			}
			b, err := modelRouteText(baseURL)
			if err != nil {
				return err
			}
			p, err := modelRouteText(providerName)
			if err != nil {
				return err
			}
			autoCtx, err = boundedContextLengthProbe(ctx, m, b, p)
			return err
		}); probeErr != nil {
			autoCtx = 0
		}
		configCtxInt := positiveConfigInt(configCtx)
		var effective any = int64(autoCtx)
		if pyrepr.Truthy(configCtxInt) {
			effective = configCtxInt
		}
		out = pyjson.ObjectOf("model", configJSON(modelName), "provider", configJSON(providerName), "auto_context_length", int64(autoCtx),
			"config_context_length", configCtxInt, "effective_context_length", effective,
			"capabilities", modelInfoCapabilities(ctx, providerName, modelName))
		return nil
	})
	if err == nil {
		return out, nil
	}
	if h, ok := asHTTPException(err); ok {
		return nil, h
	}
	routersLog().Error("GET /api/model/info failed", "exc_info", err)
	return emptyModelInfo(), nil
}

// 503 of a process running code older than the checkout, nil when the code is current
func modelRouteSkewRefusal(route string) error {
	skew := dashboardCodeSkewGuard()
	if skew == "" {
		return nil
	}
	routersLog().Warn(fmt.Sprintf("%s refused: %s", route, skew))
	return NewHTTPException(503, "Restart required: "+skew)
}

// Authenticated providers with their curated model lists for the picker of the requested profile
func getModelOptions(ctx context.Context, profile string, refresh, includeUnconfigured, explicitOnly bool) (any, error) {
	var out any
	err := raised(func() error {
		if err := modelRouteSkewRefusal("GET /api/model/options"); err != nil {
			return err
		}
		sctx, _, err := configProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		pc, err := loadPickerContext(sctx)
		if err != nil {
			return err
		}
		payload, err := buildModelOptionsPayload(sctx, pc, explicitOnly, includeUnconfigured, refresh)
		if err != nil {
			return err
		}
		out = configJSON(payload)
		return nil
	})
	if err != nil {
		detail := "Failed to list model options"
		return nil, httpFailure(err, "GET /api/model/options failed", 500, "", &detail)
	}
	return out, nil
}

// Recommended default of the Nous account in ctx, the module attribute hermes tests patch
var nousRecommendedDefault = func(ctx context.Context) (*pyjson.Object, error) {
	rec := provider.RecommendedNousDefaultModel(ctx)
	return pyjson.ObjectOf("provider", rec.Provider, "model", rec.Model, "free_tier", rec.FreeTier), nil
}

// Model ids of a picker row as text
func pickerRowModels(row map[string]any) []string {
	out := []string{}
	switch models := row["models"].(type) {
	case []string:
		out = append(out, models...)
	case []any:
		for _, m := range models {
			out = append(out, pyrepr.Str(m))
		}
	}
	return out
}

// Default model a freshly authenticated provider lands on, the tier aware pick for Nous and the silent default elsewhere
func getRecommendedDefaultModel(ctx context.Context, providerName, profile string) (any, error) {
	slug := pystr.Lower(pystr.Strip(providerName))
	if slug == "nous" {
		var out *pyjson.Object
		err := raised(func() error {
			sctx, _, err := configProfileScope(ctx, profile)
			if err != nil {
				return err
			}
			out, err = nousRecommendedDefault(sctx)
			return err
		})
		if err == nil {
			return out, nil
		}
		if h, ok := asHTTPException(err); ok {
			return nil, h
		}
		routersLog().Error("GET /api/model/recommended-default (nous) failed", "exc_info", err)
		return pyjson.ObjectOf("provider", "nous", "model", "", "free_tier", nil), nil
	}
	var out *pyjson.Object
	err := raised(func() error {
		sctx, _, err := configProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		pc, err := loadPickerContext(sctx)
		if err != nil {
			return err
		}
		payload, err := buildModelsPayload(sctx, pc, provider.ModelsPayloadOptions{ProbeCustomProviders: true})
		if err != nil {
			return err
		}
		rows, _ := payload["providers"].([]any)
		for _, item := range rows {
			row, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(item))
			}
			rowSlug, held := row["slug"]
			if !held {
				rowSlug = ""
			}
			if pystr.Lower(pyrepr.Str(rowSlug)) == slug {
				out = pyjson.ObjectOf("provider", slug, "model", provider.PickSilentDefaultModel(ctx, pickerRowModels(row), slug), "free_tier", nil)
				return nil
			}
		}
		out = pyjson.ObjectOf("provider", slug, "model", "", "free_tier", nil)
		return nil
	})
	if err == nil {
		return out, nil
	}
	if h, ok := asHTTPException(err); ok {
		return nil, h
	}
	routersLog().Error("GET /api/model/recommended-default failed", "exc_info", err)
	return pyjson.ObjectOf("provider", slug, "model", "", "free_tier", nil), nil
}

// Auxiliary slot assignments of the requested profile with plugin tasks and the main model
func getAuxiliaryModels(ctx context.Context, profile string) (any, error) {
	var out any
	err := raised(func() error {
		sctx, _, release, err := profileScope(ctx, profile)
		if err != nil {
			return err
		}
		defer release()
		listing, err := provider.GetAuxiliaryModels(sctx, profile)
		if err != nil {
			return err
		}
		out = configJSON(listing)
		return nil
	})
	if err != nil {
		detail := "Failed to read auxiliary config"
		return nil, httpFailure(err, "GET /api/model/auxiliary failed", 500, "", &detail)
	}
	return out, nil
}

// Mixture of Agents slots of the requested profile in their normalized shape
func getMoaModels(ctx context.Context, profile string) (any, error) {
	var out any
	err := raised(func() error {
		sctx, _, release, err := profileScope(ctx, profile)
		if err != nil {
			return err
		}
		defer release()
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return err
		}
		out = configJSON(provider.NormalizeMoAConfig(cfg["moa"]))
		return nil
	})
	if err != nil {
		detail := "Failed to read MoA config"
		return nil, httpFailure(err, "GET /api/model/moa failed", 500, "", &detail)
	}
	return out, nil
}

// Saved form of one slot, unset optionals left out
func moaSlotDict(slot *pyjson.Object) map[string]any {
	out := map[string]any{}
	keys := []string{}
	for _, k := range slot.Keys {
		if v := slot.Vals[k]; v != nil {
			out[k] = v
			keys = append(keys, k)
		}
	}
	config.SetMappingKeys(out, keys)
	return out
}

// Raw preset mapping of a preset payload or of the flat fields of the config payload
func moaPresetDict(preset *pyjson.Object) map[string]any {
	refs := []any{}
	slots, _ := preset.GetOr("reference_models", nil).([]any)
	for _, slot := range slots {
		refs = append(refs, moaSlotDict(slot.(*pyjson.Object)))
	}
	out := map[string]any{"reference_models": refs, "aggregator": moaSlotDict(preset.GetOr("aggregator", nil).(*pyjson.Object))}
	for _, name := range moaPresetFields {
		out[name] = preset.GetOr(name, nil)
	}
	config.SetMappingKeys(out, append([]string{"reference_models", "aggregator"}, moaPresetFields...))
	return out
}

// Persists the Mixture of Agents slots, refusing a payload the normalizer would silently repair
func setMoaModels(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	var out any
	err := raised(func() error {
		scope := profile
		if p, _ := body.GetOr("profile", nil).(string); p != "" {
			scope = p
		}
		wctx, release, err := configWriteScope(ctx, scope)
		if err != nil {
			return err
		}
		defer release()
		lctx, unlock := config.LockConfig(wctx)
		defer unlock()
		presets, _ := body.GetOr("presets", nil).(*pyjson.Object)
		legacyFlatUpdate := presets == nil || presets.Len() == 0
		var raw map[string]any
		if !legacyFlatUpdate {
			named := make(map[string]any, presets.Len())
			for _, name := range presets.Keys {
				named[name] = moaPresetDict(presets.Vals[name].(*pyjson.Object))
			}
			config.SetMappingKeys(named, presets.Keys)
			raw = map[string]any{"default_preset": body.GetOr("default_preset", nil), "active_preset": body.GetOr("active_preset", nil),
				"presets": named}
			config.SetMappingKeys(raw, []string{"default_preset", "active_preset", "presets"})
		} else {
			raw = moaPresetDict(body)
		}
		if problems := provider.ValidateMoAPayload(raw); len(problems) > 0 {
			return NewHTTPException(422, "Invalid MoA config: "+strings.Join(problems, "; "))
		}
		rawCfg, err := config.RequireReadableConfigBeforeWrite(lctx, "")
		if err != nil {
			return err
		}
		existingMoa, existingIsDict := rawCfg["moa"].(map[string]any)
		if existingIsDict {
			if filter, held := existingMoa["privacy_filter"]; held {
				raw["privacy_filter"] = filter
				config.AppendMappingKey(raw, "privacy_filter")
			}
		}
		normalized := provider.NormalizeMoAConfig(raw)
		normalizedPresets := normalized["presets"].(map[string]any)
		if legacyFlatUpdate && existingIsDict {
			if existingPresets, ok := existingMoa["presets"].(map[string]any); ok {
				if existingDefault, isStr := existingMoa["default_preset"].(string); isStr {
					if _, held := existingPresets[existingDefault]; held {
						if synthesized, isStr := normalized["default_preset"].(string); isStr {
							if preset, held := normalizedPresets[synthesized]; held {
								delete(normalizedPresets, synthesized)
								normalizedPresets[existingDefault] = preset
								config.AppendMappingKey(normalizedPresets, existingDefault)
								normalized["default_preset"] = existingDefault
							}
						}
					}
				}
				if existingActive, isStr := existingMoa["active_preset"].(string); isStr {
					if _, held := existingPresets[existingActive]; held {
						normalized["active_preset"] = existingActive
					}
				}
			}
		}
		updated := config.MergePartialSave(rawCfg, map[string]any{"moa": normalized})
		if !legacyFlatUpdate {
			merged := updated["moa"].(map[string]any)
			mergedPresets := merged["presets"].(map[string]any)
			kept := make(map[string]any, len(normalizedPresets))
			order := config.MappingKeys(normalizedPresets)
			for _, name := range order {
				kept[name] = mergedPresets[name]
			}
			config.SetMappingKeys(kept, order)
			merged["presets"] = kept
		}
		if err := webSaveConfig(lctx, updated, config.SaveOptions{}); err != nil {
			return err
		}
		answer := pyjson.ObjectOf("ok", true)
		answer.Update(configJSON(normalized).(*pyjson.Object))
		out = answer
		return nil
	})
	if err != nil {
		detail := "Failed to save MoA config"
		return nil, httpFailure(err, "PUT /api/model/moa failed", 500, "", &detail)
	}
	return out, nil
}

// Assigns a model to the main slot or an auxiliary task slot of the requested profile
func setModelAssignment(ctx context.Context, body *pyjson.Object, fieldsSet map[string]bool, profile string) (any, error) {
	scope, task := pystr.Lower(bodyStrip(body, "scope")), pystr.Lower(bodyStrip(body, "task"))
	providerName, model := bodyStrip(body, "provider"), bodyStrip(body, "model")
	baseURL, apiKey := bodyStrip(body, "base_url"), bodyStrip(body, "api_key")
	if scope != "main" && scope != "auxiliary" {
		return nil, NewHTTPException(400, "scope must be 'main' or 'auxiliary'")
	}
	var out any
	err := raised(func() error {
		if err := modelRouteSkewRefusal("POST /api/model/set"); err != nil {
			return err
		}
		if confirmed, _ := body.GetOr("confirm_expensive_model", nil).(bool); model != "" && !confirmed {
			var warning *provider.SelectionWarning
			if raised(func() error {
				warning = combinedSelectionWarning(ctx, provider.SelectionRequest{Model: model, Provider: providerName, BaseURL: baseURL})
				return nil
			}) != nil {
				warning = nil
			}
			if warning != nil {
				out = pyjson.ObjectOf("ok", false, "scope", scope, "provider", providerName, "model", model,
					"confirm_required", true, "confirm_message", warning.Message)
				return nil
			}
		}
		var effort provider.AuxReasoningEffort
		if fieldsSet["reasoning_effort"] {
			effort.Present = true
			if level, ok := body.GetOr("reasoning_effort", nil).(string); ok {
				effort.Value = &level
			}
		}
		target := profile
		if p, _ := body.GetOr("profile", nil).(string); p != "" {
			target = p
		}
		sctx, _, release, err := profileScope(ctx, target)
		if err != nil {
			return err
		}
		defer release()
		var prepared *provider.PreparedMainAssignment
		if scope == "main" {
			cfg, err := webLoadConfig(sctx)
			if err != nil {
				return err
			}
			if prepared, err = prepareMainAssignment(sctx, cfg, providerName, model, baseURL, apiKey); err != nil {
				return err
			}
		}
		lctx, unlock := provider.LockConfigMutation(sctx)
		defer unlock()
		result, err := applyModelAssignmentSync(lctx, scope, providerName, model, task, baseURL, apiKey, effort, prepared)
		if err != nil {
			return err
		}
		out = configJSON(result)
		return nil
	})
	if err != nil {
		detail := "Failed to save model assignment"
		return nil, httpFailure(err, "POST /api/model/set failed", 500, "", &detail)
	}
	return out, nil
}

// Model assignment routes of the dashboard in their declaration order
func modelsRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/model/info", &Endpoint{
		Name:   "get_model_info",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getModelInfo(ctx, profileQuery(c)) },
	})
	r.Get("/api/model/options", &Endpoint{
		Name: "get_model_options",
		Params: []Param{optProfile, QueryParam("refresh", Bool, false), QueryParam("include_unconfigured", Bool, false),
			QueryParam("explicit_only", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getModelOptions(ctx, profileQuery(c), c.Bool("refresh"), c.Bool("include_unconfigured"), c.Bool("explicit_only"))
		},
	})
	r.Get("/api/model/recommended-default", &Endpoint{
		Name:   "get_recommended_default_model",
		Params: []Param{QueryParam("provider", Str, ""), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getRecommendedDefaultModel(ctx, c.Str("provider"), profileQuery(c))
		},
	})
	r.Get("/api/model/auxiliary", &Endpoint{
		Name:   "get_auxiliary_models",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getAuxiliaryModels(ctx, profileQuery(c)) },
	})
	r.Get("/api/model/moa", &Endpoint{
		Name:   "get_moa_models",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getMoaModels(ctx, profileQuery(c)) },
	})
	r.Put("/api/model/moa", &Endpoint{
		Name:   "set_moa_models",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", moaConfigPayloadModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return setMoaModels(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/model/set", &Endpoint{
		Name:   "set_model_assignment",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", modelAssignmentModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return setModelAssignment(ctx, c.Object("body"), c.FieldsSet("body", modelAssignmentModel), profileQuery(c))
		},
	})
	return r
}
