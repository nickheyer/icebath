package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Keys of the per turn construct inputs the owner sends a managed worker
var constructKeys = []string{"fallback_model", "ephemeral_system_prompt", "prefill_messages"}

// Whether a value is None or a list of dicts
func dictList(value any) bool {
	if value == nil {
		return true
	}
	items, isList := value.([]any)
	if !isList {
		return false
	}
	for _, item := range items {
		if _, isDict := item.(*pyjson.Object); !isDict {
			return false
		}
	}
	return true
}

// Child side validated agent construct inputs of a bootstrap frame, empty when the owner sent none
func ConstructKwargs(frame *pyjson.Object) (*pyjson.Object, error) {
	policy, _ := frame.GetOr("policy", nil).(*pyjson.Object)
	text, _ := policy.GetOr("request_json", nil).(string)
	if text == "" {
		text = "{}"
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	request, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(decoded) + "' object has no attribute 'get'"}
	}
	raw := request.GetOr("construct_v1", nil)
	if raw == nil {
		return pyjson.NewObject(), nil
	}
	inputs, isDict := raw.(*pyjson.Object)
	prompt := inputs.GetOr("ephemeral_system_prompt", nil)
	_, promptText := prompt.(string)
	if !isDict || !frameKeysAre(inputs, constructKeys...) || !dictList(inputs.Vals["fallback_model"]) ||
		!dictList(inputs.Vals["prefill_messages"]) || !(prompt == nil || promptText) {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_managed_worker_bootstrap"}
	}
	return inputs.Copy(), nil
}

// Child side api key, base url and api mode overrides of the worker's agent, resolved as the in process turn resolves its route
func WorkerRoute(ctx context.Context, policy *LocalSessionPolicy, construct map[string]any) (map[string]any, error) {
	frozen, model, err := policy.effective(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	if chain, present := construct["fallback_model"]; present {
		copied := make(map[string]any, len(frozen)+2)
		for key, value := range frozen {
			copied[key] = value
		}
		if pyrepr.Truthy(chain) {
			copied["fallback_providers"] = chain
		} else {
			copied["fallback_providers"] = []any{}
		}
		copied["fallback_model"] = nil
		frozen = copied
	}
	var key *string
	if text, isText := construct["api_key"].(string); isText {
		key = &text
	}
	runtime, fallbackEntry, err := RouteRuntime(ctx, policy, frozen, model, key, true)
	if err != nil {
		return nil, err
	}
	if fallbackEntry != nil {
		return map[string]any{}, nil
	}
	var apiMode any = string(runtime.APIMode)
	modelBlock, _ := frozen["model"].(map[string]any)
	if apiMode == "chat_completions" && runtime.Provider != "custom" && !pyrepr.Truthy(modelBlock["api_mode"]) {
		apiMode = nil
	}
	apiKey := construct["api_key"]
	if !pyrepr.Truthy(apiKey) {
		apiKey = runtime.APIKey
	}
	return map[string]any{"api_key": apiKey, "base_url": runtime.BaseURL, "api_mode": apiMode}, nil
}

// Agent checkpoint arguments of a config with the defaults filled in, a legacy boolean section meaning enabled
func CheckpointAgentKwargs(cfg map[string]any) map[string]any {
	var section map[string]any
	switch typed := cfg["checkpoints"].(type) {
	case bool:
		section = map[string]any{"enabled": typed}
	case map[string]any:
		section = typed
	default:
		section = map[string]any{}
	}
	defaults, _ := map[string]any(config.Defaults())["checkpoints"].(map[string]any)
	pick := func(key string) any {
		if value, ok := section[key]; ok {
			return value
		}
		return defaults[key]
	}
	return map[string]any{"checkpoints_enabled": pick("enabled"), "checkpoint_max_snapshots": pick("max_snapshots"),
		"checkpoint_max_total_size_mb": pick("max_total_size_mb"), "checkpoint_max_file_size_mb": pick("max_file_size_mb")}
}

// Section of a config as a mapping, empty when missing or falsy
func routingSection(cfg map[string]any, key string) map[string]any {
	if section, ok := cfg[key].(map[string]any); ok && len(section) > 0 {
		return section
	}
	return map[string]any{}
}

// Provider routing agent arguments of a frozen session config, a static fast tier carrying its request overrides
func RoutingAgentKwargs(cfg map[string]any, model, providerName, baseURL string) map[string]any {
	routing := routingSection(cfg, "provider_routing")
	score := routingSection(cfg, "openrouter")["min_coding_score"]
	rawTier, ok := routingSection(cfg, "agent")["service_tier"]
	if !ok {
		rawTier = ""
	}
	var tier any
	if parsed := provider.ParseServiceTier(pyrepr.Str(rawTier)); parsed != "" {
		tier = parsed
	}
	var overrides any
	if text, isText := tier.(string); isText && provider.StaticTiers[text] {
		overrides = provider.ResolveFastModeOverrides(model, providerName, baseURL, text)
	}
	require, present := routing["require_parameters"]
	if !present {
		require = false
	}
	var minScore any
	if number, isNumber := strictNumber(score); isNumber && number >= 0 && number <= 1 {
		minScore = score
	}
	return map[string]any{"providers_allowed": routing["only"], "providers_ignored": routing["ignore"], "providers_order": routing["order"],
		"provider_sort": routing["sort"], "provider_require_parameters": require, "provider_data_collection": routing["data_collection"],
		"service_tier": tier, "request_overrides": overrides, "openrouter_min_coding_score": minScore}
}
