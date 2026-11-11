package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Override runtime keys a session /model override carries straight into the agent kwargs
var sessionOverrideRuntimeKeys = []string{
	"provider", "requested_provider", "api_key", "base_url", "api_mode",
	"max_tokens", "credential_pool", "request_overrides", "capabilities",
}

// Python str of a value, empty for None
func noneText(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return pyrepr.Str(value)
}

// One shot notice of a provider switch made while resolving credentials, set again only by a resolution that fell back
func (r *GatewayRunner) setPreAgentFallbackNotice(notice string) {
	r.preAgentFallbackNoticeMu.Lock()
	r.preAgentFallbackNotice = notice
	r.preAgentFallbackNoticeMu.Unlock()
}

// Pending provider switch notice, empty when the last resolution stayed on the primary
func (r *GatewayRunner) pendingPreAgentFallbackNotice() string {
	r.preAgentFallbackNoticeMu.Lock()
	defer r.preAgentFallbackNoticeMu.Unlock()
	return r.preAgentFallbackNotice
}

// Takes the pending provider switch notice, leaving none behind
func (r *GatewayRunner) takePreAgentFallbackNotice() string {
	r.preAgentFallbackNoticeMu.Lock()
	defer r.preAgentFallbackNoticeMu.Unlock()
	notice := r.preAgentFallbackNotice
	r.preAgentFallbackNotice = ""
	return notice
}

// Provider a frozen config's model section names, an attribute error for a model section that is no mapping
func frozenRouteProvider(frozen map[string]any) (string, error) {
	block := frozen["model"]
	if !pyrepr.Truthy(block) {
		return "", nil
	}
	mapping, ok := block.(map[string]any)
	if !ok {
		return "", &gateway.PyError{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(block) + "' object has no attribute 'get'"}
	}
	if !pyrepr.Truthy(mapping["provider"]) {
		return "", nil
	}
	return noneText(mapping["provider"]), nil
}

// Model and agent kwargs of a launch policy session from its frozen route and launch key, the chain walked unless fallback is off
func resolvePolicyAgentRuntime(ctx context.Context, r *GatewayRunner, policy *gateway.LocalSessionPolicy, fallback bool) (string, map[string]any, error) {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return "", nil, err
	}
	frozen, model, err := policy.EffectiveFor(ctx, authority, true)
	if err != nil {
		return "", nil, err
	}
	providerName, err := frozenRouteProvider(frozen)
	if err != nil {
		return "", nil, err
	}
	key, err := gateway.RouteLaunchKey(ctx, authority, policy, frozen)
	if err != nil {
		return "", nil, err
	}
	runtime, fallbackEntry, err := gateway.RouteRuntime(ctx, policy, frozen, model, key, fallback)
	if err != nil {
		return "", nil, err
	}
	primaryModel := ""
	if model != nil {
		primaryModel = *model
	}
	if fallbackEntry != nil {
		fallbackProvider := runtime.Provider
		if fallbackProvider == "" {
			fallbackProvider = noneText(fallbackEntry["provider"])
		}
		if fallbackProvider == "" {
			fallbackProvider = "unknown"
		}
		fallbackModel := noneText(fallbackEntry["model"])
		noticeModel := fallbackModel
		if noticeModel == "" {
			noticeModel = "default"
		}
		r.setPreAgentFallbackNotice(provider.PreAgentFallbackNotice(providerName, primaryModel, fallbackProvider, noticeModel))
		return fallbackModel, runtimeAgentKwargs(runtime), nil
	}
	return primaryModel, runtimeAgentKwargs(runtime), nil
}

// Model and runtime of a session, its /model override first, then the channel override, then the profile config
func (r *GatewayRunner) resolveSessionAgentRuntime(ctx context.Context, source *gateway.SessionSource, sessionKey string, userConfig map[string]any, fallback bool) (string, map[string]any, error) {
	r.setPreAgentFallbackNotice("")
	if source != nil {
		policy, err := gateway.SessionPolicyForSource(ctx, r, source)
		if err != nil {
			return "", nil, err
		}
		if policy != nil {
			return resolvePolicyAgentRuntime(ctx, r, policy, fallback)
		}
	}
	skey := r.resolveSessionKeyOrNone(ctx, source, sessionKey)
	model := gateway.ResolveGatewayModel(ctx, userConfig)
	if skey != "" {
		r.rehydrateSessionModelOverride(ctx, skey)
	}
	var override map[string]any
	if skey != "" {
		if state := r.peekSessionState(skey); state != nil {
			override = state.conversation.modelOverride
		}
	}
	if len(override) > 0 {
		overrideModel := model
		if value, present := override["model"]; present {
			overrideModel = noneText(value)
		}
		overrideRuntime := make(map[string]any, len(sessionOverrideRuntimeKeys))
		for _, key := range sessionOverrideRuntimeKeys {
			overrideRuntime[key] = override[key]
		}
		capabilities := map[string]any{}
		if held, ok := overrideRuntime["capabilities"].(map[string]any); ok {
			maps.Copy(capabilities, held)
		}
		overrideRuntime["capabilities"] = capabilities
		if pyrepr.Truthy(overrideRuntime["api_key"]) {
			if overrideRuntime["credential_pool"] == nil {
				overrideRuntime["credential_pool"] = credentialPoolForProvider(ctx, noneText(override["provider"]))
			}
			runLog().Debug(fmt.Sprintf("Session model override (fast): session=%s config_model=%s -> override_model=%s provider=%s",
				skey, model, overrideModel, pyrepr.Str(overrideRuntime["provider"])))
			return overrideModel, overrideRuntime, nil
		}
		runLog().Debug(fmt.Sprintf("Session model override (no api_key, fallback): session=%s config_model=%s override_model=%s",
			skey, model, overrideModel))
	} else if runLog().Enabled(ctx, slog.LevelDebug) {
		var keys []any
		for key, state := range r.sessionsMap() {
			if state.conversation.modelOverride != nil {
				keys = append(keys, key)
			}
		}
		listing := "[]"
		if len(keys) > 5 {
			keys = keys[:5]
		}
		if len(keys) > 0 {
			listing = pyrepr.Repr(keys)
		}
		runLog().Debug(fmt.Sprintf("No session model override: session=%s config_model=%s override_keys=%s", skey, model, listing))
	}
	var runtimeKwargs, unavailableOverride map[string]any
	if len(override) > 0 && pyrepr.Truthy(override["provider"]) {
		kwargs, err := resolveRuntimeAgentKwargsForProvider(ctx, noneText(override["provider"]), noneText(override["model"]))
		if err != nil {
			runLog().Warn(fmt.Sprintf("Session /model override provider %s unavailable: %s", noneText(override["provider"]), err), "error", err)
			unavailableOverride, override = override, nil
		} else {
			runtimeKwargs = kwargs
		}
	}
	if runtimeKwargs == nil {
		kwargs, err := resolveRuntimeAgentKwargs(ctx)
		if err != nil {
			return "", nil, err
		}
		runtimeKwargs = kwargs
	}
	notice := noneText(runtimeKwargs["_fallback_notice"])
	delete(runtimeKwargs, "_fallback_notice")
	r.setPreAgentFallbackNotice(notice)
	runtimeModel := runtimeKwargs["model"]
	delete(runtimeKwargs, "model")
	if pyrepr.Truthy(runtimeModel) {
		runLog().Info(fmt.Sprintf("Runtime provider supplied explicit model override: %s -> %s", model, noneText(runtimeModel)))
		model = noneText(runtimeModel)
	}
	if unavailableOverride != nil && r.pendingPreAgentFallbackNotice() == "" {
		r.setPreAgentFallbackNotice(provider.PreAgentFallbackNotice(noneText(unavailableOverride["provider"]),
			noneText(unavailableOverride["model"]), noneText(runtimeKwargs["provider"]), model))
	}
	if r.config != nil && source != nil {
		var threadID, parentID *string
		if source.ThreadID != nil && *source.ThreadID != "" {
			threadID = source.ThreadID
		}
		if source.ParentChatID != nil && *source.ParentChatID != "" {
			parentID = source.ParentChatID
		}
		if ch := getChannelOverride(r.config, source.Platform, source.ChatID, threadID, parentID); ch != nil {
			if pyrepr.Truthy(ch.Model) {
				model = noneText(ch.Model)
			}
			if pyrepr.Truthy(ch.Provider) {
				kwargs, err := resolveRuntimeAgentKwargsForProvider(ctx, noneText(ch.Provider), model)
				if err != nil {
					return "", nil, err
				}
				runtimeKwargs = kwargs
				channelRuntimeModel := runtimeKwargs["model"]
				delete(runtimeKwargs, "model")
				if pyrepr.Truthy(channelRuntimeModel) && !pyrepr.Truthy(ch.Model) {
					model = noneText(channelRuntimeModel)
				}
			}
		}
	}
	if len(override) > 0 && skey != "" {
		model, runtimeKwargs = r.applySessionModelOverride(ctx, skey, model, runtimeKwargs)
	}
	if model == "" && pyrepr.Truthy(runtimeKwargs["provider"]) {
		providerName := noneText(runtimeKwargs["provider"])
		model = provider.GetDefaultModelForProvider(ctx, providerName)
		if model != "" {
			runLog().Info(fmt.Sprintf("No model configured — defaulting to %s for provider %s", model, providerName))
		}
	}
	return recoverOrRememberModel(r, skey, model), runtimeKwargs, nil
}
