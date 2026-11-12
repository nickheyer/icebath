package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var delegationRegistryLog = slog.Default().With("logger", "agent.delegation_registry")

// Delegation providers plugins register, scoped by profile home like every provider registry
var delegationRegistry = provider.NewProviderRegistry[tools.DelegationProvider](provider.ProviderRegistryOptions{
	Label: "Delegation", Logger: delegationRegistryLog, Normalize: provider.LowerKey,
})

// Plugin host object subclassing DelegationProvider wrapped as a Go provider
type hostedDelegationProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses DelegationProvider as a provider, any other value unchanged
func adaptHostedDelegationProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || !obj.HasMethod("route_task") {
		return p
	}
	return hostedDelegationProvider{obj: obj}
}

func (h hostedDelegationProvider) attr(name string) any {
	v, _ := h.obj.Get(context.Background(), name)
	return v
}

func (h hostedDelegationProvider) call(method string, args ...any) (any, error) {
	return h.obj.Call(context.Background(), method, args, nil)
}

func (h hostedDelegationProvider) mustCall(method string, args ...any) any {
	v, err := h.call(method, args...)
	if err != nil {
		panic(err)
	}
	return v
}

func (h hostedDelegationProvider) Name() string {
	s, _ := h.attr("name").(string)
	return s
}

func (h hostedDelegationProvider) DisplayName() string {
	if s, ok := h.attr("display_name").(string); ok {
		return s
	}
	return pystr.Title(h.Name())
}

func (h hostedDelegationProvider) GetSetupSchema() map[string]any {
	if m, ok := h.mustCall("get_setup_schema").(map[string]any); ok {
		return m
	}
	return map[string]any{"name": h.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

func (h hostedDelegationProvider) Settings(config map[string]any) map[string]any {
	if m, ok := h.mustCall("settings", config).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func (h hostedDelegationProvider) Summary() string {
	s, _ := h.attr("summary").(string)
	return s
}

func (h hostedDelegationProvider) IsAvailable() bool {
	return pyrepr.Truthy(h.mustCall("is_available"))
}

func (h hostedDelegationProvider) TaskParameters(config map[string]any) map[string]map[string]any {
	raw, _ := h.mustCall("task_parameters", config).(map[string]any)
	out := map[string]map[string]any{}
	for name, spec := range raw {
		if m, ok := spec.(map[string]any); ok {
			out[name] = m
		}
	}
	return out
}

func (h hostedDelegationProvider) Description(config map[string]any) string {
	s, _ := h.mustCall("description", config).(string)
	return s
}

// Route of one task, a Python DelegationRouteError crossing the wire as the Go route refusal
func (h hostedDelegationProvider) RouteTask(task, config map[string]any) (map[string]any, error) {
	v, err := h.call("route_task", task, config)
	if err != nil {
		var remote *plugins.PluginHostError
		if errors.As(err, &remote) && remote.TypeName == "DelegationRouteError" {
			return nil, &tools.DelegationRouteError{Message: remote.RemoteMessage}
		}
		return nil, err
	}
	route, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("delegation provider %s route_task returned %T, not a mapping", h.Name(), v)
	}
	return route, nil
}

// Scoped registry view that wraps plugin host objects as DelegationProvider values before delegating
type hostedDelegationScopedRegistry struct{ inner plugins.ScopedRegistry }

func (s hostedDelegationScopedRegistry) Accepts(p any) bool {
	return s.inner.Accepts(adaptHostedDelegationProvider(p))
}
func (s hostedDelegationScopedRegistry) ProviderName(p any) string {
	return s.inner.ProviderName(adaptHostedDelegationProvider(p))
}
func (s hostedDelegationScopedRegistry) Identity(p any) error {
	return s.inner.Identity(adaptHostedDelegationProvider(p))
}
func (s hostedDelegationScopedRegistry) Snapshot(name, scope string) any {
	return s.inner.Snapshot(name, scope)
}
func (s hostedDelegationScopedRegistry) Register(p any, scope string) (bool, error) {
	return s.inner.Register(adaptHostedDelegationProvider(p), scope)
}
func (s hostedDelegationScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	return s.inner.Restore(name, adaptHostedDelegationProvider(current), adaptHostedDelegationProvider(replacement), scope)
}

// Plugin facing registry that register_delegation_provider writes into, adapting hosted Python providers
func DelegationScopedRegistry() plugins.ScopedRegistry {
	return hostedDelegationScopedRegistry{delegationRegistry.Scoped()}
}

// Installs the hermes delegation_registry reads over the plugin manager and the compiled-in strategies
func InstallDelegationRegistryHooks() {
	tools.DelegationRegistryHooks.GetProvider = func(ctx context.Context, name string) tools.DelegationProvider {
		return delegationRegistry.GetProvider(ctx, name, "")
	}
	tools.DelegationRegistryHooks.ListProviders = func(ctx context.Context) []tools.DelegationProvider {
		return delegationRegistry.ListProviders(ctx, "")
	}
	tools.DelegationBackendHooks.ListProviders = tools.DelegationRegistryHooks.ListProviders
	tools.DelegationBackendHooks.RefreshAgentMCPTools = func(ctx context.Context, a tools.DelegationRebuildAgent, quiet, contentAware bool) {
		mcp.RefreshAgentMCPTools(ctx, a.(*delegationAgentView).a, mcp.RefreshOptions{Quiet: quiet, ContentAware: contentAware})
	}
	tools.DelegationBackendHooks.RecordCacheBreak = func(ctx context.Context, a tools.DelegationRebuildAgent, reason string) {
		observability.RecordCacheBreak(ctx, a.(*delegationAgentView).a, reason)
	}
	tools.DelegationRegistryHooks.DisabledPlugins = plugins.DisabledPlugins
	tools.DelegationRegistryHooks.DiscoverPlugins = plugins.DiscoverPlugins
	tools.DelegationRegistryHooks.LoadBundled = loadBundledDelegationStrategy
}

// Runs the compiled-in strategy plugin of a name through a registrar sink, false when none is bundled or registers
func loadBundledDelegationStrategy(ctx context.Context, name string, register func(tools.DelegationProvider)) bool {
	plugin, ok := plugins.BundledPlugin("delegation/" + name)
	if !ok {
		return false
	}
	manifest := plugin.Manifest()
	registered := false
	registrar := plugins.NewContext(&manifest, plugins.GetPluginManager(ctx)).WithDelegationSink(func(p any) {
		if dp, ok := adaptHostedDelegationProvider(p).(tools.DelegationProvider); ok {
			register(dp)
			registered = true
		}
	})
	if err := plugin.Register(registrar); err != nil {
		delegationRegistryLog.Warn(fmt.Sprintf("bundled delegation strategy %s failed to register: %v", name, err))
	}
	return registered
}
