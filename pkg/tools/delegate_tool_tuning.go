package tools

import (
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Provider side speed tier gates pkg/agent installs at init from pkg/provider
var DelegateServiceTierHooks struct {
	ParseExactServiceTier    func(raw string) (string, error)
	IsStaticTier             func(tier string) bool
	ResolveFastModeOverrides func(model, providerName, baseURL, tier string) map[string]any
	UltrafastOffered         func(model, providerName, baseURL, accessToken string) (bool, error)
}

// Route facts of a delegating parent or built child the speed tier reads and pins
type DelegateRouteAgent interface {
	Provider() string
	Model() string
	BaseURL() string
	APIKey() string
	RequestOverrides() map[string]any
	SetRequestOverrides(map[string]any)
}

var serviceTierKeys = map[string]bool{"service_tier": true, "speed": true}

// Deep copy of overrides without a speed tier key at the top level or inside extra_body
func WithoutServiceTier(overrides any) map[string]any {
	cleaned := map[string]any{}
	switch o := pycopy.DeepCopy(overrides).(type) {
	case map[string]any:
		cleaned = o
	case *pyjson.Object:
		cleaned = o.Vals
	}
	maps.DeleteFunc(cleaned, func(k string, _ any) bool { return serviceTierKeys[k] })
	var extra map[string]any
	switch e := cleaned["extra_body"].(type) {
	case map[string]any:
		extra = e
	case *pyjson.Object:
		extra = e.Vals
	default:
		return cleaned
	}
	maps.DeleteFunc(extra, func(k string, _ any) bool { return serviceTierKeys[k] })
	if len(extra) > 0 {
		cleaned["extra_body"] = extra
	} else {
		delete(cleaned, "extra_body")
	}
	return cleaned
}

// Request overrides of tier on this route, nil when the route or its Codex account does not offer it
func offeredTierOverrides(tier, model, providerName, baseURL, accessToken string) (map[string]any, error) {
	fast := DelegateServiceTierHooks.ResolveFastModeOverrides(model, providerName, baseURL, tier)
	if len(fast) > 0 && tier == "ultrafast" {
		offered, err := DelegateServiceTierHooks.UltrafastOffered(model, providerName, baseURL, accessToken)
		if err != nil {
			return nil, err
		}
		if !offered {
			return nil, nil
		}
	}
	return fast, nil
}

func credString(creds map[string]any, key string) string {
	s, _ := creds[key].(string)
	return s
}

// Creds keys pinning a per task speed tier on the child route, an error for a tier the route cannot serve
func TaskServiceTier(word string, creds map[string]any, parent DelegateRouteAgent) (map[string]any, error) {
	tier, err := DelegateServiceTierHooks.ParseExactServiceTier(word)
	if err != nil || (tier != "" && !DelegateServiceTierHooks.IsStaticTier(tier)) {
		return nil, &DelegationValueError{Message: fmt.Sprintf("Unknown service_tier '%s'. Use normal, fast, or ultrafast.", word)}
	}
	attr := func(get func(DelegateRouteAgent) string) string {
		if parent == nil {
			return ""
		}
		return get(parent)
	}
	pinned := credString(creds, "provider") != "" || credString(creds, "base_url") != ""
	providerName := credString(creds, "provider")
	if providerName == "" {
		providerName = attr(DelegateRouteAgent.Provider)
	}
	model := credString(creds, "model")
	if model == "" {
		model = attr(DelegateRouteAgent.Model)
	}
	baseURL, apiKey := attr(DelegateRouteAgent.BaseURL), attr(DelegateRouteAgent.APIKey)
	if pinned {
		baseURL, apiKey = credString(creds, "base_url"), credString(creds, "api_key")
	}
	base := creds["request_overrides"]
	if base == nil && credString(creds, "provider") == "" && parent != nil {
		if ro := parent.RequestOverrides(); ro != nil {
			base = ro
		}
	}
	fast := map[string]any{}
	if tier != "" {
		offered, err := offeredTierOverrides(tier, model, providerName, baseURL, apiKey)
		if err != nil {
			return nil, err
		}
		if len(offered) == 0 {
			return nil, &DelegationValueError{Message: fmt.Sprintf("service_tier '%s' is not available for model '%s' on provider '%s'. Omit service_tier or choose a model/provider that offers it.", word, shownRoute(model, parent), shownRoute(providerName, parent))}
		}
		fast = offered
	}
	overrides := WithoutServiceTier(base)
	maps.Copy(overrides, fast)
	var serviceTier any
	if tier != "" {
		serviceTier = tier
	}
	return map[string]any{"request_overrides": overrides, "service_tier": serviceTier, "task_tier_overrides": fast}, nil
}

// Route word as the Python f-string shows it, None when no parent supplied one
func shownRoute(s string, parent DelegateRouteAgent) string {
	if s == "" && parent == nil {
		return "None"
	}
	return s
}

// Reapplies a task speed tier to the built child after construction merged provider extra_body back
func PinChildServiceTier(child DelegateRouteAgent, creds map[string]any) {
	fast, present := creds["task_tier_overrides"]
	if !present || fast == nil {
		return
	}
	overrides := WithoutServiceTier(child.RequestOverrides())
	if m, ok := fast.(map[string]any); ok {
		maps.Copy(overrides, m)
	}
	child.SetRequestOverrides(overrides)
}
