package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Dashboard auth provider double with every protocol method, tagged to tell instances apart
type basicAuthProvider struct {
	name, display, tag string
}

func (p *basicAuthProvider) Name() string        { return p.name }
func (p *basicAuthProvider) DisplayName() string { return p.display }
func (p *basicAuthProvider) StartLogin(context.Context, string) (LoginStart, error) {
	return LoginStart{RedirectURL: "x", CookiePayload: map[string]string{}}, nil
}
func (p *basicAuthProvider) CompleteLogin(context.Context, string, string, string, string) (*DashboardSession, error) {
	return &DashboardSession{UserID: "u", Email: "e", DisplayName: "n", OrgID: "o", Provider: p.name, AccessToken: "a", RefreshToken: "r"}, nil
}
func (p *basicAuthProvider) VerifySession(context.Context, string) (*DashboardSession, error) {
	return nil, nil
}
func (p *basicAuthProvider) RefreshSession(context.Context, string) (*DashboardSession, error) {
	return nil, nil
}
func (p *basicAuthProvider) RevokeSession(context.Context, string) error { return nil }

func newBasic(tag string) *basicAuthProvider {
	return &basicAuthProvider{name: "basic", display: "Basic", tag: tag}
}

// Installs the gateway dashboard auth registry behind the plugin services and empties it around the test
func isolatedAuthRegistry(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ClearDashboardAuthProviders()
	plugins.InstallServices(&plugins.Services{
		DashboardAuth: DashboardAuthRegistryService(),
		HostObject: func(kind string, obj any) (string, bool) {
			if kind == "dashboard_auth_provider" {
				return HostedDashboardAuthProviderName(obj)
			}
			return "", false
		},
	})
	buf := &bytes.Buffer{}
	prevLogger := plugins.Logger
	plugins.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		plugins.Logger = prevLogger
		plugins.InstallServices(nil)
		ClearDashboardAuthProviders()
	})
	return buf
}

func realAuthCtx() (*plugins.Manager, *plugins.Context) {
	manager := plugins.NewManager(home.Key(home.Process()))
	return manager, plugins.NewPluginContext(&plugins.Manifest{Name: "basic", Version: "0.0.1", Kind: "backend"}, manager)
}

func registerAuth(t *testing.T, ctx *plugins.Context, provider any) *plugins.Registration {
	t.Helper()
	handle, err := ctx.RegisterDashboardAuthProvider(provider)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func liveBasic() DashboardAuthProvider {
	return GetDashboardAuthProvider(context.Background(), "basic", "")
}

func sessionProviderNames() []string {
	var out []string
	for _, p := range ListSessionDashboardAuthProviders(context.Background()) {
		out = append(out, p.Name())
	}
	return out
}

func TestPluginCtxSilentlyIgnoresNonProvider(t *testing.T) {
	logs := isolatedAuthRegistry(t)
	ctx := plugins.NewPluginContext(&plugins.Manifest{Name: "dashboard-auth-bad", Version: "0.0.1", Description: "stub"},
		plugins.NewManager(home.Key(home.Process())))
	if handle := registerAuth(t, ctx, "not a provider"); handle != nil {
		t.Fatal("non provider registered")
	}
	if GetDashboardAuthProvider(context.Background(), "stub", "") != nil {
		t.Fatal("registry not empty")
	}
	line := logs.String()
	if !strings.Contains(line, "dashboard-auth-bad") || !strings.Contains(line, "DashboardAuthProvider") || !strings.Contains(line, "level=WARN") {
		t.Fatalf("warning missing: %s", line)
	}
}

func TestAuthProviderRegistersGloballyNotInHomeOverlay(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("a"))
	if _, ok := dashboardAuthGlobal.get("basic"); !ok {
		t.Fatal("not in the global slot")
	}
	if overlay, ok := dashboardAuthOverlays[home.Key(home.Process())]; ok {
		if _, in := overlay.get("basic"); in {
			t.Fatal("registered in the home overlay")
		}
	}
	_ = manager
	if names := sessionProviderNames(); len(names) != 1 || names[0] != "basic" {
		t.Fatalf("session providers %v", names)
	}
}

func TestAuthProviderSurvivesPerHomeManagerUnload(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("a"))
	if liveBasic() == nil {
		t.Fatal("not registered")
	}
	manager.Unload(nil)
	if liveBasic() == nil {
		t.Fatal("auth provider was disposed by a per-home plugin-manager unload")
	}
	if names := sessionProviderNames(); len(names) != 1 || names[0] != "basic" {
		t.Fatalf("session providers %v", names)
	}
}

func TestAuthProviderKeptOutOfManagerTeardownOrder(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("a"))
	if order := manager.RegistrationOrder(); len(order) != 0 {
		t.Fatalf("enrolled in teardown order %v", order)
	}
	if owned := manager.OwnedRegistrations("basic"); len(owned) == 0 {
		t.Fatal("not attributed to the plugin")
	}
}

func TestAuthProviderReRegisterRotatesInPlace(t *testing.T) {
	isolatedAuthRegistry(t)
	_, ctx := realAuthCtx()
	old, fresh := newBasic("old"), newBasic("new")
	stale := registerAuth(t, ctx, old)
	registerAuth(t, ctx, fresh)
	if liveBasic() != fresh {
		t.Fatal("upsert did not rotate")
	}
	if err := stale.Dispose(); err != nil {
		t.Fatal(err)
	}
	if liveBasic() != fresh {
		t.Fatal("stale handle cleared the live provider")
	}
}

func TestProfileManagerCannotReplaceLaunchHomeAuthProvider(t *testing.T) {
	isolatedAuthRegistry(t)
	_, ctx := realAuthCtx()
	launch := newBasic("launch-home")
	registerAuth(t, ctx, launch)
	profileScope := home.Key(filepath.Join(home.Key(home.Process()), "profiles", "bot"))
	profileManager := plugins.NewManager(profileScope)
	profileCtx := plugins.NewPluginContext(&plugins.Manifest{Name: "basic", Version: "0.0.1", Kind: "backend"}, profileManager)
	if registration := registerAuth(t, profileCtx, newBasic("profile")); registration != nil {
		t.Fatal("profile scope registered a dashboard auth provider")
	}
	if liveBasic() != launch {
		t.Fatal("profile replaced the launch provider")
	}
	if owned := profileManager.OwnedRegistrations("basic"); len(owned) != 0 {
		t.Fatalf("profile ledger %v", owned)
	}
}

func TestProfileDirectoryCanBeTheProcessLaunchHome(t *testing.T) {
	isolatedAuthRegistry(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profiles", "bot"))
	manager, ctx := realAuthCtx()
	provider := newBasic("profile-launch-home")
	registration := registerAuth(t, ctx, provider)
	if manager == nil || home.Key(home.Process()) != home.Key(home.Dir(context.Background())) {
		t.Fatal("manager scope is not the process launch home")
	}
	if registration == nil || liveBasic() != provider {
		t.Fatal("launch profile could not register")
	}
}

func TestTargetedUnloadDisposesPersistentAuthProvider(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("a"))
	if liveBasic() == nil {
		t.Fatal("not registered")
	}
	if !manager.Unload("basic") {
		t.Fatal("targeted unload found nothing")
	}
	if liveBasic() != nil {
		t.Fatal("disabled auth plugin's provider stayed registered process-wide")
	}
	if names := sessionProviderNames(); len(names) != 0 {
		t.Fatalf("session providers %v", names)
	}
}

func TestRediscoveryEvictsProviderWhenPluginGone(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("a"))
	manager.Unload(nil)
	if liveBasic() == nil {
		t.Fatal("unload all disposed the provider")
	}
	manager.EvictStalePersistentRegistrations()
	if liveBasic() != nil {
		t.Fatal("provider survived a re-discovery its plugin was dropped from")
	}
}

func TestRediscoveryKeepsProviderWhenPluginReturns(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("old"))
	manager.Unload(nil)
	fresh := newBasic("new")
	registerAuth(t, ctx, fresh)
	manager.EvictStalePersistentRegistrations()
	if liveBasic() != fresh {
		t.Fatal("re-registered provider not live")
	}
	if parked := manager.PersistentCarryover(); len(parked) != 0 {
		t.Fatalf("carryover not drained %v", parked)
	}
}

func TestRediscoverySameObjectReregistrationSurvivesEviction(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	provider := newBasic("same")
	registerAuth(t, ctx, provider)
	manager.Unload(nil)
	registerAuth(t, ctx, provider)
	manager.EvictStalePersistentRegistrations()
	if liveBasic() != provider {
		t.Fatal("same object re-registration was evicted")
	}
}

func TestPersistentDisposeIsIdempotentAfterTargetedUnload(t *testing.T) {
	isolatedAuthRegistry(t)
	manager, ctx := realAuthCtx()
	registerAuth(t, ctx, newBasic("a"))
	manager.Unload("basic")
	if liveBasic() != nil {
		t.Fatal("targeted unload kept the provider")
	}
	manager.Unload(nil)
	if parked := manager.PersistentCarryover(); len(parked) != 0 {
		t.Fatalf("unload all parked a forgotten handle %v", parked)
	}
	manager.EvictStalePersistentRegistrations()
	if liveBasic() != nil {
		t.Fatal("eviction resurrected the provider")
	}
}

type nameless struct{ basicAuthProvider }

func TestAssertDashboardAuthProtocolAndScopedRegistry(t *testing.T) {
	isolatedAuthRegistry(t)
	if err := AssertDashboardAuthProtocol(&nameless{}); err == nil || err.Error() != "nameless missing or empty attribute: 'name'" {
		t.Fatalf("nameless %v", err)
	}
	if err := AssertDashboardAuthProtocol("x"); err == nil || err.Error() != "str missing or empty attribute: 'name'" {
		t.Fatalf("str %v", err)
	}
	ctx := context.Background()
	scope := filepath.Join(t.TempDir(), "profiles", "p")
	scoped := newBasic("scoped")
	if err := RegisterDashboardAuthProvider(ctx, scoped, scope); err != nil {
		t.Fatal(err)
	}
	if err := RegisterDashboardAuthProvider(ctx, newBasic("dup"), scope); err == nil || err.Error() != "dashboard-auth provider already registered: 'basic'" {
		t.Fatalf("duplicate %v", err)
	}
	if GetDashboardAuthProvider(ctx, "basic", scope) != scoped || liveBasic() != nil {
		t.Fatal("scoped registration leaked into the active home")
	}
	if SnapshotDashboardAuthRegistration("basic", scope) != scoped {
		t.Fatal("snapshot missed the scoped slot")
	}
	if RestoreDashboardAuthRegistration("basic", newBasic("other"), nil, scope) {
		t.Fatal("restore ran for a provider that is not current")
	}
	if !RestoreDashboardAuthRegistration("basic", scoped, nil, scope) || GetDashboardAuthProvider(ctx, "basic", scope) != nil {
		t.Fatal("restore did not clear the scoped slot")
	}
	if _, ok := dashboardAuthOverlays[home.NormalizeScope(scope)]; ok {
		t.Fatal("emptied overlay kept")
	}
	if _, err := DashboardAuthVerifyToken(ctx, scoped, "t"); err == nil ||
		err.Error() != "basicAuthProvider does not support token auth (set supports_token = True and override verify_token)" {
		t.Fatalf("verify token default %v", err)
	}
	if DashboardAuthSupportsToken(scoped) || !DashboardAuthSupportsSession(scoped) || !DashboardAuthInteractiveLogin(scoped) || DashboardAuthSupportsPassword(scoped) {
		t.Fatal("capability defaults")
	}
}
