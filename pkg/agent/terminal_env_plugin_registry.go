package agent

import (
	"context"

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

// Attribute of the hosted object, a failing read raising like the Python attribute access
func (h hostedTerminalEnvProvider) mustGet(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		panic(err)
	}
	return v
}

// Method call of the hosted object, a failing call raising like the Python call
func (h hostedTerminalEnvProvider) mustCall(method string, args ...any) any {
	v, err := h.obj.Call(context.Background(), method, args, nil)
	if err != nil {
		panic(err)
	}
	return v
}

// String attribute, empty for None
func (h hostedTerminalEnvProvider) text(name string) string {
	s, _ := h.mustGet(name).(string)
	return s
}

func (h hostedTerminalEnvProvider) Name() string           { return h.text("name") }
func (h hostedTerminalEnvProvider) DisplayName() string    { return h.text("display_name") }
func (h hostedTerminalEnvProvider) Description() string    { return h.text("description") }
func (h hostedTerminalEnvProvider) EnvDescription() string { return h.text("env_description") }
func (h hostedTerminalEnvProvider) IsRemote() bool         { return config.PyTruthy(h.mustGet("is_remote")) }
func (h hostedTerminalEnvProvider) IsContainer() bool {
	return config.PyTruthy(h.mustGet("is_container"))
}
func (h hostedTerminalEnvProvider) SkipContainerGuards() bool {
	return config.PyTruthy(h.mustGet("skip_container_guards"))
}
func (h hostedTerminalEnvProvider) CachePathBase() string { return h.text("cache_path_base") }
func (h hostedTerminalEnvProvider) DefaultCwd() string    { return h.text("default_cwd") }

// Vendor credential names of the plugin, a frozenset crossing the wire as a list
func (h hostedTerminalEnvProvider) StripEnvKeys() []string {
	items, _ := h.mustGet("strip_env_keys").([]any)
	keys := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			keys = append(keys, s)
		}
	}
	return keys
}

func (h hostedTerminalEnvProvider) SessionIsolatedWhenNonpersistent() bool {
	return config.PyTruthy(h.mustGet("session_isolated_when_nonpersistent"))
}
func (h hostedTerminalEnvProvider) SupportsStreams() bool {
	return config.PyTruthy(h.mustGet("supports_streams"))
}
func (h hostedTerminalEnvProvider) IsAvailable() bool {
	return config.PyTruthy(h.mustCall("is_available"))
}

// Requirements verdict of the plugin, a False answer reading as the requirements not met
func (h hostedTerminalEnvProvider) CheckRequirements(_ *tools.Env, cfg map[string]any) error {
	v, err := h.obj.Call(context.Background(), "check_requirements", []any{cfg}, nil)
	if err != nil {
		return err
	}
	if !config.PyTruthy(v) {
		return tools.ErrBackendRequirementsNotMet
	}
	return nil
}

// Dashboard picker health pair of the plugin
func (h hostedTerminalEnvProvider) Probe() (string, string) {
	pair, _ := h.mustCall("probe").([]any)
	var status, detail string
	if len(pair) > 0 {
		status, _ = pair[0].(string)
	}
	if len(pair) > 1 {
		detail, _ = pair[1].(string)
	}
	return status, detail
}

func (h hostedTerminalEnvProvider) SetupInstructions(*tools.Env) []string {
	items, _ := h.mustCall("setup_instructions").([]any)
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			lines = append(lines, s)
		}
	}
	return lines
}

// Runs the hosted provider's interactive post_setup hook once the setup wizard selects it
func (h hostedTerminalEnvProvider) PostSetup(ctx context.Context, _ *tools.Env) error {
	_, err := h.obj.Call(ctx, "post_setup", nil, nil)
	return err
}

var _ tools.TerminalBackendPostSetup = hostedTerminalEnvProvider{}

// Doctor rows of the plugin, each wire tuple read as ok, label and detail
func (h hostedTerminalEnvProvider) DoctorChecks() []tools.DoctorCheck {
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
