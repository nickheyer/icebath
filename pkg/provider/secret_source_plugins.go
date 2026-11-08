package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Overridable base methods a source answers in place of the spec driven defaults
type SecretSourceHooks interface {
	IsEnabled(cfg map[string]any) (bool, error)
	OverrideExisting(cfg map[string]any) (bool, error)
	ProtectedEnvVars(cfg map[string]any) (map[string]bool, error)
	FetchTimeoutSeconds(cfg map[string]any) (float64, error)
	Remediation(kind SecretErrorKind, cfg map[string]any) (string, error)
}

// Whether the source runs for its section, through its own activation when it declares one
func secretSourceIsEnabled(source SecretSource, cfg map[string]any) (bool, error) {
	if h, ok := source.(SecretSourceHooks); ok {
		return h.IsEnabled(cfg)
	}
	return SecretSourceEnabled(cfg), nil
}

// Whether the source may overwrite existing values, false when its override raises
func secretSourceOverrides(source SecretSource, cfg map[string]any) bool {
	if h, ok := source.(SecretSourceHooks); ok {
		on, err := h.OverrideExisting(cfg)
		return err == nil && on
	}
	return SecretSourceOverrideExisting(source.Spec(), cfg)
}

// Names the source protects from every source, none when its override raises
func secretSourceProtected(source SecretSource, cfg map[string]any) map[string]bool {
	if h, ok := source.(SecretSourceHooks); ok {
		names, err := h.ProtectedEnvVars(cfg)
		if err != nil {
			return map[string]bool{}
		}
		return names
	}
	return SecretSourceProtectedEnvVars(source.Spec(), cfg)
}

// Wall clock budget of one fetch, the raised error of an override propagating to the apply
func secretSourceTimeout(source SecretSource, cfg map[string]any) (time.Duration, error) {
	if h, ok := source.(SecretSourceHooks); ok {
		seconds, err := h.FetchTimeoutSeconds(cfg)
		if err != nil {
			return 0, err
		}
		return time.Duration(seconds * float64(time.Second)), nil
	}
	return SecretSourceFetchTimeout(cfg), nil
}

// Fix it hint of a failed fetch, empty when the source's remediation raises
func secretSourceRemediation(source SecretSource, kind SecretErrorKind, cfg map[string]any) string {
	if h, ok := source.(SecretSourceHooks); ok {
		text, err := h.Remediation(kind, cfg)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(SecretSourceRemediation(source.Spec(), kind, cfg))
}

// Secret source a plugin host child serves, every base method answered by the child object
type hostedSecretSource struct{ obj *plugins.HostedObject }

var _ SecretSourceHooks = hostedSecretSource{}

// Hosted proxy of the secret source base as a SecretSource, false for any other value
func adaptSecretSource(p any) (SecretSource, bool) {
	switch v := p.(type) {
	case *plugins.HostedObject:
		if v == nil || v.Base != plugins.HostObjectBases["register_secret_source"] {
			return nil, false
		}
		return hostedSecretSource{obj: v}, true
	case SecretSource:
		if v == nil {
			return nil, false
		}
		return v, true
	}
	return nil, false
}

// The value a plugin registered, the hosted proxy for an adapted source and nil for an empty slot
func unwrapSecretSource(s SecretSource) any {
	switch v := s.(type) {
	case nil:
		return nil
	case hostedSecretSource:
		return v.obj
	}
	return s
}

func (h hostedSecretSource) attr(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		return nil
	}
	return v
}

func (h hostedSecretSource) str(name string) string {
	s, _ := h.attr(name).(string)
	return s
}

func (h hostedSecretSource) call(ctx context.Context, method string, args ...any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return h.obj.Call(ctx, method, args, nil)
}

// Class attributes of the hosted source read through the proxy
func (h hostedSecretSource) Spec() SecretSourceSpec {
	version := 0
	if n, ok := pyrepr.Int(h.attr("api_version")); ok {
		version = int(n.Int64())
	}
	hints := map[SecretErrorKind]string{}
	for k, v := range hostedMapping(h.attr("remediation_hints")) {
		if text, ok := v.(string); ok {
			hints[SecretErrorKind(k)] = text
		}
	}
	return SecretSourceSpec{APIVersion: version, Name: h.str("name"), Label: h.str("label"), Shape: h.str("shape"),
		Scheme: h.str("scheme"), TokenEnvKey: h.str("token_env_key"), DefaultTokenEnv: h.str("default_token_env"),
		OverrideExistingDefault: truthyAny(h.attr("override_existing_default")), RemediationHints: hints}
}

// Runs the child's fetch for the home and rebuilds its FetchResult, a raise or a foreign value failing as internal
func (h hostedSecretSource) Fetch(cfg map[string]any, homePath string, _ *SourceEnvironment) *SecretFetchResult {
	got, err := h.call(home.WithOverride(context.Background(), homePath), "fetch", cfg, homePath)
	if err != nil {
		return NewSecretFetchResult().Fail(fmt.Sprintf("fetch raised %s: %s", plugins.ErrorTypeName(err), err.Error()), SecretErrorInternal)
	}
	dc, ok := got.(*plugins.Dataclass)
	if !ok || dc == nil || dc.Type != "FetchResult" {
		name := pyrepr.TypeName(got)
		if ok && dc != nil {
			name = dc.Type
		}
		return NewSecretFetchResult().Fail(fmt.Sprintf("fetch returned %s instead of FetchResult", name), SecretErrorInternal)
	}
	result := NewSecretFetchResult()
	for k, v := range hostedMapping(dc.Fields["secrets"]) {
		if s, isStr := v.(string); isStr {
			result.Secrets[k] = s
		}
	}
	result.Applied = hostedStrings(dc.Fields["applied"])
	result.Skipped = hostedStrings(dc.Fields["skipped"])
	result.Warnings = hostedStrings(dc.Fields["warnings"])
	result.BinaryPath, _ = dc.Fields["binary_path"].(string)
	if raw := dc.Fields["error"]; raw != nil {
		kind, _ := dc.Fields["error_kind"].(string)
		result.Fail(fmt.Sprint(raw), SecretErrorKind(kind))
	}
	return result
}

// Setup UI schema the child declares, key to description and default
func (h hostedSecretSource) ConfigSchema() map[string]map[string]any {
	got, err := h.call(nil, "config_schema")
	out := map[string]map[string]any{}
	if err != nil {
		return out
	}
	for k, v := range hostedMapping(got) {
		out[k] = hostedMapping(v)
	}
	return out
}

func (h hostedSecretSource) IsEnabled(cfg map[string]any) (bool, error) {
	got, err := h.call(nil, "is_enabled", cfg)
	return err == nil && truthyAny(got), err
}

func (h hostedSecretSource) OverrideExisting(cfg map[string]any) (bool, error) {
	got, err := h.call(nil, "override_existing", cfg)
	return err == nil && truthyAny(got), err
}

func (h hostedSecretSource) ProtectedEnvVars(cfg map[string]any) (map[string]bool, error) {
	got, err := h.call(nil, "protected_env_vars", cfg)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, name := range hostedStrings(got) {
		out[name] = true
	}
	return out, nil
}

func (h hostedSecretSource) FetchTimeoutSeconds(cfg map[string]any) (float64, error) {
	got, err := h.call(nil, "fetch_timeout_seconds", cfg)
	if err != nil {
		return 0, err
	}
	return CoerceFloat(got, DefaultSecretFetchTimeout.Seconds()), nil
}

func (h hostedSecretSource) Remediation(kind SecretErrorKind, cfg map[string]any) (string, error) {
	var wireKind any
	if kind != "" {
		wireKind = string(kind)
	}
	got, err := h.call(nil, "remediation", wireKind, cfg)
	if err != nil {
		return "", err
	}
	if got == nil || got == "" {
		return "", nil
	}
	return pyrepr.Str(got), nil
}

// String keyed view of a dict the host child returned
func hostedMapping(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case plugins.Record:
		return map[string]any(m)
	}
	return map[string]any{}
}

// String members of a list or set the host child returned
func hostedStrings(v any) []string {
	var items []any
	switch l := v.(type) {
	case []any:
		items = l
	case plugins.PySet:
		items = l
	}
	out := []string{}
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Plugin context view of the secret source registry that register_secret_source drives
func SecretSourceScopedRegistry() plugins.ScopedRegistry { return secretSourceScopedRegistry{} }

type secretSourceScopedRegistry struct{}

var _ plugins.ScopedRegistry = secretSourceScopedRegistry{}

// Whether the value is a SecretSource or a hosted proxy of the base
func (secretSourceScopedRegistry) Accepts(provider any) bool {
	_, ok := adaptSecretSource(provider)
	return ok
}

// Raw source name, the registrar keeps it unnormalized
func (secretSourceScopedRegistry) ProviderName(provider any) string {
	if s, ok := adaptSecretSource(provider); ok {
		return s.Spec().Name
	}
	return ""
}

// Secret sources are judged by the registry's own validation when they register
func (secretSourceScopedRegistry) Identity(any) error { return nil }

// Exact registered value in the slot, nil when empty
func (secretSourceScopedRegistry) Snapshot(name, scope string) any {
	return unwrapSecretSource(SnapshotSecretRegistration(name, scope))
}

// Registers without replacing a same named source, true when it now occupies its slot
func (secretSourceScopedRegistry) Register(provider any, scope string) (bool, error) {
	s, ok := adaptSecretSource(provider)
	if !ok {
		return false, nil
	}
	return RegisterSecretSource(s, false, false, scope), nil
}

// Puts the replacement back while current still occupies the slot
func (secretSourceScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	cur, ok := adaptSecretSource(current)
	if !ok {
		return false
	}
	var previous SecretSource
	if replacement != nil {
		if previous, ok = adaptSecretSource(replacement); !ok {
			return false
		}
	}
	return RestoreSecretRegistration(name, cur, previous, scope)
}

// Orchestrator the plugin manager re-pulls enabled plugin secret sources through after discovery
func PluginSecretSources() plugins.SecretSources { return pluginSecretSources{} }

type pluginSecretSources struct{}

// Plugin registered sources of the process scope, each with its own activation check
func (pluginSecretSources) ListPluginSources() []plugins.SecretSourceInfo {
	sources := ListPluginSecretSources()
	out := make([]plugins.SecretSourceInfo, 0, len(sources))
	for _, s := range sources {
		out = append(out, plugins.SecretSourceInfo{Name: s.Spec().Name,
			IsEnabled: func(section map[string]any) (bool, error) { return secretSourceIsEnabled(s, section) }})
	}
	return out
}

func (pluginSecretSources) ResetCache(h string) error {
	ResetSecretSourceCache(h)
	return nil
}

func (pluginSecretSources) LoadDotenv(ctx context.Context, h string) error {
	_, err := LoadIcebathDotenv(ctx, DotenvLoadOptions{Home: h})
	return err
}

func (pluginSecretSources) RefreshInstalledScope(ctx context.Context, h string) error {
	RefreshInstalledSecretScope(ctx, h)
	return nil
}
