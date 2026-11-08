package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Credential resolver of a delegation config block, swapped by tests the way the Python facade is patched
var resolveDelegationCredentialsFn = resolveDelegationCredentials

func routeError(format string, args ...any) error {
	return &tools.DelegationRouteError{Message: fmt.Sprintf(format, args...)}
}

// Whether an error is the ValueError family a task route refusal raises
func isDelegationValueError(err error) bool {
	var routeErr *tools.DelegationRouteError
	var valueErr *tools.DelegationValueError
	var refusal *delegateValueError
	return errors.As(err, &routeErr) || errors.As(err, &valueErr) || errors.As(err, &refusal)
}

// Home directory of an existing profile named by a route, a route error otherwise
func delegationProfileHome(name any) (string, error) {
	s, ok := name.(string)
	if !ok || pystr.Strip(s) == "" || !profiles.ProfileExists(s) {
		return "", routeError("Profile %s does not exist (see `icebath profile list`).", pyrepr.Repr(name))
	}
	return profiles.GetProfileDir(s)
}

// Context running as a profile home with its secrets and terminal policy, unchanged for the current home
func delegatedProfileScope(ctx context.Context, h string) context.Context {
	if h == "" || home.Key(h) == home.Key(home.Dir(ctx)) {
		return ctx
	}
	routed := home.Key(h) != home.Key(provider.RoutingProcessHome())
	provider.HydrateProfileSecretSources(h)
	ctx = home.WithOverride(ctx, h)
	ctx = provider.WithSecretScope(ctx, provider.OwnerSecretScope(ctx, h), h)
	if routed {
		ctx = provider.WithMultiplexContext(ctx, true)
	}
	return tools.WithProfileTerminalScope(ctx, h, nil)
}

// Profile scope a built child's own work runs in, unchanged for a child without a profile route
func childProfileScope(ctx context.Context, child *AIAgent) context.Context {
	return delegatedProfileScope(ctx, child.delegateProfileHome)
}

// Runtime provider request of a profile's primary model with its startup alias decoded
type profileModelRequest struct {
	targetModel, requested, explicitBaseURL, explicitAPIKey string
}

// Primary model request of a profile config, resolved the way CLI startup reads model.default
func profileModelRequestOf(ctx context.Context, cfg map[string]any) (profileModelRequest, error) {
	var model any
	cfgProvider := ""
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		model = modelCfg["default"]
		if !pyTruthy(model) {
			model = modelCfg["model"]
		}
		cfgProvider = pystr.Strip(liveText(modelCfg["provider"]))
	} else if s, ok := cfg["model"].(string); ok {
		model = s
	}
	text, isStr := model.(string)
	request := profileModelRequest{}
	if isStr {
		request.targetModel = text
	}
	if !isStr || pystr.Strip(text) == "" {
		return request, nil
	}
	userProviders, _ := cfg["providers"].(map[string]any)
	customProviders, _ := cfg["custom_providers"].([]any)
	startup, err := provider.ResolveStartupModelRoute(ctx, text, provider.StartupRouteOptions{
		CurrentProvider: pystr.Lower(cfgProvider), UserProviders: userProviders, CustomProviders: customProviders,
		AliasConfig: maps.Clone(cfg),
	})
	if err != nil {
		return request, err
	}
	if startup != nil {
		request = profileModelRequest{
			targetModel: startup.Model, requested: startup.Provider, explicitBaseURL: startup.BaseURL, explicitAPIKey: startup.APIKey,
		}
	}
	return request, nil
}

// Refuses a profile route from a managed worker whose config is frozen at creation
func refuseUnderWorkerSnapshot(ctx context.Context, h string) error {
	if config.WorkerConfigSnapshot() != nil && home.Key(h) != home.Key(home.Dir(ctx)) {
		return routeError("Profile routes are unavailable in a managed worker session (its config is frozen at creation): "+
			"cannot run as the profile at %s. Delegate without a profile.", h)
	}
	return nil
}

// The profile's own primary route resolved inside its scope with its own keys, fallback chain and reasoning
func profileRoute(ctx context.Context, h string) (*tools.TaskRoute, error) {
	if err := refuseUnderWorkerSnapshot(ctx, h); err != nil {
		return nil, err
	}
	scoped := delegatedProfileScope(ctx, h)
	tree, err := config.LoadReadonly(scoped, home.Dir(scoped), nil)
	if err != nil {
		return nil, err
	}
	cfg := map[string]any(tree)
	if cfg == nil {
		cfg = map[string]any{}
	}
	request, err := profileModelRequestOf(scoped, cfg)
	if err != nil {
		return nil, err
	}
	rt, err := provider.ResolveRuntimeProvider(scoped, home.Dir(scoped), request.requested, request.targetModel,
		request.explicitBaseURL, request.explicitAPIKey)
	if err != nil {
		return nil, routeError("Profile at %s has no usable model provider: %s", h, err)
	}
	model := request.targetModel
	if model == "" {
		model = rt.Model
	}
	fallback := cfg["fallback_providers"]
	requested := pystr.Strip(rt.RequestedProvider)
	providerName := rt.Provider
	if rt.Provider == "custom" && !slices.Contains([]string{"", "auto"}, pystr.Lower(requested)) {
		providerName = requested
	}
	if err := tools.RequirePinnedCommand(rt.Command, fmt.Sprintf("Profile at %s is pinned to the '%s' command, which was not "+
		"found on PATH. Install it or route this task elsewhere.", h, rt.Command)); err != nil {
		return nil, err
	}
	apiKey := rt.APIKey
	if apiKey == "" {
		apiKey = "no-key-required"
	}
	overrides := maps.Clone(rt.RequestOverrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	args := make([]any, 0, len(rt.Args))
	for _, a := range rt.Args {
		args = append(args, a)
	}
	creds := tools.CredentialBundle(noneIfEmpty(model), noneIfEmpty(providerName), noneIfEmpty(rt.BaseURL), apiKey,
		noneIfEmpty(string(rt.APIMode)), overrides, map[string]any{"command": noneIfEmpty(rt.Command), "args": args})
	chain, ok := fallback.([]any)
	if !ok {
		chain = []any{}
	}
	return &tools.TaskRoute{
		Creds: creds, RoutingCfg: map[string]any{"fallback_providers": chain},
		ReasoningEffort: tools.ResolvedReasoning{Config: config.ResolveReasoningConfig(cfg, model)}, ProfileHome: h,
		Backend: tools.DefaultDelegationBackend,
	}, nil
}

// Route taking delegation.fallback_providers unless it is a profile route or carries its own chain
func withInheritedFallback(route, cfg map[string]any) map[string]any {
	if pyTruthy(route["profile"]) || route["fallback_providers"] != nil {
		return route
	}
	inherited := cfg["fallback_providers"]
	if inherited == nil {
		return route
	}
	out := maps.Clone(route)
	out["fallback_providers"] = inherited
	return out
}

// Task route of one cleaned provider route, a profile route or the delegation credential resolver
func resolveRoute(ctx context.Context, route map[string]any, parent *AIAgent, backend string) (*tools.TaskRoute, error) {
	if pyTruthy(route["profile"]) {
		h, err := delegationProfileHome(route["profile"])
		if err != nil {
			return nil, err
		}
		r, err := profileRoute(ctx, h)
		if err != nil {
			return nil, err
		}
		out := *r
		out.Backend = backend
		return &out, nil
	}
	creds, err := resolveDelegationCredentialsFn(ctx, route, parent)
	if err != nil {
		return nil, err
	}
	if tier := route["service_tier"]; tier != nil && tier != "" {
		extra, err := tools.TaskServiceTier(pyrepr.Str(tier), creds, parent)
		if err != nil {
			return nil, err
		}
		merged := maps.Clone(creds)
		maps.Copy(merged, extra)
		creds = merged
	}
	return &tools.TaskRoute{Creds: creds, RoutingCfg: route, ReasoningEffort: route["reasoning_effort"], Backend: backend}, nil
}

// Task route of a cleaned route resolved once per identical route, its display label applied per task
func routeToTaskRoute(ctx context.Context, route map[string]any, parent *AIAgent, cache map[string]*tools.TaskRoute,
	cfg map[string]any, backend string) (*tools.TaskRoute, error) {
	label, _ := route["label"].(string)
	route = withInheritedFallback(route, cfg)
	keyed := maps.Clone(route)
	delete(keyed, "label")
	key, err := pyjson.Dumps(keyed, pyjson.SortKeys(true), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return nil, err
	}
	if _, ok := cache[key]; !ok {
		r, err := resolveRoute(ctx, route, parent, backend)
		if err != nil {
			return nil, err
		}
		cache[key] = r
	}
	out := *cache[key]
	out.Label = label
	return &out, nil
}

// Refuses a backend route whose reasoning effort is not a known level
func checkBackendEffort(index int, route, cfg map[string]any) error {
	effort := route["reasoning_effort"]
	if pyTruthy(route["profile"]) || effort == nil || effort == "" || pyrepr.Equal(effort, cfg["reasoning_effort"]) {
		return nil
	}
	if config.ParseReasoningEffort(effort) == nil {
		return routeError("Task %d: unknown reasoning_effort %s on this route. Use none or one of: %s.", index,
			pyrepr.Repr(effort), strings.Join(config.ValidReasoningEfforts, ", "))
	}
	return nil
}

// Why a task carrying parameters the session backend does not declare was refused
func undeclaredParamsError(ctx context.Context, p tools.DelegationProvider, index int, names []string, cfg map[string]any) string {
	active := p.Name()
	quotedNames := make([]string, len(names))
	for i, n := range names {
		quotedNames[i] = "'" + n + "'"
	}
	listed := strings.Join(quotedNames, ", ")
	var owners []string
	for _, other := range tools.DelegationRegistryHooks.ListProviders(ctx) {
		if other.Name() == active || slices.Contains(owners, other.Name()) {
			continue
		}
		declared := tools.DeclaredDelegationParameters(other, cfg)
		if slices.ContainsFunc(names, func(n string) bool { _, ok := declared[n]; return ok }) {
			owners = append(owners, other.Name())
		}
	}
	slices.Sort(owners)
	head := fmt.Sprintf("Task %d: the delegation backend is '%s', which does not take %s", index, active, listed)
	fix := fmt.Sprintf("run `/delegation %s --now` to rebuild this session's delegate_task for '%s', "+
		"or start a new session with /new", active, active)
	if len(owners) > 0 {
		quoted := make([]string, len(owners))
		for i, o := range owners {
			quoted[i] = "'" + o + "'"
		}
		return fmt.Sprintf("%s (declared by %s), so this task was not started. This session's delegate_task was "+
			"built for another backend: %s.", head, strings.Join(quoted, ", "), fix)
	}
	return fmt.Sprintf("%s, and no installed delegation backend does, so this task was not started. Resend it without "+
		"%s; if this session's delegate_task tool offers %s, %s.", head, listed, listed, fix)
}

// Provider answer for one task, a recovered provider panic standing for its raised exception
func callRouteTask(p tools.DelegationProvider, payload, cfg map[string]any) (route map[string]any, panicked any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = rec
		}
	}()
	route, err = p.RouteTask(payload, cfg)
	return route, nil, err
}

// Validated parameters of one task routed through the provider into its cleaned route keys
func routeOneTask(ctx context.Context, p tools.DelegationProvider, declared map[string]map[string]any, index int,
	task, cfg map[string]any) (map[string]any, error) {
	var undeclared []string
	for name, value := range task {
		if _, ok := declared[name]; value != nil && !ok && !tools.ReservedTaskParameters[name] {
			undeclared = append(undeclared, name)
		}
	}
	slices.Sort(undeclared)
	if len(undeclared) > 0 {
		return nil, &tools.DelegationRouteError{Message: undeclaredParamsError(ctx, p, index, undeclared, cfg)}
	}
	names := slices.Sorted(maps.Keys(declared))
	params := map[string]any{}
	for _, name := range names {
		if v := task[name]; v != nil {
			params[name] = v
		}
	}
	for _, name := range names {
		if v, ok := params[name]; ok {
			if msg := tools.DelegationParamError(index, name, v, declared[name]); msg != "" {
				return nil, &tools.DelegationRouteError{Message: msg}
			}
		}
	}
	payload := map[string]any{"goal": task["goal"], "context": task["context"], "index": index}
	maps.Copy(payload, params)
	route, panicked, err := callRouteTask(p, payload, cfg)
	if panicked == nil && err == nil {
		var answer any
		if route != nil {
			answer = route
		}
		route, err = tools.CleanDelegationRoute(p.Name(), answer)
	}
	var routeErr *tools.DelegationRouteError
	switch {
	case panicked == nil && err == nil:
		return route, nil
	case panicked == nil && errors.As(err, &routeErr):
		return nil, routeError("Task %d: %s", index, err)
	}
	reason := fmt.Sprint(panicked)
	if panicked == nil {
		reason = err.Error()
	}
	delegateLog().Warn(fmt.Sprintf("delegation provider %s.route_task() raised", p.Name()), "exc_info", reason)
	return nil, routeError("Task %d: delegation backend '%s' failed: %s", index, p.Name(), reason)
}

// One task route per task from the session's pinned or configured backend, a refusal text when it cannot run
func resolveTaskRoutes(ctx context.Context, taskList []map[string]any, cfg map[string]any, parent *AIAgent) ([]*tools.TaskRoute, string, error) {
	var backendAgent tools.DelegationBackendAgent
	if parent != nil {
		backendAgent = newDelegationAgentView(parent)
	}
	if pinned := tools.PinnedBackend(backendAgent); pinned != "" {
		cfg = maps.Clone(cfg)
		cfg["backend"] = pinned
	}
	p, err := tools.GetActiveDelegationProvider(ctx, cfg)
	if err != nil {
		return nil, err.Error(), nil
	}
	declared := tools.DeclaredDelegationParameters(p, cfg)
	cache := map[string]*tools.TaskRoute{}
	routes := make([]*tools.TaskRoute, 0, len(taskList))
	for i, task := range taskList {
		r, err := func() (*tools.TaskRoute, error) {
			route, err := routeOneTask(ctx, p, declared, i, task, cfg)
			if err != nil {
				return nil, err
			}
			if err := checkBackendEffort(i, route, cfg); err != nil {
				return nil, err
			}
			return routeToTaskRoute(ctx, route, parent, cache, cfg, p.Name())
		}()
		if err != nil {
			if !isDelegationValueError(err) {
				return nil, "", err
			}
			r = &tools.TaskRoute{Creds: map[string]any{}, RoutingCfg: map[string]any{}, Backend: p.Name(), Error: err.Error()}
		}
		routes = append(routes, r)
	}
	return routes, "", nil
}

// Route an internal caller's own config block describes, bypassing the delegation provider
func routeForConfig(ctx context.Context, credentialsCfg map[string]any, parent *AIAgent) (*tools.TaskRoute, error) {
	creds, err := resolveDelegationCredentialsFn(ctx, credentialsCfg, parent)
	if err != nil {
		if isDelegationValueError(err) {
			return nil, &delegateValueError{err.Error()}
		}
		return nil, err
	}
	return &tools.TaskRoute{Creds: creds, RoutingCfg: credentialsCfg, Backend: tools.DefaultDelegationBackend}, nil
}
