package provider

import (
	"context"
)

// Model name and provider of the config model section, a plain string section naming the model alone
func mainModelFields(modelCfg any) (any, any) {
	if m, ok := modelCfg.(map[string]any); ok {
		model, held := m["default"]
		if !held {
			model = pyGetAny(m, "name", "")
		}
		return model, pyGetAny(m, "provider", "")
	}
	if pyTruthy(modelCfg) {
		return pyStr(modelCfg), ""
	}
	return "", ""
}

// GetAuxiliaryModels lists each auxiliary slot's stored route, plugin tasks with their labels and live inherited route, and the main model
func GetAuxiliaryModels(ctx context.Context, profile string) (map[string]any, error) {
	detail := "Failed to read auxiliary config"
	out, err := getAuxiliaryModels(ctx, profile)
	if err != nil {
		return nil, httpFailure(err, "GET /api/model/auxiliary failed", 500, "", &detail)
	}
	return out, nil
}

// Auxiliary listing read under the profile scope, plugin discovery and resolution keyed on that profile's home
func getAuxiliaryModels(ctx context.Context, profile string) (map[string]any, error) {
	sctx, _, err := ConfigProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	cfg, err := configLoadFn(sctx)
	if err != nil {
		return nil, err
	}
	pluginTasks := pluginAuxTasks(sctx)
	effective := map[string]map[string]any{}
	for _, entry := range pluginTasks {
		if pyTruthy(entry["inherit_from"]) {
			key := entry["key"].(string)
			if effective[key], err = auxGetAuxiliaryTaskConfig(sctx, key); err != nil {
				return nil, err
			}
		}
	}
	auxCfg, ok := pyGetAny(cfg, "auxiliary", map[string]any{}).(map[string]any)
	if !ok {
		auxCfg = map[string]any{}
	}
	row := func(slot string) map[string]any {
		slotCfg, ok := auxCfg[slot].(map[string]any)
		if !ok {
			slotCfg = map[string]any{}
		}
		baseURL := pyOrStr(pyGetAny(slotCfg, "base_url", ""))
		provider := pyOrStr(pyGetAny(slotCfg, "provider", "auto"))
		if provider == "" {
			provider = "auto"
		}
		var effort any
		if level := pyOrStr(slotCfg["reasoning_effort"]); level != "" {
			effort = level
		}
		return map[string]any{"task": slot, "provider": provider, "model": pyOrStr(pyGetAny(slotCfg, "model", "")),
			"base_url": baseURL, "reasoning_effort": effort, "local_endpoint": IsLocalEndpoint(baseURL)}
	}
	tasks := []any{}
	for _, slot := range auxTaskSlotsBuiltin {
		tasks = append(tasks, row(slot))
	}
	for _, entry := range pluginTasks {
		key := entry["key"].(string)
		r := row(key)
		label := pyOrStr(entry["display_name"])
		if label == "" {
			label = key
		}
		var inherit any
		if pyTruthy(entry["inherit_from"]) {
			inherit = entry["inherit_from"]
		}
		r["label"], r["hint"], r["plugin"], r["inherit_from"] = label, pyOrStr(entry["description"]), pyOrStr(entry["plugin"]), inherit
		if resolved, held := effective[key]; held {
			r["effective"] = map[string]any{"provider": pyOrStr(resolved["provider"]), "model": pyOrStr(resolved["model"]),
				"base_url": pyOrStr(resolved["base_url"])}
		}
		tasks = append(tasks, r)
	}
	model, provider := mainModelFields(pyGetAny(cfg, "model", map[string]any{}))
	return map[string]any{"tasks": tasks, "main": map[string]any{"provider": pyOrStr(provider), "model": pyOrStr(model)}}, nil
}
