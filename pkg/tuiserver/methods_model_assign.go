package tuiserver

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var modelAssignRegistry = &handlerRegistry{}

func init() {
	modelAssignRegistry.scopedMethod("model.probe", catch(5038, modelProbe))
	modelAssignRegistry.scopedMethod("model.assign", catch(5039, modelAssign))
	modelAssignRegistry.install()
}

// Models an OpenAI compatible endpoint lists, with the base URL that answered the probe
func modelProbe(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	baseURL := pystr.Strip(strOr(params["base_url"]))
	if baseURL == "" {
		return rpcErr(rid, 4001, "base_url is required", nil), nil
	}
	probe := provider.ProbeAPIModels(ctx, pystr.Strip(strOr(params["api_key"])), baseURL, 5*time.Second, "", nil)
	if probe.UsedFallback && probe.ResolvedBaseURL != "" {
		baseURL = probe.ResolvedBaseURL
	}
	models := make([]any, len(probe.Models))
	for i, model := range probe.Models {
		models[i] = model
	}
	return ok(rid, map[string]any{"base_url": baseURL, "found": probe.Found, "models": models,
		"probed_url": probe.ProbedURL, "suggested_base_url": probe.SuggestedBaseURL}), nil
}

// Writes the default model and provider of the scoped profile, registering a new custom endpoint with its key
func modelAssign(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	if skew := modelSkewErr(ctx, rid); skew != nil {
		return skew, nil
	}
	providerName, model := pystr.Strip(strOr(params["provider"])), pystr.Strip(strOr(params["model"]))
	baseURL, apiKey := pystr.Strip(strOr(params["base_url"])), pystr.Strip(strOr(params["api_key"]))
	if providerName == "" || model == "" {
		return rpcErr(rid, 4001, "provider and model are required", nil), nil
	}
	if config.IsManaged(ctx, home.Dir(ctx)) {
		return rpcErr(rid, 4006, "managed install — the model is read-only", nil), nil
	}
	if !truthy(params["confirm_expensive_model"]) {
		if warning := provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: model, Provider: providerName, BaseURL: baseURL}); warning != nil {
			return ok(rid, map[string]any{"provider": providerName, "model": model, "base_url": baseURL,
				"confirm_required": true, "confirm_message": warning.Message}), nil
		}
	}
	prepared, err := provider.PrepareMainAssignment(ctx, loadCfg(ctx), providerName, model, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	locked, unlock := provider.LockConfigMutation(ctx)
	defer unlock()
	saved, err := provider.ApplyModelAssignmentSync(locked, "main", providerName, model, "", baseURL, apiKey, provider.AuxReasoningEffort{}, prepared)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"provider": saved["provider"], "model": saved["model"], "base_url": saved["base_url"],
		"confirm_required": false, "confirm_message": ""}), nil
}
