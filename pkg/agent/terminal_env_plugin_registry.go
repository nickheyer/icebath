package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const hostedTerminalEnvBase = "agent.terminal_env_provider:TerminalEnvironmentProvider"

// Terminal environment provider object of a guest plugin
type hostedTerminalEnvProvider struct{ obj *plugins.HostedObject }

// Guest object standing for a terminal environment provider as a backend, any other value unchanged
func adaptHostedTerminalEnvProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || (obj.Base != hostedTerminalEnvBase && !obj.HasMethod("create_environment")) {
		return p
	}
	return hostedTerminalEnvProvider{obj: obj}
}

// Attribute of the guest object, a failing read escaping as a panic
func (h hostedTerminalEnvProvider) mustGet(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		panic(err)
	}
	return v
}

// Method call of the guest object, a failing call escaping as a panic
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

// Vendor credential names the plugin strips from the environment
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

// Environment the guest backend builds for the options, its commands run in the guest
func (h hostedTerminalEnvProvider) CreateEnvironment(ctx context.Context, _ *tools.Env, opts tools.CreateOptions) (tools.Environment, error) {
	kwargs := map[string]any{"cwd": opts.Cwd, "timeout": int64(opts.Timeout), "task_id": opts.TaskID, "image": opts.Image,
		"container_config": opts.ContainerConfig, "host_cwd": opts.HostCwd, "probe_only": opts.ProbeOnly}
	v, err := h.obj.Call(ctx, "create_environment", nil, kwargs)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*plugins.HostedObject)
	if !ok || obj == nil {
		return nil, fmt.Errorf("terminal backend %s create_environment returned %T, not an environment object", h.Name(), v)
	}
	e := &hostedEnvironment{obj: obj, envType: h.Name(), hostCwd: opts.HostCwd, cwd: opts.Cwd}
	if cwd := e.attrText("cwd"); cwd != "" {
		e.cwd = cwd
	}
	return e, nil
}

// Environment object of a guest terminal backend, every command and file fetch a call into the guest
type hostedEnvironment struct {
	obj     *plugins.HostedObject
	envType string
	hostCwd string
	mu      sync.Mutex
	cwd     string
}

// String attribute of the environment object, empty when it is absent or not a string
func (e *hostedEnvironment) attrText(name string) string {
	v, err := e.obj.Get(context.Background(), name)
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// Flag attribute of the environment object, false when it is absent
func (e *hostedEnvironment) attrFlag(name string) bool {
	v, err := e.obj.Get(context.Background(), name)
	return err == nil && config.PyTruthy(v)
}

// Runs one command in the guest and reads its output, exit code and the cwd it reports
func (e *hostedEnvironment) Execute(ctx context.Context, command string, opts tools.ExecOptions) (*tools.ExecResult, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = e.Cwd()
	}
	kwargs := map[string]any{"cwd": cwd, "timeout": int64(opts.Timeout)}
	if opts.Stdin != nil {
		kwargs["stdin"] = *opts.Stdin
	}
	v, err := e.obj.Call(ctx, "execute", []any{command}, kwargs)
	if err != nil {
		return nil, err
	}
	reply, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("terminal backend %s execute returned %T, not a result mapping", e.envType, v)
	}
	out := &tools.ExecResult{TimedOut: config.PyTruthy(reply["timed_out"]), Interrupted: config.PyTruthy(reply["interrupted"])}
	out.Output, _ = reply["output"].(string)
	out.OutputTotalChars = len([]rune(out.Output))
	switch code := reply["returncode"].(type) {
	case int64:
		out.ReturnCode = int(code)
	case float64:
		out.ReturnCode = int(code)
	default:
		return nil, fmt.Errorf("terminal backend %s execute returned no integer returncode", e.envType)
	}
	if observed, ok := reply["cwd"].(string); ok && observed != "" {
		out.Cwd, out.CwdObserved = observed, true
		e.SetCwd(observed)
	}
	return out, nil
}

func (e *hostedEnvironment) Cleanup(forceRemove bool) error {
	_, err := e.obj.Call(context.Background(), "cleanup", nil, map[string]any{"force_remove": forceRemove})
	return err
}

func (e *hostedEnvironment) EnvType() string { return e.envType }

func (e *hostedEnvironment) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

// Moves the working directory here and on the guest object
func (e *hostedEnvironment) SetCwd(cwd string) {
	e.mu.Lock()
	changed := e.cwd != cwd
	e.cwd = cwd
	e.mu.Unlock()
	if changed {
		_ = e.obj.Set(context.Background(), "cwd", cwd)
	}
}

func (e *hostedEnvironment) TempDir() string {
	if dir := e.attrText("temp_dir"); dir != "" {
		return dir
	}
	return "/tmp"
}

func (e *hostedEnvironment) HostCwd() string          { return e.hostCwd }
func (e *hostedEnvironment) HostCwdMount() string     { return e.attrText("host_cwd_mount") }
func (e *hostedEnvironment) SandboxHome() string      { return e.attrText("sandbox_home") }
func (e *hostedEnvironment) IsPersistent() bool       { return e.attrFlag("persistent") }
func (e *hostedEnvironment) IsSessionScoped() bool    { return e.attrFlag("session_scoped") }
func (e *hostedEnvironment) UsesHostFilesystem() bool { return e.attrFlag("uses_host_filesystem") }

// Copies a file out of the guest environment to localDest, refusing more than maxBytes when positive
func (e *hostedEnvironment) FetchFile(ctx context.Context, remotePath, localDest string, maxBytes int) error {
	v, err := e.obj.Call(ctx, "fetch_file", []any{remotePath}, map[string]any{"max_bytes": int64(maxBytes)})
	if err != nil {
		return err
	}
	data, ok := v.([]byte)
	if !ok {
		return fmt.Errorf("terminal backend %s fetch_file returned %T, not bytes", e.envType, v)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return fmt.Errorf("%s is larger than %d bytes", remotePath, maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(localDest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(localDest, data, 0o644)
}

// Resolved path the guest reports, the path itself when the guest cannot resolve it
func (e *hostedEnvironment) FetchRealpath(ctx context.Context, remotePath string) string {
	v, err := e.obj.Call(ctx, "fetch_realpath", []any{remotePath}, nil)
	if s, ok := v.(string); err == nil && ok && s != "" {
		return s
	}
	return remotePath
}

// Scoped registry view that wraps guest objects as BackendProvider values before delegating
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

// Plugin facing registry that register_terminal_environment_provider writes into, adapting guest providers
func TerminalEnvScopedRegistry() plugins.ScopedRegistry {
	return hostedTerminalEnvScopedRegistry{tools.TerminalEnvRegistryScoped()}
}
