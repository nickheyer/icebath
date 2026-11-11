package dashboardauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Isolated home with an optional config.yaml, the global auth registry and token routes cleared around the case
func drainCase(t *testing.T, configYAML string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_DASHBOARD_DRAIN_SECRET", "")
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gateway.ClearTokenRoutes()
	gateway.ClearDashboardAuthProviders()
	plugins.InstallServices(&plugins.Services{DashboardAuth: gateway.DashboardAuthRegistryService(),
		HostObject: func(kind string, obj any) (string, bool) {
			if kind == "dashboard_auth_provider" {
				return gateway.HostedDashboardAuthProviderName(obj)
			}
			return "", false
		}})
	t.Cleanup(func() {
		gateway.ClearTokenRoutes()
		gateway.ClearDashboardAuthProviders()
		plugins.InstallServices(nil)
	})
}

// Plugin context of the launch profile the registry accepts providers from
func launchPluginContext() *plugins.Context {
	return plugins.NewContext(&plugins.Manifest{Name: "drain", Key: "dashboard_auth/drain"}, plugins.NewManager(home.Key(home.Process())))
}

func strongSecret() string { return tokenURLSafeForTest() }

func tokenURLSafeForTest() string { return b64urlNoPad(tokenBytes(32)) }

func registeredDrain(t *testing.T) *DrainSecretProvider {
	t.Helper()
	p, _ := gateway.SnapshotDashboardAuthRegistration("drain-secret", "").(*DrainSecretProvider)
	return p
}

// test_drain_provider.py TestEntropyGate test_strong_secret_passes
func TestEntropyGateStrongSecretPasses(t *testing.T) {
	drainCase(t, "")
	if reason := AssessSecretStrength(strongSecret(), drainDefaultMinSecretChars); reason != "" {
		t.Fatal(reason)
	}
}

// test_drain_provider.py TestEntropyGate test_empty_rejected
func TestEntropyGateEmptyRejected(t *testing.T) {
	drainCase(t, "")
	if AssessSecretStrength("", drainDefaultMinSecretChars) == "" {
		t.Fatal("empty secret accepted")
	}
}

// test_drain_provider.py TestEntropyGate test_too_short_rejected
func TestEntropyGateTooShortRejected(t *testing.T) {
	drainCase(t, "")
	if AssessSecretStrength(strings.Repeat("a1B2c3", 7), drainDefaultMinSecretChars) == "" {
		t.Fatal("42 char secret accepted")
	}
}

// test_drain_provider.py TestEntropyGate test_long_but_repeated_rejected
func TestEntropyGateLongButRepeatedRejected(t *testing.T) {
	drainCase(t, "")
	if AssessSecretStrength(strings.Repeat("a", 60), drainDefaultMinSecretChars) == "" {
		t.Fatal("repeated secret accepted")
	}
}

// test_drain_provider.py TestEntropyGate test_custom_min_chars_enforced
func TestEntropyGateCustomMinCharsEnforced(t *testing.T) {
	drainCase(t, "")
	if AssessSecretStrength(strongSecret(), 999) == "" {
		t.Fatal("secret under the custom minimum accepted")
	}
}

// test_drain_provider.py TestProvider test_protocol_compliance
func TestDrainProviderProtocolCompliance(t *testing.T) {
	drainCase(t, "")
	p, err := NewDrainSecretProvider(strongSecret(), "drain")
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.AssertDashboardAuthProtocol(p); err != nil {
		t.Fatal(err)
	}
}

// test_drain_provider.py TestProvider test_is_non_interactive
func TestDrainProviderIsNonInteractive(t *testing.T) {
	drainCase(t, "")
	p, err := NewDrainSecretProvider(strongSecret(), "")
	if err != nil {
		t.Fatal(err)
	}
	if gateway.DashboardAuthSupportsSession(p) {
		t.Fatal("drain provider supports sessions")
	}
}

// test_drain_provider.py TestProvider test_verify_token_accepts_matching_secret
func TestDrainProviderVerifyTokenAcceptsMatchingSecret(t *testing.T) {
	drainCase(t, "")
	s := strongSecret()
	p, err := NewDrainSecretProvider(s, "drain")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := p.VerifyToken(context.Background(), s)
	if err != nil || principal == nil {
		t.Fatalf("principal %v err %v", principal, err)
	}
	if principal.Principal != "drain-control" || principal.Provider != "drain-secret" || !slices.Equal(principal.Scopes, []string{"drain"}) {
		t.Fatalf("principal %+v", principal)
	}
}

// test_drain_provider.py TestProvider test_verify_token_rejects_empty
func TestDrainProviderVerifyTokenRejectsEmpty(t *testing.T) {
	drainCase(t, "")
	p, err := NewDrainSecretProvider(strongSecret(), "")
	if err != nil {
		t.Fatal(err)
	}
	if principal, err := p.VerifyToken(context.Background(), ""); principal != nil || err != nil {
		t.Fatalf("principal %v err %v", principal, err)
	}
}

// test_drain_provider.py TestProvider test_construction_rejects_weak_secret
func TestDrainProviderConstructionRejectsWeakSecret(t *testing.T) {
	drainCase(t, "")
	_, err := NewDrainSecretProvider("weak", "")
	var valueErr *gateway.DashboardAuthValueError
	if !errors.As(err, &valueErr) {
		t.Fatalf("err %v, want ValueError", err)
	}
}

// test_drain_provider.py TestProvider test_interactive_methods_raise
func TestDrainProviderInteractiveMethodsRaise(t *testing.T) {
	drainCase(t, "")
	p, err := NewDrainSecretProvider(strongSecret(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var notImpl *gateway.DashboardAuthNotImplementedError
	if _, err := p.StartLogin(ctx, "r"); !errors.As(err, &notImpl) {
		t.Fatalf("start_login %v", err)
	}
	if _, err := p.CompleteLogin(ctx, "c", "s", "v", "r"); !errors.As(err, &notImpl) {
		t.Fatalf("complete_login %v", err)
	}
	if _, err := p.RefreshSession(ctx, "r"); !errors.As(err, &notImpl) {
		t.Fatalf("refresh_session %v", err)
	}
}

// test_drain_provider.py TestRegister test_skips_when_no_secret
func TestDrainRegisterSkipsWhenNoSecret(t *testing.T) {
	drainCase(t, "")
	if err := (DrainPlugin{}).Register(launchPluginContext()); err != nil {
		t.Fatal(err)
	}
	if registeredDrain(t) != nil {
		t.Fatal("provider registered without a secret")
	}
	if !strings.Contains(DrainLastSkipReason(), "ICEBATH_DASHBOARD_DRAIN_SECRET") {
		t.Fatalf("skip reason %q", DrainLastSkipReason())
	}
	if gateway.IsTokenRoute(DrainRoutePath) {
		t.Fatal("drain route token authable")
	}
}

// test_drain_provider.py TestRegister test_skips_and_fails_closed_on_weak_secret
func TestDrainRegisterSkipsAndFailsClosedOnWeakSecret(t *testing.T) {
	drainCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_DRAIN_SECRET", "tooweak")
	if err := (DrainPlugin{}).Register(launchPluginContext()); err != nil {
		t.Fatal(err)
	}
	if registeredDrain(t) != nil {
		t.Fatal("provider registered with a weak secret")
	}
	if !strings.Contains(DrainLastSkipReason(), "rejected") {
		t.Fatalf("skip reason %q", DrainLastSkipReason())
	}
	if gateway.IsTokenRoute(DrainRoutePath) {
		t.Fatal("drain route token authable")
	}
}

// test_drain_provider.py TestRegister test_registers_with_strong_env_secret
func TestDrainRegisterRegistersWithStrongEnvSecret(t *testing.T) {
	drainCase(t, "")
	s := strongSecret()
	t.Setenv("ICEBATH_DASHBOARD_DRAIN_SECRET", s)
	if err := (DrainPlugin{}).Register(launchPluginContext()); err != nil {
		t.Fatal(err)
	}
	provider := registeredDrain(t)
	if provider == nil {
		t.Fatal("provider not registered")
	}
	if principal, err := provider.VerifyToken(context.Background(), s); principal == nil || err != nil {
		t.Fatalf("principal %v err %v", principal, err)
	}
	if DrainLastSkipReason() != "" {
		t.Fatalf("skip reason %q", DrainLastSkipReason())
	}
	if !gateway.IsTokenRoute(DrainRoutePath) {
		t.Fatal("drain route not token authable")
	}
}

// test_drain_provider.py TestRegister test_config_scope_applied
func TestDrainRegisterConfigScopeApplied(t *testing.T) {
	drainCase(t, "dashboard:\n  drain_auth:\n    scope: lifecycle\n")
	s := strongSecret()
	t.Setenv("ICEBATH_DASHBOARD_DRAIN_SECRET", s)
	if err := (DrainPlugin{}).Register(launchPluginContext()); err != nil {
		t.Fatal(err)
	}
	provider := registeredDrain(t)
	if provider == nil {
		t.Fatal("provider not registered")
	}
	principal, err := provider.VerifyToken(context.Background(), s)
	if err != nil || principal == nil || !slices.Equal(principal.Scopes, []string{"lifecycle"}) {
		t.Fatalf("principal %+v err %v", principal, err)
	}
}

// test_drain_provider.py TestRegister test_config_min_secret_chars_can_reject_otherwise_ok_secret
func TestDrainRegisterConfigMinSecretCharsCanRejectOtherwiseOkSecret(t *testing.T) {
	drainCase(t, "dashboard:\n  drain_auth:\n    min_secret_chars: 999\n")
	t.Setenv("ICEBATH_DASHBOARD_DRAIN_SECRET", strongSecret())
	if err := (DrainPlugin{}).Register(launchPluginContext()); err != nil {
		t.Fatal(err)
	}
	if registeredDrain(t) != nil {
		t.Fatal("provider registered under a higher minimum")
	}
	if !strings.Contains(DrainLastSkipReason(), "rejected") {
		t.Fatalf("skip reason %q", DrainLastSkipReason())
	}
}
