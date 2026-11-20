package manage

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Pins the requested model, a pick the model switch refuses recorded as the result's model error instead of a failed step
func pinModel(ctx context.Context, path string, req ProfileCreateRequest, result *ProvisionResult) (bool, error) {
	err := WriteProfileModel(ctx, path, req.Provider, req.Model)
	var rejected *provider.ModelAssignmentRejected
	if errors.As(err, &rejected) {
		result.ModelError = rejected.Error()
		return false, nil
	}
	return err == nil, err
}

// Effective config of the home a context is scoped to
func loadScopedConfig(ctx context.Context) (map[string]any, error) {
	tree, err := config.LoadContext(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
	return map[string]any(tree), err
}

// Raw user config of the home a context is scoped to with its model section canonical, empty when the home has none
func rawScopedConfig(ctx context.Context) (map[string]any, error) {
	raw, err := config.ReadUserConfigRaw(ctx, config.GetConfigPath(home.Dir(ctx)))
	if err != nil {
		return nil, err
	}
	return config.NormalizeRootModelKeys(raw), nil
}

// Writes a validated main model assignment into a profile's config, the profile's own catalog and secrets vouching for the pick
func WriteProfileModel(ctx context.Context, profileDir, providerName, model string) error {
	foreign := resolvePath(profileDir) != resolvePath(home.Process())
	if foreign {
		provider.HydrateProfileSecretSources(profileDir)
	}
	validating := provider.ProfileConfigScope(ctx, profileDir, "")
	if foreign {
		validating = provider.WithMultiplexContext(validating, true)
	}
	providerName, model = provider.NormalizeMainModelAssignment(validating, providerName, model)
	cfg, err := loadScopedConfig(validating)
	if err != nil {
		return err
	}
	selection, err := provider.ValidatedMainModelSelection(validating, cfg, providerName, model, "")
	if err != nil {
		return err
	}
	scoped, unlock := provider.LockConfigMutation(provider.ProfileConfigScope(ctx, profileDir, ""))
	defer unlock()
	if cfg, err = loadScopedConfig(scoped); err != nil {
		return err
	}
	current, held := cfg["model"]
	if !held {
		current = map[string]any{}
	}
	assigned, err := provider.ApplyMainModelAssignment(scoped, current, selection)
	if err != nil {
		return err
	}
	cfg["model"] = assigned
	return config.SaveConfig(config.RefusingLockedOmissions(scoped, "model"), home.Dir(scoped), cfg, config.SaveOptions{})
}

// Model section of a raw config as a mapping, empty for an absent or empty one, an error for any other shape
func modelSection(cfg map[string]any, where string) (map[string]any, error) {
	section := cfg["model"]
	if !truthy(section) {
		return map[string]any{}, nil
	}
	mapping, isMapping := section.(map[string]any)
	if !isMapping {
		return nil, fmt.Errorf("the model section of %s is not a mapping", where)
	}
	return mapping, nil
}

// Copies the launch profile's model and the endpoint it selects into a profile without validating them, a profile that already names a provider and model left alone
func InheritLaunchModel(ctx context.Context, path string) (bool, error) {
	launch, unlock := provider.LockConfigMutation(ctx)
	defer unlock()
	target := provider.ProfileConfigScope(launch, path, "")
	cfg, err := rawScopedConfig(target)
	if err != nil {
		return false, err
	}
	dstModel, err := modelSection(cfg, config.GetConfigPath(path))
	if err != nil {
		return false, err
	}
	if truthy(dstModel["provider"]) && truthy(dstModel["default"]) {
		return false, nil
	}
	effective, err := config.LoadReadonly(launch, home.Dir(launch), config.EnvRefLookupContext(launch))
	if err != nil {
		return false, err
	}
	launchModel, err := modelSection(effective, "the launch profile")
	if err != nil {
		return false, err
	}
	if !truthy(launchModel["provider"]) || !truthy(launchModel["default"]) {
		return false, nil
	}
	providerName, model := pyrepr.Str(launchModel["provider"]), pyrepr.Str(launchModel["default"])
	if normalizedProvider, normalizedModel := provider.NormalizeMainModelAssignment(launch, providerName, model); normalizedProvider == providerName {
		model = normalizedModel
	}
	raw, err := rawScopedConfig(launch)
	if err != nil {
		return false, err
	}
	rawModel, err := modelSection(raw, "the launch profile")
	if err != nil {
		return false, err
	}
	pinned := make(map[string]any, len(rawModel)+2)
	order := config.MappingKeys(rawModel)
	for key, value := range rawModel {
		pinned[key] = value
	}
	for _, key := range []string{"provider", "default"} {
		if _, held := pinned[key]; !held {
			order = append(order, key)
		}
	}
	pinned["provider"], pinned["default"] = providerName, model
	config.SetMappingKeys(pinned, order)
	source := make(map[string]any, len(raw))
	for key, value := range raw {
		source[key] = value
	}
	config.SetMappingKeys(source, config.MappingKeys(raw))
	source["model"] = pinned
	seed, claimed := provider.PrepareInheritedEndpointSeed(launch, source)
	merged := provider.MergeEndpointSeed(target, cfg, seed, claimed)
	assigned, err := provider.ApplyInheritedModelAssignment(target, dstModel, pinned)
	if err != nil {
		return false, err
	}
	keys := config.MappingKeys(merged)
	if _, held := merged["model"]; !held {
		keys = append(keys, "model")
	}
	merged["model"] = assigned
	config.SetMappingKeys(merged, keys)
	saving := config.RefusingLockedOmissions(target, "model", "providers", "custom_providers")
	if err := config.SaveConfig(saving, home.Dir(saving), merged, config.SaveOptions{}); err != nil {
		return false, err
	}
	return true, nil
}
