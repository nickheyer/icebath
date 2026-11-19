package agent

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const hostedTerminalEnvBase = "agent.terminal_env_provider:TerminalEnvironmentProvider"

// Python plugin terminal environment provider object proxied from the plugin host
type hostedTerminalEnvProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses TerminalEnvironmentProvider as a backend, any other value unchanged
func adaptHostedTerminalEnvProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || (obj.Base != hostedTerminalEnvBase && !obj.HasMethod("create_environment")) {
		return p
	}
	return hostedTerminalEnvProvider{obj: obj}
}

// Contract defaults bound to this provider for every member the plugin object does not declare
func (h hostedTerminalEnvProvider) defaults() tools.TerminalEnvironmentProviderDefaults {
	return tools.TerminalEnvironmentProviderDefaults{Self: h}
}

// Attribute of the hosted object, a failing read raising like the Python attribute access
func (h hostedTerminalEnvProvider) mustGet(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		panic(err)
	}
	return v
}

// Attribute of the hosted object and whether the object declares it, any other failing read raising like mustGet
func (h hostedTerminalEnvProvider) declared(name string) (any, bool) {
	v, err := h.obj.Get(context.Background(), name)
	var builtin *plugins.HostBuiltinError
	if errors.As(err, &builtin) && builtin.Type == "AttributeError" {
		return nil, false
	}
	if err != nil {
		panic(err)
	}
	return v, true
}

// Method call of the hosted object, a failing call raising like the Python call
func (h hostedTerminalEnvProvider) mustCall(method string, args ...any) any {
	v, err := h.obj.Call(context.Background(), method, args, nil)
	if err != nil {
		panic(err)
	}
	return v
}

// Declared string attribute, empty for None, def when the object does not declare it
func (h hostedTerminalEnvProvider) text(name string, def func() string) string {
	v, declared := h.declared(name)
	if !declared {
		return def()
	}
	s, _ := v.(string)
	return s
}

// Truth of a declared attribute, def when the object does not declare it
func (h hostedTerminalEnvProvider) flag(name string, def func() bool) bool {
	v, declared := h.declared(name)
	if !declared {
		return def()
	}
	return config.PyTruthy(v)
}

func (h hostedTerminalEnvProvider) Name() string {
	s, _ := h.mustGet("name").(string)
	return s
}
func (h hostedTerminalEnvProvider) DisplayName() string {
	return h.text("display_name", h.defaults().DisplayName)
}
func (h hostedTerminalEnvProvider) Description() string {
	return h.text("description", h.defaults().Description)
}
func (h hostedTerminalEnvProvider) EnvDescription() string {
	return h.text("env_description", h.defaults().EnvDescription)
}
func (h hostedTerminalEnvProvider) IsRemote() bool { return h.flag("is_remote", h.defaults().IsRemote) }
func (h hostedTerminalEnvProvider) IsContainer() bool {
	return h.flag("is_container", h.defaults().IsContainer)
}
func (h hostedTerminalEnvProvider) SkipContainerGuards() bool {
	return h.flag("skip_container_guards", h.defaults().SkipContainerGuards)
}
func (h hostedTerminalEnvProvider) CachePathBase() string {
	return h.text("cache_path_base", h.defaults().CachePathBase)
}
func (h hostedTerminalEnvProvider) DefaultCwd() string {
	return h.text("default_cwd", h.defaults().DefaultCwd)
}

// Vendor credential names of the plugin, a frozenset crossing the wire as a list
func (h hostedTerminalEnvProvider) StripEnvKeys() []string {
	v, declared := h.declared("strip_env_keys")
	if !declared {
		return h.defaults().StripEnvKeys()
	}
	items, _ := v.([]any)
	keys := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			keys = append(keys, s)
		}
	}
	return keys
}

func (h hostedTerminalEnvProvider) SessionIsolatedWhenNonpersistent() bool {
	return h.flag("session_isolated_when_nonpersistent", h.defaults().SessionIsolatedWhenNonpersistent)
}
func (h hostedTerminalEnvProvider) SupportsStreams() bool {
	return h.flag("supports_streams", h.defaults().SupportsStreams)
}
func (h hostedTerminalEnvProvider) IsAvailable() bool {
	return config.PyTruthy(h.mustCall("is_available"))
}

// Requirements verdict of the plugin, a False answer reading as the requirements not met and the availability probe without the method
func (h hostedTerminalEnvProvider) CheckRequirements(env *tools.Env, cfg map[string]any) error {
	if !h.obj.HasMethod("check_requirements") {
		return h.defaults().CheckRequirements(env, cfg)
	}
	v, err := h.obj.Call(context.Background(), "check_requirements", []any{cfg}, nil)
	if err != nil {
		return err
	}
	if !config.PyTruthy(v) {
		return tools.ErrBackendRequirementsNotMet
	}
	return nil
}

// Setup lines of the plugin, none when it declares no setup_instructions
func (h hostedTerminalEnvProvider) SetupInstructions(env *tools.Env) []string {
	if !h.obj.HasMethod("setup_instructions") {
		return h.defaults().SetupInstructions(env)
	}
	items, _ := h.mustCall("setup_instructions").([]any)
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			lines = append(lines, s)
		}
	}
	return lines
}

// Runs the hosted provider's interactive post_setup hook once the setup wizard selects it, nothing when it declares none
func (h hostedTerminalEnvProvider) PostSetup(ctx context.Context, _ *tools.Env) error {
	if !h.obj.HasMethod("post_setup") {
		return nil
	}
	_, err := h.obj.Call(ctx, "post_setup", nil, nil)
	return err
}

var _ tools.TerminalBackendPostSetup = hostedTerminalEnvProvider{}

// Doctor rows of the plugin, each wire tuple read as ok, label and detail, the availability row when it declares no doctor_checks
func (h hostedTerminalEnvProvider) DoctorChecks() []tools.DoctorCheck {
	if !h.obj.HasMethod("doctor_checks") {
		return h.defaults().DoctorChecks()
	}
	rows, _ := h.mustCall("doctor_checks").([]any)
	out := make([]tools.DoctorCheck, 0, len(rows))
	for _, r := range rows {
		row, _ := r.([]any)
		check := tools.DoctorCheck{}
		if len(row) > 0 {
			check.OK = config.PyTruthy(row[0])
		}
		if len(row) > 1 {
			check.Label, _ = row[1].(string)
		}
		if len(row) > 2 {
			check.Detail, _ = row[2].(string)
		}
		out = append(out, check)
	}
	return out
}

// The host cannot hand a plugin environment to this process, so creation is refused
func (h hostedTerminalEnvProvider) CreateEnvironment(context.Context, *tools.Env, tools.CreateOptions) (tools.Environment, error) {
	return nil, tools.HostedBackendRefusal(h.Name())
}

// Environments of this provider live in the plugin host outside this process
func (h hostedTerminalEnvProvider) RunsInPluginHost() bool { return true }

// Scoped registry view that wraps plugin host objects as BackendProvider values before delegating
type hostedTerminalEnvScopedRegistry struct{ inner plugins.ScopedRegistry }

func (s hostedTerminalEnvScopedRegistry) Accepts(p any) bool {
	return s.inner.Accepts(adaptHostedTerminalEnvProvider(p))
}
func (s hostedTerminalEnvScopedRegistry) ProviderName(p any) string {
	return s.inner.ProviderName(adaptHostedTerminalEnvProvider(p))
}
func (s hostedTerminalEnvScopedRegistry) Identity(p any) error {
	return s.inner.Identity(adaptHostedTerminalEnvProvider(p))
}
func (s hostedTerminalEnvScopedRegistry) Snapshot(name, scope string) any {
	return s.inner.Snapshot(name, scope)
}
func (s hostedTerminalEnvScopedRegistry) Register(p any, scope string) (bool, error) {
	return s.inner.Register(adaptHostedTerminalEnvProvider(p), scope)
}
func (s hostedTerminalEnvScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	return s.inner.Restore(name, adaptHostedTerminalEnvProvider(current), adaptHostedTerminalEnvProvider(replacement), scope)
}

// Plugin facing registry that register_terminal_environment_provider writes into, adapting hosted Python providers
func TerminalEnvScopedRegistry() plugins.ScopedRegistry {
	return hostedTerminalEnvScopedRegistry{tools.TerminalEnvRegistryScoped()}
}
