package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Name keyed providers in Python dict order, an upsert keeping the original position
type authProviderMap struct {
	names  []string
	byName map[string]DashboardAuthProvider
}

func newAuthProviderMap() *authProviderMap {
	return &authProviderMap{byName: map[string]DashboardAuthProvider{}}
}

func (m *authProviderMap) get(name string) (DashboardAuthProvider, bool) {
	p, ok := m.byName[name]
	return p, ok
}

func (m *authProviderMap) set(name string, p DashboardAuthProvider) {
	if _, ok := m.byName[name]; !ok {
		m.names = append(m.names, name)
	}
	m.byName[name] = p
}

func (m *authProviderMap) pop(name string) {
	if _, ok := m.byName[name]; ok {
		delete(m.byName, name)
		m.names = slices.DeleteFunc(m.names, func(n string) bool { return n == name })
	}
}

func (m *authProviderMap) values() []DashboardAuthProvider {
	out := make([]DashboardAuthProvider, 0, len(m.names))
	for _, n := range m.names {
		out = append(out, m.byName[n])
	}
	return out
}

// Process global providers plus the per home overlays a scoped registration writes
var (
	dashboardAuthMu       sync.Mutex
	dashboardAuthGlobal   = newAuthProviderMap()
	dashboardAuthOverlays = map[string]*authProviderMap{}
)

func dashboardAuthLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.dashboard_auth.registry")
}

// Global providers overlaid by the read scope providers, the active home when scope is empty
func mergedDashboardAuth(ctx context.Context, scope string) *authProviderMap {
	if scope == "" {
		scope = home.Dir(ctx)
	}
	merged := newAuthProviderMap()
	for _, n := range dashboardAuthGlobal.names {
		merged.set(n, dashboardAuthGlobal.byName[n])
	}
	if overlay, ok := dashboardAuthOverlays[home.Key(scope)]; ok {
		for _, n := range overlay.names {
			merged.set(n, overlay.byName[n])
		}
	}
	return merged
}

// Global map for the empty write scope, else that scope overlay created on demand
func targetDashboardAuth(scope string, create bool) *authProviderMap {
	if scope == "" {
		return dashboardAuthGlobal
	}
	if overlay, ok := dashboardAuthOverlays[scope]; ok {
		return overlay
	}
	overlay := newAuthProviderMap()
	if create {
		dashboardAuthOverlays[scope] = overlay
	}
	return overlay
}

func logRegisteredDashboardAuth(kind string, p DashboardAuthProvider) {
	dashboardAuthLog().Info(fmt.Sprintf("dashboard-auth: registered %s%s (%s)", kind, pyrepr.Repr(p.Name()), p.DisplayName()))
}

// Python identity of two provider values
func sameAuthProvider(a, b DashboardAuthProvider) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta.Comparable() {
		return a == b
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	switch ta.Kind() {
	case reflect.Map, reflect.Slice, reflect.Func, reflect.Pointer, reflect.Chan:
		return va.Pointer() == vb.Pointer()
	}
	return false
}

// Registers into the global map or a scope overlay, TypeError on protocol violation, ValueError on duplicates
func RegisterDashboardAuthProvider(ctx context.Context, p DashboardAuthProvider, scope string) error {
	if err := AssertDashboardAuthProtocol(p); err != nil {
		return err
	}
	dashboardAuthMu.Lock()
	scope = home.NormalizeScope(scope)
	target := targetDashboardAuth(scope, true)
	effective := target
	if scope != "" {
		effective = mergedDashboardAuth(ctx, scope)
	}
	if _, dup := effective.get(p.Name()); dup {
		dashboardAuthMu.Unlock()
		return &DashboardAuthValueError{Msg: "dashboard-auth provider already registered: " + pyrepr.Repr(p.Name())}
	}
	target.set(p.Name(), p)
	dashboardAuthMu.Unlock()
	logRegisteredDashboardAuth("provider ", p)
	return nil
}

// Registered provider for name in the read scope, nil when unknown
func GetDashboardAuthProvider(ctx context.Context, name, scope string) DashboardAuthProvider {
	dashboardAuthMu.Lock()
	defer dashboardAuthMu.Unlock()
	p, _ := mergedDashboardAuth(ctx, scope).get(name)
	return p
}

// Provider currently in the write scope slot for name, nil when the slot is empty
func SnapshotDashboardAuthRegistration(name, scope string) DashboardAuthProvider {
	dashboardAuthMu.Lock()
	defer dashboardAuthMu.Unlock()
	p, _ := targetDashboardAuth(home.NormalizeScope(scope), false).get(name)
	return p
}

// Puts previous back in the slot for name while current still holds it, dropping an emptied overlay
func RestoreDashboardAuthRegistration(name string, current, previous DashboardAuthProvider, scope string) bool {
	dashboardAuthMu.Lock()
	defer dashboardAuthMu.Unlock()
	scope = home.NormalizeScope(scope)
	target := targetDashboardAuth(scope, true)
	held, _ := target.get(name)
	if !sameAuthProvider(held, current) {
		return false
	}
	if previous == nil {
		target.pop(name)
	} else {
		target.set(name, previous)
	}
	if scope != "" && len(target.names) == 0 {
		delete(dashboardAuthOverlays, scope)
	}
	return true
}

// Every provider of the read scope in registration order
func ListDashboardAuthProviders(ctx context.Context, scope string) []DashboardAuthProvider {
	dashboardAuthMu.Lock()
	defer dashboardAuthMu.Unlock()
	return mergedDashboardAuth(ctx, scope).values()
}

// Providers the token auth seam may consult, in registration order
func ListTokenDashboardAuthProviders(ctx context.Context) []DashboardAuthProvider {
	return slices.DeleteFunc(ListDashboardAuthProviders(ctx, ""), func(p DashboardAuthProvider) bool { return !DashboardAuthSupportsToken(p) })
}

// Providers backing interactive cookie sessions, in registration order
func ListSessionDashboardAuthProviders(ctx context.Context) []DashboardAuthProvider {
	return slices.DeleteFunc(ListDashboardAuthProviders(ctx, ""), func(p DashboardAuthProvider) bool { return !DashboardAuthSupportsSession(p) })
}

// Session providers that offer a browser sign in flow, in registration order
func ListInteractiveDashboardAuthProviders(ctx context.Context) []DashboardAuthProvider {
	return slices.DeleteFunc(ListSessionDashboardAuthProviders(ctx), func(p DashboardAuthProvider) bool { return !DashboardAuthInteractiveLogin(p) })
}

// Upserts a host owned provider into the process global slot every profile scope shares
func RegisterGlobalDashboardAuthProvider(p DashboardAuthProvider) error {
	if err := AssertDashboardAuthProtocol(p); err != nil {
		return err
	}
	dashboardAuthMu.Lock()
	dashboardAuthGlobal.set(p.Name(), p)
	dashboardAuthMu.Unlock()
	logRegisteredDashboardAuth("global provider ", p)
	return nil
}

// Removes a global registration only while p is still the current provider for name
func UnregisterGlobalDashboardAuthProvider(name string, p DashboardAuthProvider) bool {
	dashboardAuthMu.Lock()
	defer dashboardAuthMu.Unlock()
	if held, ok := dashboardAuthGlobal.get(name); ok && sameAuthProvider(held, p) {
		dashboardAuthGlobal.pop(name)
		return true
	}
	return false
}

// Drops every global and scoped dashboard auth registration
func ClearDashboardAuthProviders() {
	dashboardAuthMu.Lock()
	defer dashboardAuthMu.Unlock()
	dashboardAuthGlobal = newAuthProviderMap()
	dashboardAuthOverlays = map[string]*authProviderMap{}
}

// Registry name of a dashboard auth provider object, false for anything that does not implement the protocol
func HostedDashboardAuthProviderName(obj any) (string, bool) {
	p, ok := adaptHostedDashboardAuthProvider(obj).(DashboardAuthProvider)
	if !ok {
		return "", false
	}
	return p.Name(), true
}

// Process global dashboard auth registry the plugin context registers launch scope providers into
type dashboardAuthRegistryService struct{}

func (dashboardAuthRegistryService) RegisterGlobal(provider any) (string, error) {
	provider = adaptHostedDashboardAuthProvider(provider)
	if err := AssertDashboardAuthProtocol(provider); err != nil {
		return "", err
	}
	p := provider.(DashboardAuthProvider)
	if err := RegisterGlobalDashboardAuthProvider(p); err != nil {
		return "", err
	}
	return p.DisplayName(), nil
}

func (dashboardAuthRegistryService) UnregisterGlobal(name string, provider any) {
	if p, ok := adaptHostedDashboardAuthProvider(provider).(DashboardAuthProvider); ok {
		UnregisterGlobalDashboardAuthProvider(name, p)
	}
}

// Registry service the plugin manager installs for register_dashboard_auth_provider
func DashboardAuthRegistryService() plugins.DashboardAuthRegistry {
	return dashboardAuthRegistryService{}
}

// Names of the providers backing interactive cookie sessions in the process home scope
func dashboardSessionProviderNames() []string {
	names := []string{}
	for _, p := range ListSessionDashboardAuthProviders(context.Background()) {
		names = append(names, p.Name())
	}
	return names
}
