package provider

import (
	"errors"
	"testing"
	"time"
)

// Source overriding every base method, each answer or raise set by the test
type hookedSource struct {
	*testSource
	enabled   func(cfg map[string]any) (bool, error)
	timeout   float64
	timeErr   error
	overErr   error
	protErr   error
	remedy    string
	remedyErr error
}

func (h *hookedSource) IsEnabled(cfg map[string]any) (bool, error) { return h.enabled(cfg) }
func (h *hookedSource) OverrideExisting(map[string]any) (bool, error) {
	return h.overErr == nil, h.overErr
}
func (h *hookedSource) ProtectedEnvVars(map[string]any) (map[string]bool, error) {
	return map[string]bool{"GUARDED": true}, h.protErr
}
func (h *hookedSource) FetchTimeoutSeconds(map[string]any) (float64, error) {
	return h.timeout, h.timeErr
}
func (h *hookedSource) Remediation(SecretErrorKind, map[string]any) (string, error) {
	return h.remedy, h.remedyErr
}

func newHookedSource(name string, secrets map[string]string) *hookedSource {
	return &hookedSource{testSource: makeSource(name, "mapped", secrets), timeout: 5,
		enabled: func(cfg map[string]any) (bool, error) { id, _ := cfg["vault_id"].(string); return id != "", nil }}
}

func TestCustomActivationDrivesWhichSourcesApply(t *testing.T) {
	cleanRegistry(t)
	custom := newHookedSource("custom", map[string]string{"CUSTOM_KEY": "v"})
	raising := newHookedSource("raising", map[string]string{"RAISING_KEY": "v"})
	raising.enabled = func(map[string]any) (bool, error) { return false, errors.New("boom") }
	RegisterSecretSource(custom, false, false, "")
	RegisterSecretSource(raising, false, false, "")
	env := MappingSourceEnvironment(map[string]string{"GUARDED": "keep"})
	report := applyAllSecretSourcesForTest(t, map[string]any{"custom": map[string]any{"vault_id": "v1"}, "raising": map[string]any{"enabled": true}}, t.TempDir(), env)
	if len(report.Sources) != 1 || report.Sources[0].Name != "custom" || raising.calls != 0 {
		t.Fatalf("sources %+v raising calls %d", report.Sources, raising.calls)
	}
	if v, _ := env.Get("CUSTOM_KEY"); v != "v" {
		t.Fatalf("custom source not applied, got %q", v)
	}
}

func TestRaisingBaseOverridesDegradeLikeThePythonOrchestrator(t *testing.T) {
	cleanRegistry(t)
	src := newHookedSource("vault", map[string]string{"GUARDED": "new", "OPEN": "new"})
	src.overErr, src.protErr = errors.New("override raised"), errors.New("protected raised")
	RegisterSecretSource(src, false, false, "")
	env := MappingSourceEnvironment(map[string]string{"GUARDED": "old", "OPEN": "old"})
	applyAllSecretSourcesForTest(t, map[string]any{"vault": map[string]any{"vault_id": "x"}}, t.TempDir(), env)
	if v, _ := env.Get("OPEN"); v != "old" {
		t.Fatalf("raising override_existing must read as false, OPEN=%q", v)
	}
	src.timeErr = errors.New("timeout raised")
	if _, err := ApplyAllSecretSources(map[string]any{"vault": map[string]any{"vault_id": "x"}}, t.TempDir(), MappingSourceEnvironment(nil)); err == nil {
		t.Fatal("a raising fetch_timeout_seconds must propagate out of the apply")
	}
	src.remedy = "  fix it  "
	if got := secretSourceRemediation(src, SecretErrorAuthFailed, nil); got != "fix it" {
		t.Fatalf("remediation %q", got)
	}
	src.remedyErr = errors.New("remediation raised")
	if got := secretSourceRemediation(src, SecretErrorAuthFailed, nil); got != "" {
		t.Fatalf("raising remediation %q", got)
	}
	src.timeErr, src.timeout = nil, 0.05
	if d, err := secretSourceTimeout(src, nil); err != nil || d != 50*time.Millisecond {
		t.Fatalf("timeout %v %v", d, err)
	}
}

func TestSecretSourceScopedRegistryLeasesTheSlot(t *testing.T) {
	cleanRegistry(t)
	reg := SecretSourceScopedRegistry()
	scope := t.TempDir()
	first, second := makeSource("leased", "mapped", nil), makeSource("leased", "mapped", nil)
	if !reg.Accepts(first) || reg.Accepts("not a source") || reg.ProviderName(first) != "leased" {
		t.Fatal("registry type check")
	}
	if ok, err := reg.Register(first, scope); !ok || err != nil || reg.Snapshot("leased", scope) != any(first) {
		t.Fatalf("register %v %v", ok, err)
	}
	if ok, _ := reg.Register(second, scope); ok {
		t.Fatal("a same named source replaced the registration")
	}
	if !reg.Restore("leased", first, nil, scope) || reg.Snapshot("leased", scope) != nil {
		t.Fatal("restore did not empty the slot")
	}
	if reg.Restore("leased", first, nil, scope) {
		t.Fatal("restore of a vacated slot succeeded")
	}
}

func TestPluginSecretSourcesListsPluginOriginsWithTheirActivation(t *testing.T) {
	cleanRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	RegisterSecretSource(makeSource("bundled", "mapped", nil), false, true, "")
	custom := newHookedSource("custom", nil)
	RegisterSecretSource(custom, false, false, "")
	infos := PluginSecretSources().ListPluginSources()
	if len(infos) != 1 || infos[0].Name != "custom" {
		t.Fatalf("plugin sources %+v", infos)
	}
	if on, err := infos[0].IsEnabled(map[string]any{"enabled": true}); on || err != nil {
		t.Fatal("custom activation ignored the vault_id rule")
	}
	if on, _ := infos[0].IsEnabled(map[string]any{"vault_id": "v"}); !on {
		t.Fatal("custom activation refused a configured vault")
	}
}
