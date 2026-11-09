package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Host side of agent/plugin_llm.py, call_llm and async_call_llm over the auxiliary client with the main route readers
type pluginLLMClient struct{}

// PluginLLMClient is the auxiliary client cmd/icebath installs as plugins.Services.PluginLLM
func PluginLLMClient() plugins.PluginLLMClient { return pluginLLMClient{} }

// call_llm keyword arguments of PluginLlm._host_kwargs, route_info only when a task slot routes the call
func pluginLLMKwargs(req *plugins.AuxLLMRequest) map[string]any {
	kw := map[string]any{"task": optStr(req.Task), "provider": optStr(req.Provider), "model": optStr(req.Model),
		"messages": req.Messages, "temperature": optFloat(req.Temperature), "max_tokens": optIntArg(req.MaxTokens), "timeout": optFloat(req.Timeout)}
	if len(req.ExtraBody) > 0 {
		kw["extra_body"] = req.ExtraBody
	} else {
		kw["extra_body"] = nil
	}
	if req.RouteInfo != nil {
		kw["route_info"] = req.RouteInfo
	} else {
		kw["route_info"] = nil
	}
	return kw
}

func optIntArg(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func (pluginLLMClient) CallLLM(ctx context.Context, req *plugins.AuxLLMRequest) (any, error) {
	return provider.CallLLM(ctx, pluginLLMKwargs(req))
}

func (pluginLLMClient) AsyncCallLLM(ctx context.Context, req *plugins.AuxLLMRequest) (any, error) {
	return provider.AsyncCallLLM(ctx, pluginLLMKwargs(req))
}

func (pluginLLMClient) ReadMainProvider() string {
	return provider.AuxReadMainProvider(context.Background())
}
func (pluginLLMClient) ReadMainModel() string { return provider.AuxReadMainModel(context.Background()) }

// Child agent of one lifecycle launch read through the attributes agent/subagent_lifecycle.py takes off it
type subagentChild struct{ agent *AIAgent }

func (c subagentChild) SubagentID() string { return c.agent.SubagentID() }

func (c subagentChild) Provider() *string {
	if c.agent.Provider() == "" {
		return nil
	}
	v := c.agent.Provider()
	return &v
}

func (c subagentChild) Model() *string {
	if c.agent.Model() == "" {
		return nil
	}
	v := c.agent.Model()
	return &v
}

func (c subagentChild) DelegateRole() (string, bool) { return c.agent.delegateRole, true }
func (c subagentChild) DelegateDepth() int           { return c.agent.DelegateDepth() }

// The tools.delegate_tool collaborators SubagentLifecycleService imports lazily, over the real agents
type subagentDelegation struct{}

// SubagentDelegationService is the lifecycle delegation cmd/icebath installs as plugins.Services.SubagentLifecycle
func SubagentDelegationService() plugins.SubagentDelegation { return subagentDelegation{} }

// Parent handle as the live agent, nil for a foreign value the way getattr on a stranger reads None
func parentAgentOf(parent any) *AIAgent {
	switch p := parent.(type) {
	case *AIAgent:
		return p
	case subagentChild:
		return p.agent
	}
	return nil
}

func (subagentDelegation) SessionID(parent any) string {
	if a := parentAgentOf(parent); a != nil {
		return a.SessionID()
	}
	return ""
}

func (subagentDelegation) EnabledToolsets(parent any) ([]string, bool) {
	a := parentAgentOf(parent)
	if a == nil || a.EnabledToolsets() == nil {
		return nil, false
	}
	return a.EnabledToolsets(), true
}

func (subagentDelegation) ValidateToolset(ctx context.Context, name string) bool {
	return tools.Default.ValidateToolset(ctx, tools.ScopeKey(home.Dir(ctx)), name)
}

func (subagentDelegation) LoadConfig(ctx context.Context) map[string]any {
	return tools.LoadDelegationConfig(ctx)
}

func (subagentDelegation) ResolveCredentials(ctx context.Context, routingCfg map[string]any, parent any) (map[string]any, error) {
	a := parentAgentOf(parent)
	if a == nil {
		return nil, fmt.Errorf("No active Icebath parent session is available.")
	}
	return resolveDelegationCredentials(ctx, routingCfg, a)
}

// _build_child_preserving_parent_tools with task index 0, one task and the default iteration budget
func (subagentDelegation) BuildChild(ctx context.Context, spec plugins.SubagentChildSpec) (plugins.SubagentChild, error) {
	parent := parentAgentOf(spec.Parent)
	if parent == nil {
		return nil, fmt.Errorf("No active Icebath parent session is available.")
	}
	build := credentialOverrides(spec.Credentials, spec.RoutingCfg)
	build.TaskIndex = 0
	build.Goal = spec.Goal
	build.Context = optStr(spec.Context)
	build.Toolsets = spec.Toolsets
	build.Model = spec.Model
	build.MaxIterations = delegateDefaultMaxIterations
	build.TaskCount = 1
	build.Parent = parent
	build.Role = spec.Role
	child, err := buildChildPreservingParentTools(ctx, build)
	if err != nil {
		return nil, err
	}
	return subagentChild{agent: child}, nil
}

// _run_child_lifecycle of task index 0, the result dict as plain Go values
func (subagentDelegation) RunChild(ctx context.Context, goal string, child plugins.SubagentChild, parent any) (any, error) {
	c, ok := child.(subagentChild)
	if !ok {
		return nil, fmt.Errorf("subagent child %T is not an Icebath agent", child)
	}
	raw := runChildLifecycle(ctx, 0, goal, c.agent, parentAgentOf(parent))
	if raw == nil {
		return nil, nil
	}
	return pyjson.Plain(raw), nil
}

func (subagentDelegation) HardInterrupt(child plugins.SubagentChild, reason, toolReason string) (bool, error) {
	c, ok := child.(subagentChild)
	if !ok {
		return false, fmt.Errorf("subagent child %T is not an Icebath agent", child)
	}
	return RequestHardInterrupt(c.agent, reason, toolReason), nil
}

func (subagentDelegation) Steer(subagentID, text string) bool {
	return SteerSubagent(subagentID, text, nil, nil, nil)
}

// agent/delegation_backends.py DelegationBackends as the host facade a plugin reaches through ctx.delegation_backends
type delegationBackendsFacade struct{ backends tools.DelegationBackends }

// DelegationBackendsFor is the facade constructor cmd/icebath installs as plugins.Services.DelegationBackends
func DelegationBackendsFor(*plugins.Context) any { return delegationBackendsFacade{} }

var delegationBackendsMethods = map[string]bool{"list_backends": true, "set_global_backend": true}

func (delegationBackendsFacade) HostAttr(_ context.Context, name string) (any, bool, bool, error) {
	if delegationBackendsMethods[name] {
		return nil, true, false, nil
	}
	return nil, false, false, &plugins.HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'DelegationBackends' object has no attribute %s", pyrepr.Repr(name))}
}

func (f delegationBackendsFacade) HostCall(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	switch method {
	case "list_backends":
		if _, _, err := plugins.BindHostCall("DelegationBackends", method, nil, args, kwargs); err != nil {
			return nil, err
		}
		rows := f.backends.ListBackends(ctx)
		out := make([]any, len(rows))
		for i, row := range rows {
			out[i] = row
		}
		return out, nil
	case "set_global_backend":
		a, _, err := plugins.BindHostCall("DelegationBackends", method, []plugins.HostParam{plugins.HostReq("name"), plugins.HostKwo("session_key", ""), plugins.HostKwo("apply_now", false)}, args, kwargs)
		if err != nil {
			return nil, err
		}
		return nil, f.backends.SetGlobalBackend(ctx, pyrepr.Str(a["name"]), pyrepr.Str(a["session_key"]), pyrepr.Truthy(a["apply_now"]))
	}
	return nil, &plugins.HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'DelegationBackends' object has no attribute %s", pyrepr.Repr(method))}
}

// hermes_platform.declaration register and unregister over the tools declaration registry
type platformDeclarations struct{}

// PlatformDeclarationsService is the declaration registry cmd/icebath installs as plugins.Services.PlatformDeclarations
func PlatformDeclarationsService() plugins.PlatformDeclarations { return platformDeclarations{} }

func (platformDeclarations) Register(name string, app, requires map[string]any) error {
	decl, err := tools.ParsePlatformDeclaration(name, anyMap(app), anyMap(requires), "plugin.json extension server "+pyrepr.Repr(name))
	if err != nil {
		return err
	}
	tools.RegisterPlatformDeclaration(name, decl)
	return nil
}

func (platformDeclarations) Unregister(name string) { tools.UnregisterPlatformDeclaration(name) }

// A nil map as the Python None the declaration parser accepts
func anyMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}
