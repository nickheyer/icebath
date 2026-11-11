package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One fast argument as its service tier, persisted value and label key, an empty label key showing the argument upper cased
type fastSelection struct {
	tier       *string
	savedValue string
	labelKey   string
}

// Fast arguments to their selection
var fastSelections = map[string]fastSelection{
	"fast":      {new("priority"), "fast", "gateway.fast.label_fast"},
	"on":        {new("priority"), "fast", "gateway.fast.label_fast"},
	"normal":    {nil, "normal", "gateway.fast.label_normal"},
	"off":       {nil, "normal", "gateway.fast.label_normal"},
	"auto":      {new("auto"), "auto", ""},
	"cold":      {new("cold"), "cold", ""},
	"ultrafast": {new("ultrafast"), "ultrafast", ""},
}

// Whether the session's next request carries a fast tier for the model and runtime route
func fastRouteSupports(model string, runtime map[string]any, tier string) bool {
	return provider.ResolveFastModeOverrides(model, cleanStr(runtime["provider"]), cleanStr(runtime["base_url"]), tier) != nil
}

// Shows the reasoning effort and display state or applies a typed reasoning selection
func (r *GatewayRunner) handleReasoningCommand(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	rawArgs := strings.TrimSpace(event.GetCommandArgs())
	args, persistGlobal := parseReasoningCommandArgs(rawArgs)
	source := r.normalizeSourceForSessionKey(ctx, event.Source)
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	r.showReasoning = loadShowReasoning(ctx)
	if live := gateway.LocalLive(r, event.Source); live != nil {
		r.showReasoning = live.ShowReasoning != nil && *live.ShowReasoning
	}
	sessionRoute, _ := r.sessionModelOverrides().Get(sessionKey)
	sessionModel := ""
	if sessionRoute != nil && pyrepr.Truthy(sessionRoute["model"]) {
		sessionModel = pyrepr.Str(sessionRoute["model"])
	}
	rc, err := r.resolveSessionReasoningConfig(ctx, event.Source, sessionKey, sessionModel)
	if err != nil {
		return nil, err
	}
	r.reasoningConfig = rc
	platformKey := platformConfigKey(event.Source.Platform)
	if rawArgs != "" {
		reply := r.applyReasoningSelection(ctx, sessionKey, platformKey, args, persistGlobal)
		return &reply, nil
	}
	modelCfg := map[string]any{}
	if m, ok := gateway.LoadGatewayConfigMap(ctx, "")["model"].(map[string]any); ok && m != nil {
		modelCfg = m
	}
	routeProvider := pyrepr.Str(pyrepr.Or(pyrepr.Or(sessionRoute["provider"], modelCfg["provider"]), ""))
	routeModel := sessionModel
	if routeModel == "" {
		routeModel = pyrepr.Str(pyrepr.Or(pyrepr.Or(modelCfg["default"], modelCfg["model"]), ""))
	}
	routeMode := provider.GetCurrentCodexRuntime(map[string]any{"model": modelCfg})
	label := func(effort string) string {
		return provider.EffortDisplayLabel(effort, routeProvider, routeModel, routeMode)
	}
	var level, currentEffort string
	switch {
	case rc == nil:
		level, currentEffort = i18n.T(ctx, "gateway.reasoning.level_default", "", nil), "medium"
	case rc["enabled"] == false:
		level, currentEffort = i18n.T(ctx, "gateway.reasoning.level_disabled", "", nil), "none"
	default:
		currentEffort = "medium"
		if effort, ok := rc["effort"]; ok {
			currentEffort = pyrepr.Str(effort)
		}
		level = label(currentEffort)
	}
	displayState := i18n.T(ctx, "gateway.reasoning.display_off", "", nil)
	if r.showReasoning {
		displayState = i18n.T(ctx, "gateway.reasoning.display_on", "", nil)
	}
	scope := i18n.T(ctx, "gateway.reasoning.scope_global", "", nil)
	if r.sessionReasoningOverrides().Contains(sessionKey) {
		scope = i18n.T(ctx, "gateway.reasoning.scope_session", "", nil)
	}
	choices := []map[string]any{{"value": "none", "label": i18n.T(ctx, "gateway.reasoning.choice_none", "", nil), "is_current": currentEffort == "none"}}
	for _, lv := range config.ValidReasoningEfforts {
		choices = append(choices, map[string]any{"value": lv, "label": label(lv), "is_current": lv == currentEffort})
	}
	for _, v := range []string{"reset", "show", "hide"} {
		choices = append(choices, map[string]any{"value": v, "label": i18n.T(ctx, "gateway.reasoning.choice_"+v, "", nil), "is_current": false})
	}
	onChoice := func(choiceCtx context.Context, _ string, value string) (string, error) {
		return r.applyReasoningSelection(choiceCtx, sessionKey, platformKey, value, false), nil
	}
	values := map[string]any{"level": level, "scope": scope, "display": displayState}
	if r.trySendChoicePicker(ctx, event, sessionKey, i18n.T(ctx, "gateway.reasoning.picker_title", "", values), choices, onChoice) {
		return nil, nil
	}
	reply := i18n.T(ctx, "gateway.reasoning.status", "", values)
	return &reply, nil
}

// Applies a typed or picked fast argument, persisting the tier globally when asked and the write succeeds
func (r *GatewayRunner) applyFastSelection(ctx context.Context, sessionKey, value string, persist bool) string {
	selection, ok := fastSelections[value]
	if !ok {
		return i18n.T(ctx, "gateway.fast.unknown_arg", "", map[string]any{"arg": value})
	}
	label := strings.ToUpper(value)
	if selection.labelKey != "" {
		label = i18n.T(ctx, selection.labelKey, "", nil)
	}
	r.serviceTier = selection.tier
	if persist && r.saveGatewayConfigKey(ctx, "agent.service_tier", selection.savedValue) {
		if gateway.LocalLiveForRoute(r, sessionKey) != nil {
			r.setSessionServiceTierOverride(sessionKey, selection.tier, false)
		} else {
			r.setSessionServiceTierOverride(sessionKey, nil, true)
		}
		r.evictCachedAgent(ctx, sessionKey, false, false)
		return i18n.T(ctx, "gateway.fast.saved", "", map[string]any{"label": label})
	}
	r.setSessionServiceTierOverride(sessionKey, selection.tier, false)
	r.evictCachedAgent(ctx, sessionKey, false, false)
	return i18n.T(ctx, "gateway.fast.session_only", "", map[string]any{"label": label})
}

// Priority processing toggle, session scoped unless global persists the service tier
func (r *GatewayRunner) handleFastCommand(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	args, persistGlobal := parseReasoningCommandArgs(strings.ToLower(strings.TrimSpace(event.GetCommandArgs())))
	source := r.normalizeSourceForSessionKey(ctx, event.Source)
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	tier, err := r.resolveSessionServiceTier(ctx, nil, sessionKey)
	if err != nil {
		return nil, err
	}
	r.serviceTier = tier
	userConfig := gateway.LoadGatewayConfigMap(ctx, "")
	model, runtime, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, userConfig, true)
	if err != nil {
		model, runtime = gateway.ResolveGatewayModel(ctx, userConfig), map[string]any{}
	}
	if !fastRouteSupports(model, runtime, "") {
		reply := i18n.T(ctx, "gateway.fast.not_supported", "", nil)
		return &reply, nil
	}
	if alias, ok := provider.FastArgAliases[args]; ok {
		args = alias
	}
	ultrafast, err := provider.UltrafastOffered(model, cleanStr(runtime["provider"]), cleanStr(runtime["base_url"]), cleanStr(runtime["api_key"]))
	if err != nil {
		return nil, err
	}
	if args == "ultrafast" && !ultrafast {
		reply := i18n.T(ctx, "gateway.fast.ultrafast_not_supported", "", map[string]any{"model": model})
		return &reply, nil
	}
	if args != "" && args != "status" {
		reply := r.applyFastSelection(ctx, sessionKey, args, persistGlobal)
		return &reply, nil
	}
	tierText := ""
	if r.serviceTier != nil {
		tierText = *r.serviceTier
	}
	mode := provider.ServiceTierWord(tierText)
	status := mode
	switch mode {
	case "fast":
		status = i18n.T(ctx, "gateway.fast.status_fast", "", nil)
	case "normal":
		status = i18n.T(ctx, "gateway.fast.status_normal", "", nil)
	}
	options := []string{"fast", "normal", "auto", "cold"}
	if ultrafast {
		options = append(options, "ultrafast")
	}
	choices := make([]map[string]any, len(options))
	for i, v := range options {
		choices[i] = map[string]any{"value": v, "label": i18n.T(ctx, "gateway.fast.choice_"+v, "", nil), "is_current": mode == v}
	}
	onChoice := func(choiceCtx context.Context, _ string, value string) (string, error) {
		return r.applyFastSelection(choiceCtx, sessionKey, value, persistGlobal), nil
	}
	if r.trySendChoicePicker(ctx, event, sessionKey, i18n.T(ctx, "gateway.fast.picker_title", "", map[string]any{"mode": status}), choices, onChoice) {
		return nil, nil
	}
	reply := i18n.T(ctx, "gateway.fast.status", "", map[string]any{"mode": status})
	return &reply, nil
}
