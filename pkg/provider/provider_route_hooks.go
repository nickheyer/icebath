package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

var routePureHooks = map[string]bool{"request_headers": true, "max_tokens_param": true, "reasoning_payload": true,
	"replays_function_thought_signature": true}

const routePureMemoMax = 512

var (
	routePureMemoMu sync.Mutex
	routePureMemo   = map[string]any{}
	routeWarnedMu   sync.Mutex
	routeWarned     = map[string]bool{}
	scopeFailuresMu sync.Mutex
	scopeFailures   = map[any]map[string]bool{}
)

// ProviderHookFailureScope is a session scope that keeps its own failed profile hooks until the session resets
type ProviderHookFailureScope interface {
	ProviderHookFailures() map[string]bool
}

var routeHookParams = map[string][]string{
	"client_headers": {"base_url", "api_key", "has_images"}, "request_headers": {"base_url", "model", "user_initiated"},
	"max_tokens_param": {"base_url", "model"}, "reasoning_payload": {"model", "reasoning", "base_url"},
	"replays_function_thought_signature": {"model", "base_url"}, "replay_message_carriers": {"values", "model", "base_url"},
	"refresh_runtime_credentials": {"reason"}, "credential_refresh_notice": {"reason"},
	"is_stale_credential_error": {"status_code", "message"},
}

// True when the profile defines the hook instead of inheriting the no op base answer
func profileOverrides(p *Profile, hook string) bool {
	switch hook {
	case "client_headers":
		return p.ClientHeadersHook != nil
	case "request_headers":
		return p.RequestHeadersHook != nil
	case "max_tokens_param":
		return p.MaxTokensParamHook != nil
	case "reasoning_payload":
		return p.ReasoningPayloadHook != nil
	case "replays_function_thought_signature":
		return p.ReplaysFunctionThoughtSignatureHook != nil
	case "replay_message_carriers":
		return p.ReplayMessageCarriersHook != nil
	case "refresh_runtime_credentials":
		return p.RefreshRuntimeCredentialsHook != nil
	case "credential_refresh_notice":
		return p.CredentialRefreshNoticeHook != nil
	case "is_stale_credential_error":
		return p.IsStaleCredentialErrorHook != nil
	}
	return false
}

// Profiles overriding hook, the one registered for the provider then the one serving the base url host
func routeProfiles(ctx context.Context, hook, provider, baseURL string) []*Profile {
	var named *Profile
	if provider != "" {
		named = getProviderProfile(ctx, provider)
	}
	var ordered []*Profile
	if named != nil && profileOverrides(named, hook) {
		ordered = append(ordered, named)
	}
	if baseURL != "" {
		if hosted := profileForHostCtx(ctx, baseURL, false); hosted != nil && hosted != named && profileOverrides(hosted, hook) {
			ordered = append(ordered, hosted)
		}
	}
	return ordered
}

// Hook arguments by parameter name, positional arguments first then keywords
func bindHookArgs(hook string, args []any, kwargs map[string]any) (map[string]any, error) {
	params := routeHookParams[hook]
	if len(args) > len(params) {
		return nil, fmt.Errorf("%s() takes %d positional arguments but %d were given", hook, len(params), len(args))
	}
	bound := map[string]any{}
	for i, v := range args {
		bound[params[i]] = v
	}
	for k, v := range kwargs {
		known := false
		for _, p := range params {
			known = known || p == k
		}
		if !known {
			return nil, fmt.Errorf("%s() got an unexpected keyword argument '%s'", hook, k)
		}
		if _, dup := bound[k]; dup {
			return nil, fmt.Errorf("%s() got multiple values for argument '%s'", hook, k)
		}
		bound[k] = v
	}
	return bound, nil
}

func hookStr(v any) string {
	s, _ := v.(string)
	return s
}

func hookBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// Calls the profile's typed hook with bound arguments, a panic inside the hook becomes the returned error
func callProfileHook(ctx context.Context, p *Profile, hook string, args []any, kwargs map[string]any) (answer any, err error) {
	b, err := bindHookArgs(hook, args, kwargs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	switch hook {
	case "client_headers":
		return headersAnswer(p.ClientHeadersHook(hookStr(b["base_url"]), hookStr(b["api_key"]), hookBool(b["has_images"]))), nil
	case "request_headers":
		return headersAnswer(p.RequestHeadersHook(hookStr(b["base_url"]), hookStr(b["model"]), hookBool(b["user_initiated"]))), nil
	case "max_tokens_param":
		return p.MaxTokensParamHook(hookStr(b["base_url"]), hookStr(b["model"])), nil
	case "reasoning_payload":
		var r Reasoning
		switch v := b["reasoning"].(type) {
		case Reasoning:
			r = v
		case *Reasoning:
			if v != nil {
				r = *v
			}
		}
		out := p.ReasoningPayloadHook(hookStr(b["model"]), r, hookStr(b["base_url"]))
		if out == nil {
			return nil, nil
		}
		return out, nil
	case "replays_function_thought_signature":
		return p.ReplaysFunctionThoughtSignatureHook(hookStr(b["model"]), hookStr(b["base_url"])), nil
	case "replay_message_carriers":
		values, _ := b["values"].(map[string]any)
		out := p.ReplayMessageCarriersHook(values, hookStr(b["model"]), hookStr(b["base_url"]))
		if out == nil {
			return nil, nil
		}
		return out, nil
	case "refresh_runtime_credentials":
		key, base, ok, err := p.RefreshRuntimeCredentialsHook(ctx, hookStr(b["reason"]))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return map[string]any{"api_key": key, "base_url": base}, nil
	case "credential_refresh_notice":
		return p.CredentialRefreshNoticeHook(hookStr(b["reason"])), nil
	case "is_stale_credential_error":
		status, _ := b["status_code"].(int)
		return p.IsStaleCredentialErrorHook(status, hookStr(b["message"])), nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute '%s'", p.Name, hook)
}

func headersAnswer(m map[string]string) any {
	if m == nil {
		return nil
	}
	return m
}

// Stable text of hook arguments for the pure hook memo, false for an argument with no stable value
func freezeHookArgs(v any) (string, bool) {
	switch x := v.(type) {
	case nil, string, bool, int, int64, float64:
		return fmt.Sprintf("%T:%v", x, x), true
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			s, ok := freezeHookArgs(e)
			if !ok {
				return "", false
			}
			parts[i] = s
		}
		return "[" + strings.Join(parts, ",") + "]", true
	case map[string]any:
		return fmt.Sprintf("%#v", x), true
	case map[string]bool:
		return "", false
	}
	return fmt.Sprintf("%#v", v), true
}

// Deep copy of a dict or list answer so a memoized answer is never shared with a caller
func copyHookAnswer(v any) any {
	switch x := v.(type) {
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, e := range x {
			out[k] = e
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = copyHookAnswer(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyHookAnswer(e)
		}
		return out
	}
	return v
}

// Asks the profile, memoizing documented pure hooks per argument set and registry generation
func askHook(ctx context.Context, p *Profile, hook string, args []any, kwargs map[string]any) (any, error) {
	if !routePureHooks[hook] {
		return callProfileHook(ctx, p, hook, args, kwargs)
	}
	frozenArgs, ok1 := freezeHookArgs(append([]any{}, args...))
	frozenKw, ok2 := freezeHookArgs(kwargs)
	if !ok1 || !ok2 {
		return callProfileHook(ctx, p, hook, args, kwargs)
	}
	key := fmt.Sprintf("%d|%p|%s|%s|%s", RegistryGeneration(), p, hook, frozenArgs, frozenKw)
	routePureMemoMu.Lock()
	answer, hit := routePureMemo[key]
	routePureMemoMu.Unlock()
	if !hit {
		var err error
		if answer, err = callProfileHook(ctx, p, hook, args, kwargs); err != nil {
			return nil, err
		}
		routePureMemoMu.Lock()
		if len(routePureMemo) >= routePureMemoMax {
			clear(routePureMemo)
		}
		routePureMemo[key] = answer
		routePureMemoMu.Unlock()
	}
	return copyHookAnswer(answer), nil
}

// Failed hook set of a scope, a fresh set each call for a scopeless one shot caller
func hookFailures(scope any) map[string]bool {
	if scope == nil {
		return map[string]bool{}
	}
	if s, ok := scope.(ProviderHookFailureScope); ok {
		if f := s.ProviderHookFailures(); f != nil {
			return f
		}
	}
	scopeFailuresMu.Lock()
	defer scopeFailuresMu.Unlock()
	f := scopeFailures[scope]
	if f == nil {
		f = map[string]bool{}
		scopeFailures[scope] = f
	}
	return f
}

func stickyHookKey(p *Profile, hook string) (string, string) {
	name := p.Name
	if name == "" {
		name = "?"
	}
	return fmt.Sprintf("%p|%s|%s", p, name, hook), name + "|" + hook
}

// HookFailed reports whether the profile's hook already failed in scope so asking answers the fallback
func HookFailed(p *Profile, hook string, scope any) bool {
	key, _ := stickyHookKey(p, hook)
	scopeFailuresMu.Lock()
	defer scopeFailuresMu.Unlock()
	return hookFailuresLocked(scope)[key]
}

func hookFailuresLocked(scope any) map[string]bool {
	if scope == nil {
		return map[string]bool{}
	}
	if s, ok := scope.(ProviderHookFailureScope); ok {
		if f := s.ProviderHookFailures(); f != nil {
			return f
		}
	}
	return scopeFailures[scope]
}

// AskProfile is the profile's answer to a per request hook, a failure answers fallback for the rest of the scope's session
func AskProfile(ctx context.Context, p *Profile, hook string, fallback any, scope any, args []any, kwargs map[string]any) any {
	failed := hookFailures(scope)
	sticky, warnKey := stickyHookKey(p, hook)
	scopeFailuresMu.Lock()
	isFailed := failed[sticky]
	scopeFailuresMu.Unlock()
	if isFailed {
		return fallback
	}
	answer, err := askHook(ctx, p, hook, args, kwargs)
	if err == nil {
		return answer
	}
	scopeFailuresMu.Lock()
	failed[sticky] = true
	scopeFailuresMu.Unlock()
	routeWarnedMu.Lock()
	warn := scope != nil || !routeWarned[warnKey]
	routeWarned[warnKey] = true
	routeWarnedMu.Unlock()
	if warn {
		slog.Warn(fmt.Sprintf("provider %s: %s hook failed (%s: %v); using the generic answer for the rest of the session",
			strings.SplitN(warnKey, "|", 2)[0], hook, pyTypeNameOfError(err), err))
	}
	return fallback
}

var errorPathHookWarned sync.Map

// AskOnErrorPath is the profile's answer to an API error path hook asked every time, a failure answers fallback
func AskOnErrorPath(ctx context.Context, p *Profile, hook string, fallback any, kwargs map[string]any) any {
	answer, err := callProfileHook(ctx, p, hook, nil, kwargs)
	if err == nil {
		return answer
	}
	name := p.Name
	if name == "" {
		name = "?"
	}
	if _, seen := errorPathHookWarned.LoadOrStore(name+"|"+hook, true); !seen {
		slog.Warn(fmt.Sprintf("provider %s: %s hook failed; handling the API error without it", name, hook), "error", err)
	}
	return fallback
}

// First truthy answer of hook across the route profiles, nil when no profile claims the endpoint
func routeAnswer(ctx context.Context, hook, provider string, scope any, kwargs map[string]any) any {
	baseURL := ""
	if v, ok := kwargs["base_url"]; ok && v != nil {
		baseURL = fmt.Sprint(v)
	}
	for _, p := range routeProfiles(ctx, hook, provider, baseURL) {
		if answer := AskProfile(ctx, p, hook, nil, scope, nil, kwargs); routeAnswerTruthy(answer) {
			return answer
		}
	}
	return nil
}

func routeAnswerTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case map[string]string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// RouteAnswer is the first truthy answer of hook across the route profiles, nil when no profile claims the endpoint
func RouteAnswer(ctx context.Context, hook, providerName string, scope any, kwargs map[string]any) any {
	return routeAnswer(ctx, hook, providerName, scope, kwargs)
}
