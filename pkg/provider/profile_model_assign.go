package provider

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Model block keys bound to one endpoint, never carried from another profile's block
var inheritBoundKeys = []string{"api_key", "api", "key_env", "api_key_env", "context_length"}

// ModelAssignmentRejected is a main model pick the model switch refused, its message saying why
type ModelAssignmentRejected struct{ Msg string }

func (e *ModelAssignmentRejected) Error() string { return e.Msg }

// Context bound to the home, secret and terminal scope of a home whose config a body reads and writes, launchHome empty meaning the process home
func ProfileConfigScope(ctx context.Context, h, launchHome string) context.Context {
	if launchHome == "" {
		launchHome = home.Process()
	}
	if launchResolvedPath(h) == launchResolvedPath(launchHome) {
		if IsMultiplexActive(ctx) {
			return WithLaunchProfileRuntimeScope(ctx, launchHome)
		}
		ctx = home.WithOverride(ctx, "")
		ctx = WithSecretScope(ctx, LaunchSecretScopeCtx(ctx, launchHome), "")
		return tools.SetTerminalScope(ctx, nil)
	}
	ctx = home.WithOverride(ctx, h)
	ctx = WithSecretScope(ctx, BuildProfileSecretScope(h), h)
	return tools.WithProfileTerminalScope(ctx, h, nil)
}

// Main slot provider and model as persisted for the home in ctx
func NormalizeMainModelAssignment(ctx context.Context, provider, model string) (string, string) {
	return normalizeMainModelAssignment(ctx, provider, model)
}

// Main slot pick validated through the model switch against cfg, a refused pick returning ModelAssignmentRejected
func ValidatedMainModelSelection(ctx context.Context, cfg map[string]any, provider, model, baseURL string) (ModelSwitchResult, error) {
	result, err := validatedMainModelSelection(ctx, cfg, provider, model, baseURL, "", nil)
	var refused *HTTPException
	if errors.As(err, &refused) && refused.StatusCode == 400 {
		return ModelSwitchResult{}, &ModelAssignmentRejected{Msg: refused.Detail}
	}
	return result, err
}

// Model block with a validated main slot selection applied for the home in ctx
func ApplyMainModelAssignment(ctx context.Context, modelCfg any, result ModelSwitchResult) (map[string]any, error) {
	return applyMainModelAssignment(ctx, modelCfg, result, "")
}

// Model block with another profile's raw block pinned over it unvalidated, the source's endpoint bound keys left behind
func ApplyInheritedModelAssignment(ctx context.Context, modelCfg any, inherited map[string]any) (map[string]any, error) {
	current := map[string]any{}
	if held, ok := modelCfg.(map[string]any); ok {
		for k, v := range held {
			current[k] = v
		}
		config.SetMappingKeys(current, config.MappingKeys(held))
	}
	lookup := config.EnvRefLookupContext(ctx)
	here := pyStr(config.Expand(pyOr(current["base_url"], ""), lookup))
	there := pyStr(config.Expand(pyOr(inherited["base_url"], ""), lookup))
	result := ModelSwitchResult{Success: true, TargetProvider: pyStr(inherited["provider"]), NewModel: pyStr(inherited["default"]),
		BaseURL: there, APIMode: pyOrStr(inherited["api_mode"])}
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return nil, err
	}
	mask := !fileTooling
	compared := current
	if _, held := current["base_url"]; held {
		compared = make(map[string]any, len(current))
		for k, v := range current {
			compared[k] = v
		}
		config.SetMappingKeys(compared, config.MappingKeys(current))
		compared["base_url"] = here
	}
	assigned := ApplyModelSelection(compared, result, mask)
	bound := map[string]bool{}
	for _, key := range inheritBoundKeys {
		bound[key] = true
		if _, held := current[key]; !held || (here != "" && there != "") {
			continue
		}
		if mask {
			assigned[key] = nil
		} else {
			delete(assigned, key)
		}
	}
	for _, key := range config.MappingKeys(inherited) {
		if !bound[key] {
			assignKey(assigned, key, inherited[key])
		}
	}
	if err := RemoteModelSelectionGuard(ctx, modelCfg, assigned); err != nil {
		return nil, err
	}
	return assigned, nil
}
