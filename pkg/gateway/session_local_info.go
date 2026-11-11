package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Route and effort a built agent reports as its session's own, mid voice turn included
type sessionRuntimeFacts interface {
	Provider() string
	BaseURL() string
	APIMode() string
	ReasoningConfig() map[string]any
}

// Built agent of a session as its session info reads it
type sessionInfoAgent interface {
	Model() string
	APIMode() string
	APIKey() string
	ServiceTier() string
	ToolDefinitions() []map[string]any
	SessionRuntime() sessionRuntimeFacts
}

// Peeked session state of a route as session info reads it
type peekedSessionState interface {
	ReasoningOverride() map[string]any
}

// Runner that reads the session state of a route without building it
type sessionStatePeekRunner interface {
	PeekSessionState(route string) peekedSessionState
}

// Canonical profile id of a served home, default for the root and for any custom home
func servedProfileName(profileHome string) string {
	if name, ok := ProfileNameForHome(profileHome); ok && name != "" {
		return name
	}
	return "default"
}

// Text of an optional value, empty for None
func anyText(v any) string {
	if v == nil {
		return ""
	}
	if text, ok := v.(string); ok {
		return text
	}
	return pyrepr.Str(v)
}

// Model a session with no built agent will run, the welcome model when the free tier route would pin it
func lazyModel(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) (any, error) {
	if policy == nil {
		return nil, nil
	}
	current, err := policy.currentModel(ctx, a)
	if err != nil {
		return nil, err
	}
	model := optValue(current)
	request, err := loadsObject(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	if pinned, _ := request.Get("model"); pyTruthy(pinned) {
		return model, nil
	}
	if route := strings.ToLower(strings.TrimSpace(anyText(policy.Provider()))); route != "" && route != "auto" {
		return model, nil
	}
	if provider.FreeTierRoute(ownerScope(ctx, a, false)) {
		return provider.GuestModel, nil
	}
	return model, nil
}

// Effort and tier fields of a session from its built agent, or what the policy's next turn resolves
func reasoningFields(ctx context.Context, agent sessionInfoAgent, policy *LocalSessionPolicy, model any, a *SessionAuthority, override map[string]any) (*pyjson.Object, error) {
	if agent == nil && policy == nil {
		return pyjson.NewObject(), nil
	}
	if agent != nil {
		view := agent.SessionRuntime()
		return effortFields(view.ReasoningConfig(), agent.ServiceTier(), model, view.Provider(), view.BaseURL(), view.APIMode()), nil
	}
	effective, nextModel, err := policy.effective(ctx, a, false)
	if err != nil {
		return nil, err
	}
	route, _ := effective["model"].(map[string]any)
	resolved := override
	if resolved == nil {
		next := ""
		if nextModel != nil {
			next = *nextModel
		}
		resolved = config.ResolveReasoningConfig(effective, next)
	}
	section, _ := effective["agent"].(map[string]any)
	tier := provider.ParseServiceTier(anyText(section["service_tier"]))
	return effortFields(resolved, tier, model, route["provider"], route["base_url"], nil), nil
}

// Session info reasoning effort, wire effort, service tier and fast flag of one resolved route
func effortFields(reasoning any, tier string, model, providerName, baseURL, apiMode any) *pyjson.Object {
	effort := ""
	if cfg, ok := reasoning.(map[string]any); ok && cfg != nil {
		if cfg["enabled"] == any(false) {
			effort = "none"
		} else if pyTruthy(cfg["effort"]) {
			effort = anyText(cfg["effort"])
		}
	}
	wire := ""
	if effort != "" && effort != "none" {
		wire = provider.ClampEffort(effort, provider.RouteSupportedEfforts(anyText(providerName), anyText(model), anyText(apiMode)))
	}
	fast := false
	if provider.StaticTiers[tier] {
		name, base := "", ""
		if pyTruthy(providerName) {
			name = anyText(providerName)
		}
		if pyTruthy(baseURL) {
			base = anyText(baseURL)
		}
		fast = provider.ResolveFastModeOverrides(anyText(model), name, base, tier) != nil
	}
	return pyjson.ObjectOf("reasoning_effort", effort, "reasoning_effort_wire", wire, "service_tier", tier, "fast", fast)
}

// Session info of a live session, its source, model, route, effort and classic CLI fields
func localSessionInfo(ctx context.Context, a *SessionAuthority, ref SessionRef) (*pyjson.Object, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{key: ref.SessionID}
	}
	agent, err := authorityAgent(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	var source any
	switch {
	case live.Source == nil:
		row, err := a.DB.GetSession(ctx, ref.SessionID)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
		}
		label, has := row.Get("source")
		if !has {
			return nil, &pyKeyError{key: "source"}
		}
		source = label
	case policy != nil:
		source = policy.Source
	default:
		source = string(live.Source.Platform)
	}
	var model any
	if agent != nil {
		if policy != nil {
			if _, err := policy.currentModel(ctx, a); err != nil {
				return nil, err
			}
		}
		model = agent.Model()
	} else if model, err = lazyModel(ctx, a, policy); err != nil {
		return nil, err
	}
	info := pyjson.ObjectOf("source", source, "model", model, "lazy", agent == nil, "profile_id", a.ProfileID,
		"desktop_protocol", CanonicalGatewayProtocol, "profile_name", servedProfileName(a.ProfileID))
	if policy != nil {
		info.Set("cwd", policy.Cwd)
		if policy.Source == "acp" {
			info.Set("edit_approval_policy", policy.EditApprovalPolicy)
		}
	}
	var override map[string]any
	if peeker, can := a.Runner.(sessionStatePeekRunner); can {
		if peeked := peeker.PeekSessionState(live.Route); peeked != nil {
			override = peeked.ReasoningOverride()
		}
	}
	var fields *pyjson.Object
	if agent == nil && policy == nil && live.Source != nil && live.Source.Platform != PlatformLocal {
		fields, err = messagingRouteInfo(ctx, a.Runner, live)
	} else {
		fields, err = reasoningFields(ctx, agent, policy, model, a, override)
	}
	if err != nil {
		return nil, err
	}
	mergeObject(info, fields)
	if policy != nil {
		request, err := pyjson.Loads(policy.RequestJSON)
		if err != nil {
			return nil, err
		}
		info.Set("launch_request", request)
		var notices []string
		if warning := projectPluginsWarning(policy); warning != nil && *warning != "" {
			notices = append(notices, *warning)
		}
		if policy.ClientEnvRemote {
			notices = append(notices, state.ClientEnvNotLocalWarning)
		}
		if warning := strings.Join(notices, " "); warning != "" {
			info.Set("config_warning", warning)
		}
	}
	mergeObject(info, classicCLIFields(ctx, a, ref, live, agent, policy))
	return info, nil
}

// Copies every key of src onto dst in order
func mergeObject(dst, src *pyjson.Object) {
	for _, key := range src.Keys {
		value, _ := src.Get(key)
		dst.Set(key, value)
	}
}

// Stored title and times, yolo, API mode, tool count and masked credential hint the classic CLI read off itself
func classicCLIFields(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, agent sessionInfoAgent, policy *LocalSessionPolicy) *pyjson.Object {
	storedID := ref.SessionID
	if target, err := receiptEntryTarget(ctx, a.DB, ref.SessionID); err == nil && target != "" {
		storedID = target
	}
	row := pyjson.NewObject()
	if stored, err := a.DB.GetSession(ctx, storedID); err == nil && stored != nil {
		row = stored
	}
	yolo := policy != nil && policy.Yolo()
	yolo = yolo || tools.IsSessionYoloEnabled(live.Route)
	var apiMode any
	if agent != nil {
		apiMode = agent.APIMode()
	} else if policy != nil {
		if effective, _, err := policy.effective(ctx, a, false); err == nil {
			if route, isMap := effective["model"].(map[string]any); isMap {
				apiMode = route["api_mode"]
			}
		}
	}
	if !pyTruthy(apiMode) {
		apiMode = nil
	}
	var hint, toolCount any
	if agent != nil {
		if key := agent.APIKey(); key != "" {
			hint = memory.MaskSecret(key, 4, 4, 12, "***", "")
		}
		if defs := agent.ToolDefinitions(); defs != nil {
			toolCount = len(defs)
		}
	}
	title, _ := row.Get("title")
	started, _ := row.Get("started_at")
	lastActive, _ := row.Get("last_activity_at")
	if !pyTruthy(lastActive) {
		lastActive = started
	}
	return pyjson.ObjectOf("title", title, "started_at", started, "last_active_at", lastActive, "yolo", yolo,
		"api_mode", apiMode, "tool_count", toolCount, "credential_hint", hint)
}

// Built agent of a live session under its running key, nil when none is resident or cached
func authorityAgent(ctx context.Context, a *SessionAuthority, ref SessionRef) (sessionInfoAgent, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{key: ref.SessionID}
	}
	key := runningKey(a.Runner, live.Route, ref.SessionID)
	var found any
	if resident, has := a.Runner.(residentAgentRunner); has {
		found = resident.ResidentAgentFor(key)
	} else if cached, has := a.Runner.(cachedAgentRunner); has {
		found = cached.CachedAgentFor(key)
	} else {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_cached_agent_for'", a.Runner)}
	}
	if found == nil {
		return nil, nil
	}
	agent, ok := found.(sessionInfoAgent)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'model'", found)}
	}
	return agent, nil
}
