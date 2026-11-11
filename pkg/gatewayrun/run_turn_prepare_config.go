package gatewayrun

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Service tier a turn config resolves with, the runner's per turn tier unless an explicit one is given
type turnTier struct {
	fromRunner bool
	value      *string
}

// Tier of the runner's per turn service tier slot
var tierFromRunner = turnTier{fromRunner: true}

// Fast mode request overrides of a model route under a static tier
var resolveFastModeOverrides = provider.ResolveFastModeOverrides

// Effective model, runtime and identity of one turn's route with its request overrides
type turnRoute struct {
	model            string
	runtime          map[string]any
	signature        []any
	requestOverrides map[string]any
}

// Effective route of one turn, fast mode overrides deep merged over the provider ones under a static tier
func (r *GatewayRunner) resolveTurnAgentConfig(model string, runtimeKwargs map[string]any, tier turnTier) turnRoute {
	runtime := map[string]any{}
	for _, k := range []string{"api_key", "base_url", "provider", "requested_provider", "api_mode", "command", "args",
		"credential_pool", "max_tokens", "capabilities"} {
		runtime[k] = runtimeKwargs[k]
	}
	args := []any{}
	switch v := runtime["args"].(type) {
	case []string:
		for _, a := range v {
			args = append(args, a)
		}
	case []any:
		args = slices.Clone(v)
	}
	runtime["args"] = args
	capabilities := map[string]any{}
	if caps, ok := runtime["capabilities"].(map[string]any); ok {
		maps.Copy(capabilities, caps)
	}
	runtime["capabilities"] = capabilities
	baseOverrides := map[string]any{}
	if overrides, ok := runtimeKwargs["request_overrides"].(map[string]any); ok {
		maps.Copy(baseOverrides, overrides)
	}
	route := turnRoute{
		model:     model,
		runtime:   runtime,
		signature: []any{model, runtime["provider"], runtime["requested_provider"], runtime["base_url"], runtime["api_mode"], runtime["command"], slices.Clone(args)},
	}
	value := tier.value
	if tier.fromRunner {
		value = r.serviceTier
	}
	if value == nil || !provider.StaticTiers[*value] {
		route.requestOverrides = baseOverrides
		return route
	}
	text := func(v any) string { s, _ := v.(string); return s }
	overrides := func() (out map[string]any) {
		defer func() {
			if rec := recover(); rec != nil {
				out = nil
			}
		}()
		return resolveFastModeOverrides(model, text(runtime["provider"]), text(runtime["base_url"]), *value)
	}()
	route.requestOverrides = deepMergeRequestOverrides(baseOverrides, overrides)
	return route
}

// Persists the model and provider a turn actually ran on the session row when fallback switched them
func (r *GatewayRunner) syncSessionModelFromAgent(ctx context.Context, sessionID string, a *agent.AIAgent) {
	db := r.sessionDB(ctx)
	if sessionID == "" || a == nil || db == nil {
		return
	}
	model := a.Model()
	if model == "" {
		return
	}
	runtime := pyjson.NewObject()
	for _, kv := range [][2]any{
		{"provider", provider.SessionProviderIdentity(ctx, a.Provider(), a.RequestedProvider(), a.BaseURL(), model, nil)},
		{"base_url", a.BaseURL()},
		{"api_mode", a.APIMode()},
		{"fallback_active", a.FallbackActivated()},
	} {
		if s, isText := kv[1].(string); isText && s == "" {
			continue
		}
		runtime.Set(kv[0].(string), kv[1])
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to sync gateway session model metadata", "error", rec)
		}
	}()
	sync := func() error {
		row, err := db.DB().GetSession(ctx, sessionID)
		if err != nil || row == nil || row.Len() == 0 {
			return err
		}
		cfg := pyjson.NewObject()
		if raw, _ := row.GetOr("model_config", nil).(string); raw != "" {
			if parsed, err := pyjson.Loads(raw); err == nil {
				if obj, ok := parsed.(*pyjson.Object); ok {
					cfg = obj
				}
			}
		}
		gatewayRuntime := pyjson.NewObject()
		if existing, ok := cfg.GetOr("gateway_runtime", nil).(*pyjson.Object); ok && existing != nil {
			gatewayRuntime = existing.Copy()
		}
		same := row.GetOr("model", nil) == model
		for _, k := range runtime.Keys {
			v, _ := runtime.Get(k)
			if !pyjson.Equal(gatewayRuntime.GetOr(k, nil), v) {
				same = false
			}
		}
		if same {
			return nil
		}
		cfg.Set("gateway_runtime", runtime)
		text, err := pyjson.Dumps(cfg)
		if err != nil {
			return err
		}
		return db.DB().UpdateSessionMeta(ctx, sessionID, text, &model)
	}
	if err := sync(); err != nil {
		runLog().Debug(fmt.Sprintf("Failed to sync gateway session model metadata: %v", err))
	}
}
